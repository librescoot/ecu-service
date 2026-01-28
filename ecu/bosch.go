package ecu

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/brutella/can"
)

const (
	// Bosch ECU CAN IDs - Status messages (0x7xx)
	BoschStatus1FrameID   = 0x7E0 // Voltage, current, RPM, speed, throttle
	BoschStatus2FrameID   = 0x7E1 // Temperature, fault code
	BoschStatus3FrameID   = 0x7E2 // Odometer
	BoschStatus4FrameID   = 0x7E3 // KERS status
	BoschGearFrameID      = 0x7E4 // Current gear
	BoschEBSStatusFrameID = 0x7E5 // Regenerative braking status
	BoschStatus5FrameID   = 0x7E8 // Firmware version

	// Bosch ECU CAN IDs - Control messages (0x4xx)
	BoschControlMessageID     = 0x4E0 // Gear/boost/KERS control
	BoschEBSSetFrameID        = 0x4E2 // Set EBS voltage/current
	BoschStatusRequestFrameID = 0x4EF // Request all ECU status messages

	// Constants for KERS
	KersVoltage         = 56000 // 56V
	KersCurrent         = 10000 // 10A
	BoschGearModeEnable = true

	// Odometer calibration factor
	OdometerCalibrationFactor = 1.07
)

type BoschECU struct {
	BaseECU

	// State
	speed           uint16
	rawSpeed        uint16 // Store raw speed before calibration
	rpm             uint16
	voltage         int
	current         int
	temperature     int8
	odometer        uint32
	faultCode       uint32
	gear            uint8  // Current gear (1-3)
	firmwareVersion uint32 // ECU firmware version
	kersEnabled     bool
	boostEnabled    bool
	throttleOn      bool
}

func NewBoschECU() ECUInterface {
	return &BoschECU{}
}

func (b *BoschECU) Initialize(ctx context.Context, config ECUConfig) error {
	// Initialize base ECU functionality
	if err := b.InitializeBase(ctx, config); err != nil {
		return err
	}

	b.logger.Printf("Initialized Bosch ECU")
	return nil
}

func (b *BoschECU) HandleFrame(frame can.Frame) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Update timestamp for stale data detection
	b.UpdateFrameTimestamp()

	switch frame.ID {
	case BoschStatus1FrameID:
		return b.handleStatus1Frame(frame)
	case BoschStatus2FrameID:
		return b.handleStatus2Frame(frame)
	case BoschStatus3FrameID:
		return b.handleStatus3Frame(frame)
	case BoschStatus4FrameID:
		return b.handleStatus4Frame(frame)
	case BoschGearFrameID:
		return b.handleGearFrame(frame)
	case BoschEBSStatusFrameID:
		return b.handleEBSStatusFrame(frame)
	case BoschStatus5FrameID:
		return b.handleStatus5Frame(frame)
	}

	return nil
}

func (b *BoschECU) handleStatus1Frame(frame can.Frame) error {
	if frame.Length < 8 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 8", frame.ID, frame.Length)
		return nil
	}

	// Voltage (mV)
	b.voltage = int(binary.BigEndian.Uint16(frame.Data[0:2])) * 10

	// Current (mA)
	b.current = int(int16(binary.BigEndian.Uint16(frame.Data[2:4]))) * 10

	// RPM
	b.rpm = binary.BigEndian.Uint16(frame.Data[4:6])

	// Speed with calibration and averaging
	b.rawSpeed = uint16(frame.Data[6]) // Store raw speed
	b.speed = b.calculateSpeed(b.rawSpeed)

	if frame.Length >= 8 {
		b.throttleOn = (frame.Data[7] & 0x01) != 0
	} else {
		b.throttleOn = false
	}

	// Update power metrics
	b.updatePower()

	return nil
}

// updatePower calculates power and integrates energy
// Must be called while holding the lock
func (b *BoschECU) updatePower() {
	now := time.Now()

	// Initialize lastPowerUpdate on first call
	if b.lastPowerUpdate.IsZero() {
		b.lastPowerUpdate = now
		return
	}

	// Calculate time delta in seconds
	dtSeconds := now.Sub(b.lastPowerUpdate).Seconds()

	// Skip update if time delta is too large (ECU was off)
	if dtSeconds > MaxPowerDeltaSeconds {
		b.lastPowerUpdate = now
		return
	}

	b.lastPowerUpdate = now

	// Calculate instantaneous power in mW (voltage in mV, current in mA)
	// Power (mW) = Voltage (mV) × Current (mA) / 1000
	powerMW := int64(b.voltage) * int64(b.current) / 1000

	// Integrate power over time: Energy (mWh) = Power (mW) × time (hours)
	deltaEnergy := float64(powerMW) * dtSeconds / 3600.0

	// Separate consumed vs recovered energy
	if deltaEnergy > 0 {
		b.energyConsumed += uint64(deltaEnergy)
	} else {
		b.energyRecovered += uint64(-deltaEnergy)
	}
}

func (b *BoschECU) handleStatus2Frame(frame can.Frame) error {
	if frame.Length < 6 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 6", frame.ID, frame.Length)
		return nil
	}

	// Temperature
	b.temperature = int8(frame.Data[0])

	// Fault code - filter out fault code 15 which is spurious
	// when software brake is applied in parking mode
	faultCode := binary.BigEndian.Uint32(frame.Data[2:6])
	if faultCode == 15 {
		faultCode = 0
	}
	b.faultCode = faultCode

	return nil
}

func (b *BoschECU) handleStatus3Frame(frame can.Frame) error {
	if frame.Length < 4 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 4", frame.ID, frame.Length)
		return nil
	}

	// Odometer (meters) - converting from 0.1km steps
	rawOdometer := binary.BigEndian.Uint32(frame.Data[0:4])
	b.odometer = uint32(float64(rawOdometer) * OdometerCalibrationFactor * 100)

	return nil
}

