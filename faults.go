package main

type Fault uint32

const (
	FaultNone                    Fault = 0
	FaultBatteryOverVoltage      Fault = 1
	FaultBatteryUnderVoltage     Fault = 2
	FaultMotorShortCircuit       Fault = 3
	FaultMotorStalled            Fault = 4
	FaultHallSensorAbnormal      Fault = 5
	FaultMOSFETCheckError        Fault = 6
	FaultMotorOpenCircuit        Fault = 7
	FaultPowerOnSelfCheckError   Fault = 10
	FaultOverTemperature         Fault = 11
	FaultThrottleAbnormal        Fault = 12
	FaultMotorTempProtection     Fault = 13
	FaultThrottleActiveAtPowerUp Fault = 14
	FaultInternal15vAbnormal     Fault = 16
	// FaultECUCommLost (E20) is synthetic: raised by the comm-lost watchdog when
	// the ECU should be powered but has gone silent. Not a CAN-reported code.
	FaultECUCommLost Fault = 20
)

type FaultConfig struct {
	Description string
	Severity    string // "warning" or "critical"
}

var faultMap = map[uint32]Fault{
	0x01: FaultBatteryOverVoltage,
	0x02: FaultBatteryUnderVoltage,
	0x03: FaultMotorShortCircuit,
	0x04: FaultMotorStalled,
	0x05: FaultHallSensorAbnormal,
	0x06: FaultMOSFETCheckError,
	0x07: FaultMotorOpenCircuit,
	0x0A: FaultPowerOnSelfCheckError,
	0x0B: FaultOverTemperature,
	0x0C: FaultThrottleAbnormal,
	0x0D: FaultMotorTempProtection,
	0x0E: FaultThrottleActiveAtPowerUp,
	0x10: FaultInternal15vAbnormal,
}

var faultConfigs = map[Fault]FaultConfig{
	FaultBatteryOverVoltage:      {"Battery over-voltage", "critical"},
	FaultBatteryUnderVoltage:     {"Battery under-voltage", "critical"},
	FaultMotorShortCircuit:       {"Motor short-circuit", "critical"},
	FaultMotorStalled:            {"Motor stalled", "critical"},
	FaultHallSensorAbnormal:      {"Hall sensor abnormal", "critical"},
	FaultMOSFETCheckError:        {"MOSFET check error", "critical"},
	FaultMotorOpenCircuit:        {"Motor open-circuit", "critical"},
	FaultPowerOnSelfCheckError:   {"Power-on self-check error", "critical"},
	FaultOverTemperature:         {"Over-temperature", "critical"},
	FaultThrottleAbnormal:        {"Throttle abnormal", "critical"},
	FaultInternal15vAbnormal:     {"Internal 15V abnormal", "critical"},
	FaultMotorTempProtection:     {"Motor temperature protection", "warning"},
	FaultThrottleActiveAtPowerUp: {"Throttle active at power up", "warning"},
	FaultECUCommLost:             {"ECU communication lost", "critical"},
}

func MapFault(code uint32) (Fault, FaultConfig) {
	if f, ok := faultMap[code]; ok {
		return f, faultConfigs[f]
	}
	return FaultNone, FaultConfig{}
}
