package ecu

type ECUFault uint32

const (
	FaultNone ECUFault = iota
	FaultBatteryOverVoltage
	FaultBatteryUnderVoltage
	FaultMotorShortCircuit
	FaultMotorStalled
	FaultHallSensorAbnormal
	FaultMOSFETCheckError
	FaultMotorOpenCircuit
	FaultReserved8
	FaultReserved9
	FaultPowerOnSelfCheckError
	FaultOverTemperature
	FaultThrottleAbnormal
	FaultMotorTemperatureProtection
	FaultThrottleActiveAtPowerUp
	FaultReserved15
	FaultInternal15vAbnormal
)

type FaultSeverity int

const (
	SeverityWarning FaultSeverity = iota
	SeverityCritical
)

type FaultConfig struct {
	Code        ECUFault
	Description string
	Severity    FaultSeverity
}

var faultConfigs = map[ECUFault]FaultConfig{
	FaultBatteryOverVoltage:        {FaultBatteryOverVoltage, "Battery over-voltage", SeverityCritical},
	FaultBatteryUnderVoltage:       {FaultBatteryUnderVoltage, "Battery under-voltage", SeverityCritical},
	FaultMotorShortCircuit:         {FaultMotorShortCircuit, "Motor short-circuit", SeverityCritical},
	FaultMotorStalled:              {FaultMotorStalled, "Motor stalled", SeverityCritical},
	FaultHallSensorAbnormal:        {FaultHallSensorAbnormal, "Hall sensor abnormal", SeverityCritical},
	FaultMOSFETCheckError:          {FaultMOSFETCheckError, "MOSFET check error", SeverityCritical},
	FaultMotorOpenCircuit:          {FaultMotorOpenCircuit, "Motor open-circuit", SeverityCritical},
	FaultPowerOnSelfCheckError:     {FaultPowerOnSelfCheckError, "Power-on self-check error", SeverityCritical},
	FaultOverTemperature:           {FaultOverTemperature, "Over-temperature", SeverityCritical},
	FaultThrottleAbnormal:          {FaultThrottleAbnormal, "Throttle abnormal", SeverityCritical},
	FaultInternal15vAbnormal:       {FaultInternal15vAbnormal, "Internal 15V abnormal", SeverityCritical},
	FaultThrottleActiveAtPowerUp:   {FaultThrottleActiveAtPowerUp, "Throttle active at power up", SeverityWarning},
	FaultMotorTemperatureProtection: {FaultMotorTemperatureProtection, "Motor temperature protection", SeverityWarning},
}

func GetFaultConfig(fault ECUFault) (FaultConfig, bool) {
	config, ok := faultConfigs[fault]
	return config, ok
}

var boschFaultMap = map[uint32]ECUFault{
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
	0x0D: FaultMotorTemperatureProtection,
	0x0E: FaultThrottleActiveAtPowerUp,
	0x10: FaultInternal15vAbnormal,
}

func MapBoschFault(code uint32) ECUFault {
	if fault, ok := boschFaultMap[code]; ok {
		return fault
	}
	return FaultNone
}

var votolFaultMap = map[uint32]ECUFault{
	0x01: FaultMotorStalled,
	0x02: FaultHallSensorAbnormal,
	0x04: FaultThrottleAbnormal,
	0x08: FaultPowerOnSelfCheckError,
	0x10: FaultReserved15,
	0x20: FaultOverTemperature,
	0x40: FaultInternal15vAbnormal,
}

func MapVotolFault(code uint32) ECUFault {
	if fault, ok := votolFaultMap[code]; ok {
		return fault
	}
	return FaultNone
}