func (b *BoschECU) handleStatus4Frame(frame can.Frame) error {
	if frame.Length < 1 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 1", frame.ID, frame.Length)
		return nil
	}

	// KERS status
	b.kersEnabled = (frame.Data[0] & 0x40) != 0

	return nil
}

func (b *BoschECU) handleGearFrame(frame can.Frame) error {
	if frame.Length < 1 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 1", frame.ID, frame.Length)
		return nil
	}

	// Gear number (1-3)
	b.gear = frame.Data[0]
	b.logger.Debug("ECU gear: %d", b.gear)

	return nil
}

func (b *BoschECU) handleEBSStatusFrame(frame can.Frame) error {
	if frame.Length < 4 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 4", frame.ID, frame.Length)
		return nil
	}

	// EBS (regenerative braking) voltage and current (10mV and 10mA units)
	ebsVoltage := binary.BigEndian.Uint16(frame.Data[0:2])
	ebsCurrent := binary.BigEndian.Uint16(frame.Data[2:4])

	b.logger.Debug("ECU EBS: voltage=%dmV, current=%dmA", ebsVoltage*10, ebsCurrent*10)

	return nil
}

func (b *BoschECU) handleStatus5Frame(frame can.Frame) error {
	if frame.Length < 4 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 4", frame.ID, frame.Length)
		return nil
	}

	// Firmware version from ECU
	b.firmwareVersion = binary.BigEndian.Uint32(frame.Data[0:4])
	b.logger.Info("ECU firmware version: 0x%08X", b.firmwareVersion)

	return nil
}

func (b *BoschECU) SetKersEnabled(enabled bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.sendControlMessage(enabled, b.boostEnabled)
}

func (b *BoschECU) SetBoostEnabled(enabled bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.boostEnabled = enabled
	b.logger.Info("Boost setting stored: %v (will apply on next KERS update)", enabled)
	return nil
}

func (b *BoschECU) GetBoostEnabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.boostEnabled
}

// sendControlMessage sends the control frame 0x4E0 with current gear/boost/KERS state
func (b *BoschECU) sendControlMessage(kersEnabled, boostEnabled bool) error {
	b.logger.Info("Setting Bosch ECU control: boost=%v, gear=%v, kers=%v",
		boostEnabled, BoschGearModeEnable, kersEnabled)

	if kersEnabled {
		// Send voltage/current settings first
		data := make([]byte, 4)
		binary.BigEndian.PutUint16(data[0:2], uint16(KersVoltage))
		binary.BigEndian.PutUint16(data[2:4], uint16(KersCurrent))

		ebsFrame := can.Frame{
			ID:     BoschEBSSetFrameID,
			Length: 4,
			Data:   [8]byte{},
		}
		copy(ebsFrame.Data[:], data)

		// Log outgoing CAN frame
		DebugCANFrame(b.logger, "TX", ebsFrame.ID, ebsFrame.Data, ebsFrame.Length)

		if err := b.bus.Publish(ebsFrame); err != nil {
			return err
		}
	}

	// Send control message: [Gear(bit0) | Boost(bit1) | KERS(bit2)]
	controlData := []byte{
		boolToByte(BoschGearModeEnable) |
			(boolToByte(boostEnabled) << 1) |
			(boolToByte(kersEnabled) << 2),
	}

	controlFrame := can.Frame{
		ID:     BoschControlMessageID,
		Length: 1,
		Data:   [8]byte{},
	}
	copy(controlFrame.Data[:], controlData)

	// Log outgoing CAN frame
	DebugCANFrame(b.logger, "TX", controlFrame.ID, controlFrame.Data, controlFrame.Length)

	if err := b.bus.Publish(controlFrame); err != nil {
		return err
	}

	b.kersEnabled = kersEnabled
	return nil
}

// Implement getters
func (b *BoschECU) GetSpeed() uint16 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.speed
}

func (b *BoschECU) GetRPM() uint16 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.rpm
}

func (b *BoschECU) GetTemperature() int8 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.temperature
}

func (b *BoschECU) GetVoltage() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.voltage
}

func (b *BoschECU) GetCurrent() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.current
}

func (b *BoschECU) GetOdometer() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.odometer
}

func (b *BoschECU) GetFaultCode() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.faultCode
}

func (b *BoschECU) GetActiveFaults() map[ECUFault]bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	faults := make(map[ECUFault]bool)

	if b.faultCode != 0 {
		fault := MapBoschFault(b.faultCode)
		if fault != FaultNone {
			faults[fault] = true
		}
	}

	return faults
}

func (b *BoschECU) GetKersEnabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.kersEnabled
}

func (b *BoschECU) GetThrottleOn() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.throttleOn
}

func (b *BoschECU) GetRawSpeed() uint16 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.rawSpeed
}

func (b *BoschECU) GetGear() uint8 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.gear
}

func (b *BoschECU) GetFirmwareVersion() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.firmwareVersion
}

// RequestStatusUpdate sends 0x4EF to request the ECU to transmit all status frames
// This is used after fault detection to check if faults have cleared
func (b *BoschECU) RequestStatusUpdate() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	frame := can.Frame{
		ID:     BoschStatusRequestFrameID,
		Length: 0,
		Data:   [8]byte{},
	}

	DebugCANFrame(b.logger, "TX", frame.ID, frame.Data, frame.Length)

	if err := b.bus.Publish(frame); err != nil {
		b.logger.Error("Failed to send status request: %v", err)
		return err
	}

	b.logger.Debug("Sent ECU status request (0x4EF)")
	return nil
}

func (b *BoschECU) Cleanup() {
	b.CleanupBase()
}
