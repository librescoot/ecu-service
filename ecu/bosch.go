package ecu

import (
	"context"
	"encoding/binary"
	"fmt"
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
	DefaultKersVoltage  = 56000 // 56V
	DefaultKersCurrent  = 10000 // 10A
	MinKersVoltage      = 42000 // 42V
	MaxKersVoltage      = 58000 // 58V
	BoschGearModeEnable = true

	// Odometer calibration factor
	OdometerCalibrationFactor = 1.07
)

type BoschECU struct {
	BaseECU

	// State
	speed                uint16
	rawSpeed             uint16 // Store raw speed before calibration
	rpm                  uint16
	voltage              int
	current              int
	temperature          int8
	odometer             uint32
	faultCode            uint32
	gear                 uint8  // Current gear (1-3)
	firmwareVersion      uint32 // ECU firmware version
	warrantyDate         uint32 // ECU warranty date
	kersEnabled          bool
	kersCurrent          uint16 // KERS current in mA (commanded setpoint)
	kersVoltage          uint16 // KERS voltage in mV (commanded setpoint)
	acceptedRegenCurrent int    // EBS regen current limit the ECU accepted, in mA (0x7E5 echo)
	acceptedRegenVoltage int    // EBS regen voltage cap the ECU accepted, in mV (0x7E5 echo)
	boostEnabled         bool   // commanded boost (drives the control frame)
	boostReported        bool   // boost state the ECU acknowledges in status4
	throttleOn           bool
	brakeOn              bool

	energyConsumedFrac  float64 // sub-mWh remainder carried across frames
	energyRecoveredFrac float64
}

func NewBoschECU() ECUInterface {
	return &BoschECU{
		kersCurrent: DefaultKersCurrent,
		kersVoltage: DefaultKersVoltage,
	}
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
		b.brakeOn = (frame.Data[7] & 0x02) != 0
	} else {
		b.throttleOn = false
		b.brakeOn = false
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

	// Separate consumed vs recovered energy. Carry the sub-mWh remainder
	// across frames so the per-frame truncation doesn't systematically
	// undercount (at ~10 Hz, up to ~1 mWh/frame would otherwise be dropped).
	if deltaEnergy > 0 {
		b.energyConsumedFrac += deltaEnergy
		whole := uint64(b.energyConsumedFrac)
		b.energyConsumed += whole
		b.energyConsumedFrac -= float64(whole)
	} else {
		b.energyRecoveredFrac += -deltaEnergy
		whole := uint64(b.energyRecoveredFrac)
		b.energyRecovered += whole
		b.energyRecoveredFrac -= float64(whole)
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
	if faultCode != b.faultCode {
		b.logger.Info("ECU fault_code transition %d -> %d (temperature=%d)", b.faultCode, faultCode, b.temperature)
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

	// KERS status (ebs_enabled, bit 6) and boost status (boost_mode_enabled,
	// bit 2) as acknowledged by the ECU.
	b.kersEnabled = (frame.Data[0] & 0x40) != 0
	b.boostReported = (frame.Data[0] & 0x04) != 0

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

	// The EBS Status frame echoes the regen caps the ECU accepted after its
	// own clamping of the EBS Set command. This is the stored config, not a
	// live measurement. Empirically the echo fields are already in mV / mA
	// (1 unit = 1 mV / 1 mA), unlike the EBS Set frame's 10 mV / 10 mA steps.
	ebsVoltage := binary.BigEndian.Uint16(frame.Data[0:2])
	ebsCurrent := binary.BigEndian.Uint16(frame.Data[2:4])

	b.acceptedRegenVoltage = int(ebsVoltage)
	b.acceptedRegenCurrent = int(ebsCurrent)

	b.logger.Debug("ECU EBS: voltage=%dmV, current=%dmA", b.acceptedRegenVoltage, b.acceptedRegenCurrent)

	return nil
}

func (b *BoschECU) handleStatus5Frame(frame can.Frame) error {
	if frame.Length < 8 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 8", frame.ID, frame.Length)
		return nil
	}

	// Status5 layout (8 bytes, big-endian):
	//   [0:4] warranty_date
	//   [4:8] software_version
	b.warrantyDate = binary.BigEndian.Uint32(frame.Data[0:4])
	b.firmwareVersion = binary.BigEndian.Uint32(frame.Data[4:8])
	b.logger.Debug("ECU firmware version: 0x%08X (warranty: 0x%08X)", b.firmwareVersion, b.warrantyDate)

	return nil
}

func (b *BoschECU) SetKersEnabled(enabled bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.sendControlMessage(enabled, b.boostEnabled)
}

func (b *BoschECU) SetKersCurrent(current uint16) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.kersCurrent = current
	b.logger.Info("KERS current set to: %d mA", current)
	return nil
}

func (b *BoschECU) SetKersVoltage(voltage uint16) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if voltage < MinKersVoltage || voltage > MaxKersVoltage {
		return fmt.Errorf("KERS voltage %d mV out of range [%d, %d]", voltage, MinKersVoltage, MaxKersVoltage)
	}

	b.kersVoltage = voltage
	b.logger.Info("KERS voltage set to: %d mV", voltage)
	return nil
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
	return b.boostReported
}

// GetInstantPower returns instantaneous power in mW from this ECU's own voltage
// and current. The embedded BaseECU.GetInstantPower reads lastVoltage/
// lastCurrent, which this ECU never populates (it keeps its own voltage/
// current fields), so without this override the power hash field stays 0.
func (b *BoschECU) GetInstantPower() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return int(int64(b.voltage) * int64(b.current) / 1000)
}

// sendControlMessage sends the control frame 0x4E0 with current gear/boost/KERS state
func (b *BoschECU) sendControlMessage(kersEnabled, boostEnabled bool) error {
	b.logger.Info("Setting Bosch ECU control: boost=%v, gear=%v, kers=%v",
		boostEnabled, BoschGearModeEnable, kersEnabled)

	if kersEnabled {
		// Send voltage/current settings first
		// CAN wire format uses 10mV and 10mA units
		data := make([]byte, 4)
		binary.BigEndian.PutUint16(data[0:2], b.kersVoltage/10)
		binary.BigEndian.PutUint16(data[2:4], b.kersCurrent/10)

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

func (b *BoschECU) GetAcceptedRegenVoltage() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.acceptedRegenVoltage
}

func (b *BoschECU) GetAcceptedRegenCurrent() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.acceptedRegenCurrent
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

func (b *BoschECU) GetWarrantyDate() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.warrantyDate
}

func (b *BoschECU) GetBrakeOn() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.brakeOn
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
