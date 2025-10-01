package ecu

import (
	"context"
	"log"

	"github.com/brutella/can"
)

// ECUType represents the type of ECU
type ECUType int

const (
	ECUTypeBosch ECUType = iota
	ECUTypeVotol
)

// ECUConfig contains configuration for the ECU
type ECUConfig struct {
	Logger    *log.Logger
	CANDevice string
	CANBus    *can.Bus
	ECUType   ECUType
}

// ECUInterface defines the interface that all ECU implementations must satisfy
type ECUInterface interface {
	// Initialize sets up the ECU module
	Initialize(ctx context.Context, config ECUConfig) error

	// HandleFrame processes incoming CAN frames
	HandleFrame(frame can.Frame) error

	// SetKersEnabled enables/disables KERS functionality
	SetKersEnabled(enabled bool) error

	// GetSpeed returns the current speed in km/h
	GetSpeed() uint16

	// GetRawSpeed returns the raw speed before calibration
	GetRawSpeed() uint16

	// GetRPM returns the current motor RPM
	GetRPM() uint16

	// GetTemperature returns the current ECU temperature
	GetTemperature() int8

	// GetVoltage returns the current motor voltage in mV
	GetVoltage() int

	// GetCurrent returns the current motor current in mA
	GetCurrent() int

	// GetOdometer returns the total distance in meters
	GetOdometer() uint32

	// GetFaultCode returns the current fault code
	GetFaultCode() uint32

	// GetActiveFaults returns a map of currently active faults
	GetActiveFaults() map[ECUFault]bool

	// GetThrottleOn returns true if the throttle is currently active
	GetThrottleOn() bool

	// GetKersEnabled returns whether KERS is enabled
	GetKersEnabled() bool

	// Cleanup performs any necessary cleanup
	Cleanup()
}

func NewECU(ecuType ECUType) ECUInterface {
	switch ecuType {
	case ECUTypeBosch:
		log.Printf("Creating Bosch ECU")
		return NewBoschECU()
	case ECUTypeVotol:
		log.Printf("Creating Votol ECU")
		return NewVotolECU()
	default:
		log.Printf("Unknown ECU type: %v", ecuType)
		return nil
	}
}
