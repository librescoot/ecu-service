package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/go-redis/redis/v8"
)

type DiagFault int

const (
	DiagFaultNone DiagFault = iota
	DiagFaultBatteryOverVoltage
	DiagFaultBatteryUnderVoltage
	DiagFaultMotorShortCircuit
	DiagFaultMotorStalled
	DiagFaultHallSensorAbnormal
	DiagFaultMosfetCheckError
	DiagFaultMotorOpenCircuit
	DiagFaultReserved8
	DiagFaultReserved9
	DiagFaultSelfCheckError
	DiagFaultOverTemperature
	DiagFaultThrottleAbnormal
	DiagFaultMotorTemperatureProtection
	DiagFaultThrottleActiveAtPowerUp
	DiagFaultBrakingActive
	DiagFaultInternal15VAbnormal
	DiagFaultNum
)

var faultDescriptions = map[DiagFault]string{
	DiagFaultBatteryOverVoltage:         "Battery over-voltage",
	DiagFaultBatteryUnderVoltage:        "Battery under-voltage",
	DiagFaultMotorShortCircuit:          "Motor short-circuit",
	DiagFaultMotorStalled:               "Motor stalled",
	DiagFaultHallSensorAbnormal:         "Hall sensor abnormal",
	DiagFaultMosfetCheckError:           "MOSFET check error",
	DiagFaultMotorOpenCircuit:           "Motor open-circuit",
	DiagFaultReserved8:                  "Reserved",
	DiagFaultReserved9:                  "Reserved",
	DiagFaultSelfCheckError:             "Power-on self-check error",
	DiagFaultOverTemperature:            "Over-temperature",
	DiagFaultThrottleAbnormal:           "Throttle abnormal",
	DiagFaultMotorTemperatureProtection: "Motor temperature protection",
	DiagFaultThrottleActiveAtPowerUp:    "Throttle active at power up",
	DiagFaultBrakingActive:              "Braking active at power up",
	DiagFaultInternal15VAbnormal:        "Internal 15V abnormal",
}

type Diag struct {
	log    *log.Logger
	redis  *redis.Client
	mu     sync.RWMutex
	faults map[DiagFault]bool
	ctx    context.Context
}

func NewDiag(logger *log.Logger, redis *redis.Client) *Diag {
	return &Diag{
		log:    logger,
		redis:  redis,
		faults: make(map[DiagFault]bool),
		ctx:    context.Background(),
	}
}

func (d *Diag) Destroy() {}

func (d *Diag) SetFaultPresence(fault DiagFault, present bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.faults[fault] == present {
		return
	}

	d.faults[fault] = present

	// Prepare diagnostic data
	data := map[string]interface{}{
		"fault":       int(fault),
		"description": faultDescriptions[fault],
		"present":     present,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		d.log.Printf("Failed to marshal diagnostic data: %v", err)
		return
	}

	// Publish to Redis
	channel := fmt.Sprintf("engine-ecu:diag:fault:%d", fault)
	if err := d.redis.Publish(d.ctx, channel, jsonData).Err(); err != nil {
		d.log.Printf("Failed to publish diagnostic data: %v", err)
	}
}

func (d *Diag) SetEngineFaultPresence(fault DiagFault) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Clear all faults
	for f := range d.faults {
		d.faults[f] = false
	}

	// Set only the specified fault
	if fault != DiagFaultNone {
		d.faults[fault] = true
	}

	// Publish all fault states
	for f := DiagFault(1); f < DiagFaultNum; f++ {
		data := map[string]interface{}{
			"fault":       int(f),
			"description": faultDescriptions[f],
			"present":     d.faults[f],
		}

		jsonData, err := json.Marshal(data)
		if err != nil {
			d.log.Printf("Failed to marshal diagnostic data: %v", err)
			continue
		}

		channel := fmt.Sprintf("engine-ecu:diag:fault:%d", f)
		if err := d.redis.Publish(d.ctx, channel, jsonData).Err(); err != nil {
			d.log.Printf("Failed to publish diagnostic data: %v", err)
		}
	}
}
