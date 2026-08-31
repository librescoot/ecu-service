package main

import (
	"context"
	"fmt"
	"sync"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/redis/go-redis/v9"
)

const (
	ecuHashKey     = "engine-ecu"
	faultSetKey    = "engine-ecu:fault"
	faultStreamKey = "events:faults"
	faultStreamMax = 1000
	// ecuChannel carries every hash change notification. The convention is
	// channel = hash name, message = field name. "engine-ecu throttle" is how
	// that pair gets written down in docs, not a channel of its own: published
	// as a literal channel name it reaches nobody, since subscribers listen on
	// "engine-ecu".
	ecuChannel = "engine-ecu"
)

type Status struct {
	Voltage              int
	Current              int
	RPM                  uint16
	Speed                uint16
	RawSpeed             uint16
	ThrottleOn           bool
	BrakeOn              bool
	Power                int64
	EnergyConsumed       uint64
	EnergyRecovered      uint64
	Temperature          int8
	FaultCode            uint32
	FaultDesc            string
	Odometer             uint32
	OdometerValid        bool
	KersActive           bool
	BoostEnabled         bool
	KersReasonOff        string
	AcceptedRegenVoltage int    // mV, EBS regen voltage cap the ECU accepted
	AcceptedRegenCurrent int    // mA, EBS regen current limit the ECU accepted
	RegenAvailable       bool   // derived: can regen happen right now
	RegenReason          string // derived: none/cold/hot/off/standstill/full
	RegenExpected        int    // derived: expected regen current envelope, mA
	Gear                 uint8
	FirmwareVersion      uint32
	WarrantyDate         uint32

	// Status4 bits reported by the ECU (paired enable/disable decode).
	ECUStatusEnabled bool
	BoostActive      bool // same signal as BoostEnabled, published under its own key for parity with the ECU/gear-mode status trio
	GearModeEnabled  bool

	// Per-gear current/torque ratios (0-100 %), from the Gear frame (0x7E4).
	HighGearCurrent uint8
	MidGearCurrent  uint8
	LowGearCurrent  uint8
	HighGearTorque  uint8
	MidGearTorque   uint8
	LowGearTorque   uint8

	// Decoded software-version / motor spec components, from Status5 (0x7E8).
	MotorRatedPowerKW uint8
	MotorMaxSpeedKMH  uint8
	SWBaseVersion     string
	SWAppVersion      string
}

// ECUConfigStatus carries the ECU configuration values broadcast at boot or
// in response to a status request (0x4EF). Values are 0 until the ECU
// reports them.
type ECUConfigStatus struct {
	OverVoltageThresholdMV  uint32
	UnderVoltageThresholdMV uint32
	SpeedLimitRatio         uint8
	WheelCircumferenceCM    uint8
	MaxPhaseCurrentMA       uint32
	StartupPhaseCurrentMA   uint32
}

