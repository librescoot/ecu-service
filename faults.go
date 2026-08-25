package main

import "fmt"

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
	// Unknown marks a code with no entry in faultMap. Callers log these once so
	// gaps in the table surface from the field instead of being silently
	// reported as a healthy vehicle.
	Unknown bool
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
	FaultBatteryOverVoltage:      {Description: "Battery over-voltage", Severity: "critical"},
	FaultBatteryUnderVoltage:     {Description: "Battery under-voltage", Severity: "critical"},
	FaultMotorShortCircuit:       {Description: "Motor short-circuit", Severity: "critical"},
	FaultMotorStalled:            {Description: "Motor stalled", Severity: "critical"},
	FaultHallSensorAbnormal:      {Description: "Hall sensor abnormal", Severity: "critical"},
	FaultMOSFETCheckError:        {Description: "MOSFET check error", Severity: "critical"},
	FaultMotorOpenCircuit:        {Description: "Motor open-circuit", Severity: "critical"},
	FaultPowerOnSelfCheckError:   {Description: "Power-on self-check error", Severity: "critical"},
	FaultOverTemperature:         {Description: "Over-temperature", Severity: "critical"},
	FaultThrottleAbnormal:        {Description: "Throttle abnormal", Severity: "critical"},
	FaultInternal15vAbnormal:     {Description: "Internal 15V abnormal", Severity: "critical"},
	FaultMotorTempProtection:     {Description: "Motor temperature protection", Severity: "warning"},
	FaultThrottleActiveAtPowerUp: {Description: "Throttle active at power up", Severity: "warning"},
	FaultECUCommLost:             {Description: "ECU communication lost", Severity: "critical"},
}

// MapFault resolves a raw ECU fault code to a fault and its config.
//
// An unrecognised code is NOT treated as "no fault". Doing that took the
// FaultNone branch in ReportFault, which deletes the fault set and clears
// whatever was raised, so a controller reporting something we had never seen
// was announced downstream as a healthy vehicle. Unknown codes are surfaced
// under their own raw number instead, so they show up rather than being
// swallowed.
func MapFault(code uint32) (Fault, FaultConfig) {
	if code == 0 {
		return FaultNone, FaultConfig{}
	}
	if f, ok := faultMap[code]; ok {
		return f, faultConfigs[f]
	}
	// Severity is "warning" deliberately: we do not know what the code means, and
	// crying critical over every gap in the table (0x08 and 0x09 are unassigned
	// as far as we know) would train riders to ignore it. It is still reported.
	return Fault(code), FaultConfig{
		Description: fmt.Sprintf("Unknown ECU fault code %d", code),
		Severity:    "warning",
		Unknown:     true,
	}
}
