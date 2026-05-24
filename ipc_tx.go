package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-redis/redis/v8"
)

type IPCTx struct {
	log   *LeveledLogger
	redis *redis.Client
	mu    sync.Mutex
	ctx   context.Context

	throttleKnown  bool // whether lastThrottleOn has been set yet
	lastThrottleOn bool // last published throttle state (guarded by mu)
}

func NewIPCTx(logger *LeveledLogger, redis *redis.Client) *IPCTx {
	return &IPCTx{
		log:   logger,
		redis: redis,
		ctx:   context.Background(),
	}
}

func (tx *IPCTx) Destroy() {}

func (tx *IPCTx) SendStatus1(data RedisStatus1) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	pipe := tx.redis.Pipeline()

	pipe.HSet(tx.ctx, "engine-ecu", map[string]interface{}{
		"motor:voltage":    data.MotorVoltage,
		"motor:current":    data.MotorCurrent,
		"rpm":              data.RPM,
		"speed":            data.Speed,
		"raw-speed":        data.RawSpeed,
		"throttle":         map[bool]string{true: "on", false: "off"}[data.ThrottleOn],
		"brake":            map[bool]string{true: "on", false: "off"}[data.BrakeOn],
		"power":            data.Power,
		"energy:consumed":  data.EnergyConsumed,
		"energy:recovered": data.EnergyRecovered,
	})

	_, err := pipe.Exec(tx.ctx)
	if err != nil {
		return fmt.Errorf("failed to send Status1: %v", err)
	}

	// Publish a throttle notification only when the state changes; Status1 is
	// sent on every frame (speed/current jitter), so an unconditional publish
	// would notify at frame rate.
	if !tx.throttleKnown || data.ThrottleOn != tx.lastThrottleOn {
		tx.throttleKnown = true
		tx.lastThrottleOn = data.ThrottleOn
		if err := tx.redis.Publish(tx.ctx, "engine-ecu", "throttle").Err(); err != nil {
			return fmt.Errorf("failed to publish throttle state: %v", err)
		}
	}

	return nil
}

func (tx *IPCTx) SendStatus2(data RedisStatus2) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	fields := map[string]interface{}{
		"temperature": data.Temperature,
		"fault:code":  data.FaultCode,
	}

	// Only include description if there's an active fault
	if data.FaultCode != 0 && data.FaultDescription != "" {
		fields["fault:description"] = data.FaultDescription
	} else {
		fields["fault:description"] = ""
	}

	if err := tx.redis.HSet(tx.ctx, "engine-ecu", fields).Err(); err != nil {
		return fmt.Errorf("failed to send Status2: %v", err)
	}

	return nil
}

func (tx *IPCTx) SendStatus3(data RedisStatus3) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	pipe := tx.redis.Pipeline()

	pipe.HSet(tx.ctx, "engine-ecu",
		"odometer", data.Odometer,
	)

	// Also publish odometer updates
	pipe.Publish(tx.ctx, "engine-ecu", "odometer")

	_, err := pipe.Exec(tx.ctx)
	if err != nil {
		return fmt.Errorf("failed to send Status3: %v", err)
	}

	return nil
}

func (tx *IPCTx) SendStatus4(data RedisStatus4) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	pipe := tx.redis.Pipeline()

	enabledStr := map[bool]string{true: "enabled", false: "disabled"}
	pipe.HSet(tx.ctx, "engine-ecu", map[string]interface{}{
		"kers":            map[bool]string{true: "on", false: "off"}[data.KersOn],
		"boost":           map[bool]string{true: "on", false: "off"}[data.BoostOn],
		"ecu-status":      enabledStr[data.EcuEnabled],
		"boost-status":    enabledStr[data.BoostActive],
		"gear-mode":       enabledStr[data.GearModeEnabled],
	})

	// Also publish KERS state changes
	pipe.Publish(tx.ctx, "engine-ecu", "kers")

	_, err := pipe.Exec(tx.ctx)
	if err != nil {
		return fmt.Errorf("failed to send Status4: %v", err)
	}

	return nil
}