type IPCTx struct {
	raw    *redis.Client // underlying go-redis client for pipeline writes
	client *ipc.Client
	ctx    context.Context
	log    *Logger

	// mu guards last/hasLast/lastFault, which SendStatus (CAN goroutine) and
	// SetFault/ReportFault (watchdog and diagnostics goroutines) all touch.
	mu sync.Mutex
	// last is the previously sent status; SendStatus only HSETs changed fields
	// to avoid redundant Redis writes on every CAN frame.
	last    Status
	hasLast bool
	// lastFault is the fault currently reported into faultSetKey. A clear has
	// to name the code it clears, so ReportFault needs to remember it.
	lastFault Fault
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

func enabledDisabled(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
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
	if s.OdometerValid && (first || !l.OdometerValid || s.Odometer != l.Odometer) {
		fields["odometer"] = s.Odometer
	}
	add("kers", onOff(s.KersActive), s.KersActive != l.KersActive)
	add("boost", onOff(s.BoostEnabled), s.BoostEnabled != l.BoostEnabled)
	// kers-reason-off is owned by SetKERSReasonOff, which stores the reason
	// before notifying. A generic CAN snapshot must never roll it backward.
	add("kers-accepted-voltage", s.AcceptedRegenVoltage, s.AcceptedRegenVoltage != l.AcceptedRegenVoltage)
	add("kers-accepted-current", s.AcceptedRegenCurrent, s.AcceptedRegenCurrent != l.AcceptedRegenCurrent)
	add("regen-available", onOff(s.RegenAvailable), s.RegenAvailable != l.RegenAvailable)
	add("regen-reason", s.RegenReason, s.RegenReason != l.RegenReason)
	add("regen-expected", s.RegenExpected, s.RegenExpected != l.RegenExpected)
	add("gear", s.Gear, s.Gear != l.Gear)
	add("ecu-status", enabledDisabled(s.ECUStatusEnabled), s.ECUStatusEnabled != l.ECUStatusEnabled)
	add("boost-status", enabledDisabled(s.BoostActive), s.BoostActive != l.BoostActive)
	add("gear-mode", enabledDisabled(s.GearModeEnabled), s.GearModeEnabled != l.GearModeEnabled)
	if s.FirmwareVersion != 0 && (first || s.FirmwareVersion != l.FirmwareVersion) {
		fields["fw-version"] = fmt.Sprintf("%08X", s.FirmwareVersion)
		fields["motor:rated-power-kw"] = s.MotorRatedPowerKW
		fields["motor:max-speed-kmh"] = s.MotorMaxSpeedKMH
		fields["fw:base-version"] = s.SWBaseVersion
		fields["fw:app-version"] = s.SWAppVersion
	}
	if s.WarrantyDate != 0 && (first || s.WarrantyDate != l.WarrantyDate) {
		fields["warranty-date"] = fmt.Sprintf("%08X", s.WarrantyDate)
	}
	// Per-gear ratios are only meaningful once seen. Publishing zeros at
	// boot would overwrite previously-cached values; skip the whole block
	// when nothing has been reported yet.
	if s.HighGearCurrent != 0 || s.MidGearCurrent != 0 || s.LowGearCurrent != 0 ||
		s.HighGearTorque != 0 || s.MidGearTorque != 0 || s.LowGearTorque != 0 {
		fields["gear:high-current-ratio"] = s.HighGearCurrent
		fields["gear:mid-current-ratio"] = s.MidGearCurrent
		fields["gear:low-current-ratio"] = s.LowGearCurrent
		fields["gear:high-torque-ratio"] = s.HighGearTorque
		fields["gear:mid-torque-ratio"] = s.MidGearTorque
		fields["gear:low-torque-ratio"] = s.LowGearTorque
	}

	if !s.OdometerValid && l.OdometerValid {
		s.Odometer = l.Odometer
		s.OdometerValid = true
	}
	if len(fields) != 0 {
		if _, err := tx.raw.HSet(tx.ctx, ecuHashKey, fields).Result(); err != nil {
			return err
		}
	}

	// Only suppress unchanged fields after Redis has confirmed the update.
	// Otherwise an identical later Status3 must be able to retry the write.
	// Preserve the independently synchronized KERS reason across this snapshot.
	s.KersReasonOff = tx.last.KersReasonOff
	tx.last = s
	tx.hasLast = true
	return nil
}

// SendECUConfig publishes the ECU's reported configuration values. Each group
// of fields corresponds to a single CAN frame (or pair of frames that arrive
// together). When a group has not been seen — its canonical value is still
// zero — the fields are HDEL'd so callers can distinguish "not reported" from
// a real zero reading. Without this, a field populated only by an infrequent
// 0x4EF settings burst would otherwise show a misleading "0" for as long as
// the bus stays busy enough that the comm-lost watcher never triggers 0x4EF.
func (tx *IPCTx) SendECUConfig(c ECUConfigStatus) error {
	pipe := tx.raw.Pipeline()

	// 0x7E9 + 0x7EA: over- and under-voltage thresholds arrive together in
	// the 0x4EF settings burst.
	voltageKeys := []string{"config:ov-threshold-mv", "config:uv-threshold-mv"}
	if c.OverVoltageThresholdMV != 0 {
		pipe.HSet(tx.ctx, ecuHashKey, map[string]any{
			"config:ov-threshold-mv": c.OverVoltageThresholdMV,
			"config:uv-threshold-mv": c.UnderVoltageThresholdMV,
		})
	} else {
		pipe.HDel(tx.ctx, ecuHashKey, voltageKeys...)
	}

	// 0x7EB: speed limit ratio
	if c.SpeedLimitRatio != 0 {
		pipe.HSet(tx.ctx, ecuHashKey, "config:speed-limit-ratio", c.SpeedLimitRatio)
	} else {
		pipe.HDel(tx.ctx, ecuHashKey, "config:speed-limit-ratio")
	}

	// 0x7EC: wheel circumference
	if c.WheelCircumferenceCM != 0 {
		pipe.HSet(tx.ctx, ecuHashKey, "config:wheel-circumference-cm", c.WheelCircumferenceCM)
	} else {
		pipe.HDel(tx.ctx, ecuHashKey, "config:wheel-circumference-cm")
	}

	// 0x7EE + 0x7EF: max and startup phase currents
	phaseKeys := []string{"config:max-phase-current-ma", "config:startup-phase-current-ma"}
	if c.MaxPhaseCurrentMA != 0 {
		pipe.HSet(tx.ctx, ecuHashKey, map[string]any{
			"config:max-phase-current-ma":     c.MaxPhaseCurrentMA,
			"config:startup-phase-current-ma": c.StartupPhaseCurrentMA,
		})
	} else {
		pipe.HDel(tx.ctx, ecuHashKey, phaseKeys...)
	}

	_, err := pipe.Exec(tx.ctx)
	return err
}

// PublishThrottle notifies subscribers that the throttle state changed.
func (tx *IPCTx) PublishThrottle() error {
	_, err := tx.client.Publish(ecuChannel, "throttle")
	return err
}

func (tx *IPCTx) SetOdometer(odometer uint32) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if _, err := tx.raw.HSet(tx.ctx, ecuHashKey, "odometer", odometer).Result(); err != nil {
		return err
	}
	tx.last.Odometer = odometer
	tx.last.OdometerValid = true
	return nil
}

