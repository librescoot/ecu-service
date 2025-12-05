package ecu

import (
	"context"
	"encoding/binary"
	"sync"

	"github.com/brutella/can"
)

const (
	// Votol ECU CAN IDs
	VotolDisplayControllerID = 0x9026105A
	VotolVCUControllerID     = 0x90262001
	VotolControllerDisplayID = 0x90261022
	VotolControllerStatusID  = 0x90261023

	// Update rates
	VotolDisplayRate = 250 // ms
	VotolControlRate = 100 // ms
	VotolStatusRate  = 50  // ms
)

type VotolECU struct {
	mu     sync.RWMutex
	logger Logger
	bus    *can.Bus
	ctx    context.Context
	cancel context.CancelFunc

	// State
	speed       uint16
	rawSpeed    uint16 // Store raw speed before calibration
	rpm         uint16
	voltage     int
	current     int
	temperature int8
	odometer    uint32
	faultCode   uint32
	kersEnabled bool
	throttleOn  bool // Votol ECU does not seem to report throttle, will default to false
}

func NewVotolECU() ECUInterface {
	return &VotolECU{}
}

func (v *VotolECU) Initialize(ctx context.Context, config ECUConfig) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.logger = config.Logger
	v.bus = config.CANBus

	// Create cancellable context
	v.ctx, v.cancel = context.WithCancel(ctx)

	v.logger.Info("Initialized Votol ECU")
	return nil
}

func (v *VotolECU) HandleFrame(frame can.Frame) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	switch frame.ID {
	case VotolDisplayControllerID:
		return v.handleDisplayControllerFrame(frame)
	case VotolControllerDisplayID:
		return v.handleControllerDisplayFrame(frame)
	case VotolControllerStatusID:
		return v.handleControllerStatusFrame(frame)
	}

	return nil
}

func (v *VotolECU) handleDisplayControllerFrame(frame can.Frame) error {
	if frame.Length < 8 {
		return nil
	}

	// NOTE: This frame is not currently being received from the Votol ECU
	// Speed is calculated from RPM in handleControllerDisplayFrame instead

	// data5 contains speed (0-199 km/h)
	v.rawSpeed = uint16(frame.Data[5]) // Store raw speed
	v.speed = v.rawSpeed               // Votol speed is already calibrated

	// data0-1 contain odometer low/high bytes (little-endian)
	odo := binary.LittleEndian.Uint16(frame.Data[0:2])
	v.odometer = uint32(odo) * 1000 // Convert to meters

	return nil
}

func (v *VotolECU) handleControllerDisplayFrame(frame can.Frame) error {
	if frame.Length < 8 {
		return nil
	}

	// data2-3 contain RPM (little-endian)
	v.rpm = binary.LittleEndian.Uint16(frame.Data[2:4])

	// Calculate speed from RPM since Votol doesn't provide speed directly
	v.rawSpeed = v.rpm
	v.speed = uint16(float64(v.rpm) * RPMToSpeedFactor)

	// data4-5 contain battery voltage (0.1V/bit, little-endian)
	voltageRaw := binary.LittleEndian.Uint16(frame.Data[4:6])
	v.voltage = int(voltageRaw) * 100 // Convert to mV

	// data6-7 contain battery current (0.1A/bit, little-endian, signed for regen)
	currentRaw := int16(binary.LittleEndian.Uint16(frame.Data[6:8]))
	v.current = int(currentRaw) * 100 // Convert to mA

	return nil
}

func (v *VotolECU) handleControllerStatusFrame(frame can.Frame) error {
	if frame.Length < 8 {
		return nil
	}

	// data0 contains controller temperature
	v.temperature = int8(frame.Data[0])

	// data6 contains error codes (always update to allow fault clearing)
	v.faultCode = uint32(frame.Data[6])

	return nil
}

// Implement getters
func (v *VotolECU) GetSpeed() uint16 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.speed
}

func (v *VotolECU) GetRPM() uint16 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.rpm
}

func (v *VotolECU) GetTemperature() int8 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.temperature
}

func (v *VotolECU) GetVoltage() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.voltage
}

func (v *VotolECU) GetCurrent() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.current
}

func (v *VotolECU) GetOdometer() uint32 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.odometer
}

func (v *VotolECU) GetFaultCode() uint32 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.faultCode
}

func (v *VotolECU) GetActiveFaults() map[ECUFault]bool {
	v.mu.RLock()
	defer v.mu.RUnlock()

	faults := make(map[ECUFault]bool)

	for bit := 0; bit < 8; bit++ {
		if (v.faultCode & (1 << bit)) != 0 {
			votolCode := uint32(1 << bit)
			fault := MapVotolFault(votolCode)
			if fault != FaultNone {
				faults[fault] = true
			}
		}
	}

	return faults
}

func (v *VotolECU) GetKersEnabled() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.kersEnabled
}

func (v *VotolECU) GetThrottleOn() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	// Votol CAN messages, as currently parsed, do not provide throttle status.
	return v.throttleOn
}

func (v *VotolECU) SetKersEnabled(enabled bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.kersEnabled = enabled
	// TODO: Implement actual CAN message sending for KERS control
	return nil
}

func (v *VotolECU) Cleanup() {
	if v.cancel != nil {
		v.cancel()
	}
}

// Add getter for raw speed
func (v *VotolECU) GetRawSpeed() uint16 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.rawSpeed
}

// IsDataStale returns false for Votol (stale data detection not implemented yet)
func (v *VotolECU) IsDataStale() bool {
	// TODO: Implement stale data detection for Votol ECU
	return false
}

// GetGear returns 0 for Votol ECU (gear selection not supported)
func (v *VotolECU) GetGear() uint8 {
	return 0
}

// GetFirmwareVersion returns 0 for Votol ECU (not available via CAN)
func (v *VotolECU) GetFirmwareVersion() uint32 {
	return 0
}

// RequestStatusUpdate is a no-op for Votol ECU as it sends status frames continuously
// Unlike Bosch, there's no request mechanism - faults clear automatically when status frames arrive
func (v *VotolECU) RequestStatusUpdate() error {
	// Votol ECU sends status frames continuously, no request needed
	return nil
}