func (tx *IPCTx) SendEBS(data RedisEBS) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	pipe := tx.redis.Pipeline()
	pipe.HSet(tx.ctx, "engine-ecu", map[string]interface{}{
		"kers-accepted-voltage": data.AcceptedVoltage,
		"kers-accepted-current": data.AcceptedCurrent,
		"regen-available":       map[bool]string{true: "on", false: "off"}[data.RegenAvailable],
		"regen-reason":          data.RegenReason,
		"regen-expected":        data.RegenExpected,
	})
	pipe.Publish(tx.ctx, "engine-ecu", "regen-available")

	if _, err := pipe.Exec(tx.ctx); err != nil {
		return fmt.Errorf("failed to send EBS status: %v", err)
	}

	return nil
}

func (tx *IPCTx) SendStatus5(data RedisStatus5) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	fields := map[string]interface{}{
		"gear": data.Gear,
	}

	// Only set firmware fields if non-zero (avoids overwriting with 0 on startup)
	if data.FirmwareVersion != 0 {
		fields["fw-version"] = fmt.Sprintf("%08X", data.FirmwareVersion)
		fields["motor:rated-power-kw"] = data.MotorRatedPowerKW
		fields["motor:max-speed-kmh"] = data.MotorMaxSpeedKMH
		fields["fw:base-version"] = data.SWBaseVersion
		fields["fw:app-version"] = data.SWAppVersion
	}

	// Per-gear ratios are only meaningful once seen. Publishing zeros at
	// boot would overwrite previously-cached values; skip the whole block
	// when nothing has been reported yet.
	if data.HighGearCurrent != 0 || data.MidGearCurrent != 0 || data.LowGearCurrent != 0 ||
		data.HighGearTorque != 0 || data.MidGearTorque != 0 || data.LowGearTorque != 0 {
		fields["gear:high-current-ratio"] = data.HighGearCurrent
		fields["gear:mid-current-ratio"] = data.MidGearCurrent
		fields["gear:low-current-ratio"] = data.LowGearCurrent
		fields["gear:high-torque-ratio"] = data.HighGearTorque
		fields["gear:mid-torque-ratio"] = data.MidGearTorque
		fields["gear:low-torque-ratio"] = data.LowGearTorque
	}

	if err := tx.redis.HSet(tx.ctx, "engine-ecu", fields).Err(); err != nil {
		return fmt.Errorf("failed to send Status5: %v", err)
	}

	return nil
}

// SendECUConfig publishes the ECU's reported configuration values. Fields
// stay at 0 until the ECU broadcasts them (at boot, or in response to a
// status request), so we skip the write entirely while nothing is known.
func (tx *IPCTx) SendECUConfig(data RedisECUConfig) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	if data == (RedisECUConfig{}) {
		return nil
	}

	fields := map[string]interface{}{
		"config:ov-threshold-mv":       data.OverVoltageThresholdMV,
		"config:uv-threshold-mv":       data.UnderVoltageThresholdMV,
		"config:speed-limit-ratio":     data.SpeedLimitRatio,
		"config:wheel-circumference-cm": data.WheelCircumferenceCM,
		"config:max-phase-current-ma":  data.MaxPhaseCurrentMA,
		"config:startup-phase-current-ma": data.StartupPhaseCurrentMA,
		"config:ebs-voltage-mv":        data.EBSVoltageMV,
		"config:ebs-current-ma":        data.EBSCurrentMA,
	}

	if err := tx.redis.HSet(tx.ctx, "engine-ecu", fields).Err(); err != nil {
		return fmt.Errorf("failed to send ECU config: %v", err)
	}

	return nil
}

func (tx *IPCTx) SendKersReasonOff(reason KersReasonOff) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	pipe := tx.redis.Pipeline()

	reasonStr := "none"
	switch reason {
	case KersReasonOffCold:
		reasonStr = "cold"
	case KersReasonOffHot:
		reasonStr = "hot"
	}

	pipe.HSet(tx.ctx, "engine-ecu",
		"kers-reason-off", reasonStr,
	)

	// Also publish KERS reason off changes
	pipe.Publish(tx.ctx, "engine-ecu", "kers-reason-off")

	_, err := pipe.Exec(tx.ctx)
	if err != nil {
		return fmt.Errorf("failed to send KERS reason off: %v", err)
	}

	return nil
}
