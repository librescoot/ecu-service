package ecu

// ErrorCode represents ECU error codes
type ErrorCode uint32

const (
    ErrorNone ErrorCode = iota
    ErrorBatteryOverVoltage
    ErrorBatteryUnderVoltage
    ErrorMotorShortCircuit
    ErrorMotorStalled
    ErrorHallSensorAbnormal
    ErrorMosfetCheckError
    ErrorMotorOpenCircuit
    ErrorSelfCheckError
    ErrorOverTemperature
    ErrorThrottleAbnormal
    ErrorMotorTemperatureProtection
    ErrorThrottleActiveAtPowerUp
    ErrorBrakingActive
    ErrorInternal15VAbnormal
)

// Error code mappings for different ECU types
var (
    // Bosch error codes map directly to ErrorCode values
    boschErrorMap = map[uint32]ErrorCode{
        0x01: ErrorBatteryOverVoltage,
        0x02: ErrorBatteryUnderVoltage,
        0x03: ErrorMotorShortCircuit,
        0x04: ErrorMotorStalled,
        0x05: ErrorHallSensorAbnormal,
        0x06: ErrorMosfetCheckError,
        0x07: ErrorMotorOpenCircuit,
        0x0A: ErrorSelfCheckError,
        0x0B: ErrorOverTemperature,
        0x0C: ErrorThrottleAbnormal,
        0x0D: ErrorMotorTemperatureProtection,
        0x0E: ErrorThrottleActiveAtPowerUp,
        0x0F: ErrorBrakingActive,
        0x10: ErrorInternal15VAbnormal,
    }

    // Votol error code mapping
    votolErrorMap = map[uint32]ErrorCode{
        0x01: ErrorMotorStalled,            // MOTO Error
        0x02: ErrorHallSensorAbnormal,      // HALL Error
        0x04: ErrorThrottleAbnormal,        // Throttle Error
        0x08: ErrorSelfCheckError,          // Controller Error
        0x10: ErrorBrakingActive,           // Brake Error
        0x20: ErrorOverTemperature,         // MOTOR OVERTEMPERATURE
        0x40: ErrorInternal15VAbnormal,     // CONTROLLER OVERTEMPERATURE
    }
)

// Common error descriptions
var errorDescriptions = map[ErrorCode]string{
    ErrorNone:                      "No error",
    ErrorBatteryOverVoltage:        "Battery over-voltage",
    ErrorBatteryUnderVoltage:       "Battery under-voltage",
    ErrorMotorShortCircuit:         "Motor short-circuit",
    ErrorMotorStalled:              "Motor stalled",
    ErrorHallSensorAbnormal:        "Hall sensor abnormal",
    ErrorMosfetCheckError:          "MOSFET check error",
    ErrorMotorOpenCircuit:          "Motor open-circuit",
    ErrorSelfCheckError:            "Self-check error",
    ErrorOverTemperature:           "Over-temperature",
    ErrorThrottleAbnormal:          "Throttle abnormal",
    ErrorMotorTemperatureProtection: "Motor temperature protection",
    ErrorThrottleActiveAtPowerUp:   "Throttle active at power up",
    ErrorBrakingActive:             "Braking active",
    ErrorInternal15VAbnormal:       "Internal 15V abnormal",
}

// GetErrorDescription returns a human-readable description of an error code
func GetErrorDescription(code ErrorCode) string {
    if desc, ok := errorDescriptions[code]; ok {
        return desc
    }
    return "Unknown error"
}

// MapBoschError converts a Bosch error code to the common ErrorCode type
func MapBoschError(code uint32) ErrorCode {
    if mappedError, ok := boschErrorMap[code]; ok {
        return mappedError
    }
    return ErrorNone
}

// MapVotolError converts a Votol error code to the common ErrorCode type
func MapVotolError(code uint32) ErrorCode {
    if mappedError, ok := votolErrorMap[code]; ok {
        return mappedError
    }
    return ErrorNone
}