func (tx *IPCTx) ClearOdometer() (bool, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	deleted, err := tx.raw.HDel(tx.ctx, ecuHashKey, "odometer").Result()
	if err != nil {
		return false, err
	}
	tx.last.Odometer = 0
	tx.last.OdometerValid = false
	return deleted != 0, nil
}

// PublishOdometer notifies subscribers that the odometer changed.
func (tx *IPCTx) PublishOdometer() error {
	_, err := tx.client.Publish(ecuChannel, "odometer", ipc.Sync())
	return err
}

// PublishKERS notifies subscribers that KERS enable state changed.
func (tx *IPCTx) PublishKERS() error {
	_, err := tx.client.Publish(ecuChannel, "kers")
	return err
}

// SetKERSReasonOff stores the reason before synchronously notifying subscribers.
// The status cache advances only after both Redis operations succeed.
func (tx *IPCTx) SetKERSReasonOff(reason KERSReason) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	wireReason := string(reason)
	if reason == KERSReasonUnknown {
		// Keep the established public enum stable. Internally unknown remains a
		// fail-safe inhibit; externally the general regen reason reports "off".
		wireReason = string(KERSReasonNone)
	}
	if _, err := tx.raw.HSet(tx.ctx, ecuHashKey, "kers-reason-off", wireReason).Result(); err != nil {
		return err
	}
	if _, err := tx.client.Publish(ecuChannel, "kers-reason-off", ipc.Sync()); err != nil {
		return err
	}
	tx.last.KersReasonOff = wireReason
	return nil
}

// PublishKERSReasonOff is retained for callers that only need notification.
func (tx *IPCTx) PublishKERSReasonOff() error {
	_, err := tx.client.Publish(ecuChannel, "kers-reason-off", ipc.Sync())
	return err
}

// PublishRegen notifies subscribers that the derived regen availability or
// reason changed.
func (tx *IPCTx) PublishRegen() error {
	_, err := tx.client.Publish(ecuChannel, "regen-available")
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
// and notifies subscribers.
//
// The stream pairs a raise (positive code) with a clear (the same code,
// negated), so a clear has to name the fault it clears. A bare code 0 breaks
// that pairing: consumers key on the absolute code and drop it, and the clear
// never lands in fault history.
func (tx *IPCTx) ReportFault(fault Fault, cfg FaultConfig) error {
	tx.mu.Lock()
	prev := tx.lastFault
	tx.lastFault = fault
	tx.mu.Unlock()

	// Both an A -> B transition and an A -> none clear retire A.
	cleared := prev != FaultNone && prev != fault

	pipe := tx.raw.Pipeline()

	if fault == FaultNone {
		// Del rather than SRem: it also sweeps whatever a previous process
		// left in the set.
		pipe.Del(tx.ctx, faultSetKey)
	} else {
		if cleared {
			pipe.SRem(tx.ctx, faultSetKey, uint32(prev))
		}
		pipe.SAdd(tx.ctx, faultSetKey, uint32(fault))
	}

	if cleared {
		pipe.XAdd(tx.ctx, &redis.XAddArgs{
			Stream: faultStreamKey,
			MaxLen: faultStreamMax,
			// int64 because an unrecognised code is reported under its own raw
			// uint32, which would wrap when negated as an int32.
			Values: map[string]any{"group": "engine-ecu", "code": -int64(prev)},
		})
	}
	if fault != FaultNone {
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
