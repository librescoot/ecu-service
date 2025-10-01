package main

import (
	"context"
	"log"
	"sync"

	"ecu-service/ecu"

	"github.com/go-redis/redis/v8"
)

const (
	diagGroupName           = "engine-ecu"
	diagFaultSetKey         = "engine-ecu:fault"
	diagEventStream         = "events:faults"
	diagEventStreamMaxLen   = 1000
	diagNotificationChannel = "engine-ecu"
)

type Diag struct {
	log          *log.Logger
	redis        *redis.Client
	mu           sync.RWMutex
	faultStates  map[ecu.ECUFault]bool
	ctx          context.Context
}

func NewDiag(logger *log.Logger, redis *redis.Client) *Diag {
	return &Diag{
		log:         logger,
		redis:       redis,
		faultStates: make(map[ecu.ECUFault]bool),
		ctx:         context.Background(),
	}
}

func (d *Diag) Destroy() {}

func (d *Diag) SetFaultPresence(fault ecu.ECUFault, present bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if fault == ecu.FaultNone {
		return
	}

	wasPresent := d.faultStates[fault]
	if wasPresent == present {
		return
	}

	d.faultStates[fault] = present

	config, ok := ecu.GetFaultConfig(fault)
	if !ok {
		d.log.Printf("Unknown fault code: %d", fault)
		return
	}

	if present {
		d.log.Printf("Fault set: code=%d, description=%s", fault, config.Description)
		d.reportFaultPresent(fault, config)
	} else {
		d.log.Printf("Fault cleared: code=%d, description=%s", fault, config.Description)
		d.reportFaultAbsent(fault)
	}
}

func (d *Diag) SetFaults(faults map[ecu.ECUFault]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for fault := ecu.ECUFault(1); fault <= ecu.FaultInternal15vAbnormal; fault++ {
		newPresent := faults[fault]
		wasPresent := d.faultStates[fault]

		if newPresent == wasPresent {
			continue
		}

		d.faultStates[fault] = newPresent

		config, ok := ecu.GetFaultConfig(fault)
		if !ok {
			continue
		}

		if newPresent {
			d.log.Printf("Fault set: code=%d, description=%s", fault, config.Description)
			d.reportFaultPresent(fault, config)
		} else {
			d.log.Printf("Fault cleared: code=%d, description=%s", fault, config.Description)
			d.reportFaultAbsent(fault)
		}
	}
}

func (d *Diag) reportFaultPresent(fault ecu.ECUFault, config ecu.FaultConfig) {
	pipe := d.redis.Pipeline()

	pipe.SAdd(d.ctx, diagFaultSetKey, uint32(fault))

	pipe.XAdd(d.ctx, &redis.XAddArgs{
		Stream: diagEventStream,
		MaxLen: diagEventStreamMaxLen,
		Values: map[string]interface{}{
			"group":       diagGroupName,
			"code":        uint32(fault),
			"description": config.Description,
		},
	})

	pipe.Publish(d.ctx, diagNotificationChannel, "fault")

	if _, err := pipe.Exec(d.ctx); err != nil {
		d.log.Printf("Failed to report fault present: %v", err)
	}
}

func (d *Diag) reportFaultAbsent(fault ecu.ECUFault) {
	pipe := d.redis.Pipeline()

	pipe.SRem(d.ctx, diagFaultSetKey, uint32(fault))

	pipe.XAdd(d.ctx, &redis.XAddArgs{
		Stream: diagEventStream,
		MaxLen: diagEventStreamMaxLen,
		Values: map[string]interface{}{
			"group": diagGroupName,
			"code":  -int32(fault),
		},
	})

	pipe.Publish(d.ctx, diagNotificationChannel, "fault")

	if _, err := pipe.Exec(d.ctx); err != nil {
		d.log.Printf("Failed to report fault absent: %v", err)
	}
}
