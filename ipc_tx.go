package main

import (
	"context"
	"fmt"
	"sync"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/redis/go-redis/v9"
)

const (
	ecuHashKey      = "engine-ecu"
	faultSetKey     = "engine-ecu:fault"
	faultStreamKey  = "events:faults"
	faultStreamMax  = 1000
	ecuChannel      = "engine-ecu"
	throttleChannel = "engine-ecu throttle"
	odometerChannel = "engine-ecu odometer"
	kersChannel     = "engine-ecu kers"
	kersReasonChan  = "engine-ecu kers-reason-off"
	kersAppliedChan = "engine-ecu kers-applied-current"
)

type Status struct {
	Voltage             int
	Current             int
	RPM                 uint16
	Speed               uint16
	RawSpeed            uint16
	ThrottleOn          bool
	BrakeOn             bool
	Power               int
	EnergyConsumed      uint64
	EnergyRecovered     uint64
	Temperature         int8
	FaultCode           uint32
	FaultDesc           string
	Odometer            uint32
	KersActive          bool
	BoostEnabled        bool
	KersReasonOff       string
	AppliedRegenVoltage int // mV, EBS regen the ECU reports applying
	AppliedRegenCurrent int // mA, EBS regen the ECU reports applying
	Gear                uint8
	FirmwareVersion     uint32
	WarrantyDate        uint32
}

type IPCTx struct {
	raw    *redis.Client // underlying go-redis client for pipeline writes
	client *ipc.Client
	ctx    context.Context
	log    *Logger

	// mu guards last/hasLast, which SendStatus (CAN goroutine) and SetFault
	// (watchdog goroutine) both touch.
	mu sync.Mutex
	// last is the previously sent status; SendStatus only HSETs changed fields
	// to avoid redundant Redis writes on every CAN frame.
	last    Status
	hasLast bool
}

func newIPCTx(ctx context.Context, client *ipc.Client, log *Logger) *IPCTx {
	return &IPCTx{raw: client.Raw(), client: client, ctx: ctx, log: log}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// SendStatus writes engine-ecu hash fields, but only those whose value changed
// since the previous call. Slow-moving fields (temperature, fault, odometer,
// gear, firmware) are skipped on most frames; if nothing changed the call is a
// no-op. The first call after start writes everything.
func (tx *IPCTx) SendStatus(s Status) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	first := !tx.hasLast
	l := tx.last
	fields := make(map[string]any, 18)

	add := func(key string, val any, changed bool) {
		if first || changed {
			fields[key] = val
		}
	}
	add("motor:voltage", s.Voltage, s.Voltage != l.Voltage)
	add("motor:current", s.Current, s.Current != l.Current)
	add("rpm", s.RPM, s.RPM != l.RPM)
	add("speed", s.Speed, s.Speed != l.Speed)
	add("raw-speed", s.RawSpeed, s.RawSpeed != l.RawSpeed)
	add("throttle", onOff(s.ThrottleOn), s.ThrottleOn != l.ThrottleOn)
	add("brake", onOff(s.BrakeOn), s.BrakeOn != l.BrakeOn)
	add("power", s.Power, s.Power != l.Power)
	add("energy:consumed", s.EnergyConsumed, s.EnergyConsumed != l.EnergyConsumed)
	add("energy:recovered", s.EnergyRecovered, s.EnergyRecovered != l.EnergyRecovered)
	add("temperature", s.Temperature, s.Temperature != l.Temperature)
	add("fault:code", s.FaultCode, s.FaultCode != l.FaultCode)
	add("fault:description", s.FaultDesc, s.FaultDesc != l.FaultDesc)
	add("odometer", s.Odometer, s.Odometer != l.Odometer)
	add("kers", onOff(s.KersActive), s.KersActive != l.KersActive)
	add("boost", onOff(s.BoostEnabled), s.BoostEnabled != l.BoostEnabled)
	add("kers-reason-off", s.KersReasonOff, s.KersReasonOff != l.KersReasonOff)
	add("kers-applied-voltage", s.AppliedRegenVoltage, s.AppliedRegenVoltage != l.AppliedRegenVoltage)
	add("kers-applied-current", s.AppliedRegenCurrent, s.AppliedRegenCurrent != l.AppliedRegenCurrent)
	add("gear", s.Gear, s.Gear != l.Gear)
	if s.FirmwareVersion != 0 && (first || s.FirmwareVersion != l.FirmwareVersion) {
		fields["fw-version"] = fmt.Sprintf("%08X", s.FirmwareVersion)
	}
	if s.WarrantyDate != 0 && (first || s.WarrantyDate != l.WarrantyDate) {
		fields["warranty-date"] = fmt.Sprintf("%08X", s.WarrantyDate)
	}

	tx.last = s
	tx.hasLast = true

	if len(fields) == 0 {
		return nil
	}

	_, err := tx.raw.HSet(tx.ctx, ecuHashKey, fields).Result()
	return err
}

// PublishThrottle notifies subscribers that the throttle state changed.
func (tx *IPCTx) PublishThrottle() error {
	_, err := tx.client.Publish(throttleChannel, "")
	return err
}

// PublishOdometer notifies subscribers that the odometer changed.
func (tx *IPCTx) PublishOdometer() error {
	_, err := tx.client.Publish(odometerChannel, "")
	return err
}

// PublishKERS notifies subscribers that KERS enable state changed.
func (tx *IPCTx) PublishKERS() error {
	_, err := tx.client.Publish(kersChannel, "")
	return err
}

// PublishKERSReasonOff notifies subscribers that the KERS-off reason changed.
func (tx *IPCTx) PublishKERSReasonOff() error {
	_, err := tx.client.Publish(kersReasonChan, "")
	return err
}

// PublishKERSApplied notifies subscribers that the ECU's applied regen
// voltage/current changed.
func (tx *IPCTx) PublishKERSApplied() error {
	_, err := tx.client.Publish(kersAppliedChan, "")
	return err
}

// SetFault overwrites the engine-ecu hash fault fields directly. The comm-lost
// watchdog uses this to raise/clear E20 in the hash while no CAN frames are
// arriving; tx.last is updated so the next SendStatus stays consistent.
func (tx *IPCTx) SetFault(code uint32, desc string) error {
	tx.mu.Lock()
	tx.last.FaultCode = code
	tx.last.FaultDesc = desc
	tx.mu.Unlock()

	_, err := tx.raw.HSet(tx.ctx, ecuHashKey, map[string]any{
		"fault:code":        code,
		"fault:description": desc,
	}).Result()
	return err
}

// ReportFault writes fault presence or absence to the fault set, event stream,
// and notifies subscribers. An FaultNone fault clears the set.
func (tx *IPCTx) ReportFault(fault Fault, cfg FaultConfig) error {
	pipe := tx.raw.Pipeline()

	if fault == FaultNone {
		pipe.Del(tx.ctx, faultSetKey)
		pipe.XAdd(tx.ctx, &redis.XAddArgs{
			Stream: faultStreamKey,
			MaxLen: faultStreamMax,
			Values: map[string]any{"group": "engine-ecu", "code": 0},
		})
	} else {
		pipe.SAdd(tx.ctx, faultSetKey, uint32(fault))
		pipe.XAdd(tx.ctx, &redis.XAddArgs{
			Stream: faultStreamKey,
			MaxLen: faultStreamMax,
			Values: map[string]any{
				"group":       "engine-ecu",
				"code":        uint32(fault),
				"description": cfg.Description,
				"severity":    cfg.Severity,
			},
		})
	}
	pipe.Publish(tx.ctx, ecuChannel, "fault")

	_, err := pipe.Exec(tx.ctx)
	return err
}
