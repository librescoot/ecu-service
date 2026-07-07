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
	BoschStatus1FrameID             = 0x7E0 // Voltage, current, RPM, speed, throttle
	BoschStatus2FrameID             = 0x7E1 // Temperature, fault code
	BoschStatus3FrameID             = 0x7E2 // Odometer
	BoschStatus4FrameID             = 0x7E3 // Bit-packed status flags
	BoschGearFrameID                = 0x7E4 // Current gear + per-gear ratios
	BoschEBSStatusFrameID           = 0x7E5 // Regenerative braking settings
	BoschStatus5FrameID             = 0x7E8 // Motor power / max speed / SW version
	BoschMaxVoltageFrameID          = 0x7E9 // Over-voltage threshold (10 mV units)
	BoschMinVoltageFrameID          = 0x7EA // Under-voltage threshold (10 mV units)
	BoschSpeedLimitFrameID          = 0x7EB // Speed limit (%)
	BoschWheelCircumferenceFrameID  = 0x7EC // Wheel circumference (cm)
	BoschMaxPhaseCurrentFrameID     = 0x7EE // Max phase current (10 mA units)
	BoschStartupPhaseCurrentFrameID = 0x7EF // Startup phase current (10 mA units)

	// Bosch ECU CAN IDs - Control messages (0x4xx)
	BoschControlMessageID     = 0x4E0 // Gear/boost/KERS control
	BoschGearControlFrameID   = 0x4E1 // Set per-gear ratio
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

	// Configuration
	gearRatioValues []uint8 // Per-gear ratio values (1-3 entries) sent via 0x4E1

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
	throttleOn           bool
	brakeOn              bool
	gearsSentOnPower     bool // whether gearRatioValues have been sent since the ECU last powered on

	energyConsumedFrac  float64 // sub-mWh remainder carried across frames
	energyRecoveredFrac float64

	// Status bits reported by the ECU (paired enable/disable flags)
	ecuStatusEnabled bool
	boostActive      bool
	gearModeEnabled  bool

	// Per-gear current and torque ratios (0-100 %)
	gearRatioHighCurrent uint8
	gearRatioMidCurrent  uint8
	gearRatioLowCurrent  uint8
	gearRatioHighTorque  uint8
	gearRatioMidTorque   uint8
	gearRatioLowTorque   uint8

	// Decoded software-version components
	motorRatedPowerKW uint8 // Motor rated power in kW
	motorMaxSpeedKMH  uint8 // Motor max speed in km/h
	swBaseVersion     uint8 // Base SW version byte (high nibble.low nibble)
	swAppVersion      uint8 // Application SW version byte (high nibble.low nibble)

	// Reported ECU configuration values
	ovThresholdMV         uint32 // Over-voltage cut-off threshold in mV
	uvThresholdMV         uint32 // Under-voltage cut-off threshold in mV
	speedLimitRatio       uint8  // Speed limit as a percentage
	wheelCircumferenceCM  uint8  // Wheel circumference in cm
	maxPhaseCurrentMA     uint32 // Maximum phase current in mA
	startupPhaseCurrentMA uint32 // Startup phase current in mA
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

	b.gearRatioValues = config.GearRatioValues

	if len(b.gearRatioValues) > 0 {
		b.logger.Printf("Initialized Bosch ECU with gear ratios: %v", b.gearRatioValues)
	} else {
		b.logger.Printf("Initialized Bosch ECU")
	}
	return nil
}

// SetGear sends the configured ratio for the given gear (1-based) to the ECU
// via the 0x4E1 control message. Not called while handling an incoming CAN
// frame — HandleFrame already holds b.mu, and this method takes it too.
func (b *BoschECU) SetGear(gear uint8) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.gearRatioValues) == 0 {
		return fmt.Errorf("no gear ratios configured")
	}

	if gear < 1 || gear > uint8(len(b.gearRatioValues)) {
		return fmt.Errorf("invalid gear %d: must be between 1 and %d", gear, len(b.gearRatioValues))
	}

	ratio := b.gearRatioValues[gear-1]

	b.logger.Debug("Setting Bosch ECU gear %d (ratio: %d)", gear, ratio)

	gearFrame := can.Frame{
		ID:     BoschGearControlFrameID,
		Length: 1,
		Data:   [8]byte{},
	}
	gearFrame.Data[0] = ratio

	return b.bus.Publish(gearFrame)
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
	case BoschMaxVoltageFrameID:
		return b.handleMaxVoltageFrame(frame)
	case BoschMinVoltageFrameID:
		return b.handleMinVoltageFrame(frame)
	case BoschSpeedLimitFrameID:
		return b.handleSpeedLimitFrame(frame)
	case BoschWheelCircumferenceFrameID:
		return b.handleWheelCircumferenceFrame(frame)
	case BoschMaxPhaseCurrentFrameID:
		return b.handleMaxPhaseCurrentFrame(frame)
	case BoschStartupPhaseCurrentFrameID:
		return b.handleStartupPhaseCurrentFrame(frame)
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

	// Send configured gear ratios once per power-up, detected by the first
	// Status1 frame. Built inline rather than via SetGear: HandleFrame
	// already holds b.mu here, and SetGear takes it too.
	if !b.gearsSentOnPower && len(b.gearRatioValues) > 0 {
		b.gearsSentOnPower = true
		for i, ratio := range b.gearRatioValues {
			gear := uint8(i + 1)
			gearFrame := can.Frame{
				ID:     BoschGearControlFrameID,
				Length: 1,
				Data:   [8]byte{},
			}
			gearFrame.Data[0] = ratio
			if err := b.bus.Publish(gearFrame); err != nil {
				b.logger.Error("Failed to send gear %d on power-up: %v", gear, err)
			} else {
				b.logger.Debug("Sent gear %d (ratio: %d) on ECU power-up", gear, ratio)
			}
		}
	}

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

	// Fault code 15 = "braking indication": the ECU reports it whenever the
	// brake input is active. Suppress it here so brake-on doesn't surface as
	// a fault — the actual brake state is already exposed via brakeOn.
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

	// Byte 0 is a bit-packed status register with paired enable/disable
	// flags. The "enable" bit going high marks the feature as on; the
	// "disable" bit going high marks it off. We treat the disable bit as
	// authoritative when set, otherwise track the enable bit.
	byte0 := frame.Data[0]

	if byte0&0x02 != 0 { // bit 1: ECU disabled
		b.ecuStatusEnabled = false
	} else if byte0&0x01 != 0 { // bit 0: ECU enabled
		b.ecuStatusEnabled = true
	}

	if byte0&0x08 != 0 { // bit 3: boost disabled
		b.boostActive = false
	} else if byte0&0x04 != 0 { // bit 2: boost enabled
		b.boostActive = true
	}

	if byte0&0x20 != 0 { // bit 5: gear mode disabled
		b.gearModeEnabled = false
	} else if byte0&0x10 != 0 { // bit 4: gear mode enabled
		b.gearModeEnabled = true
	}

	if byte0&0x80 != 0 { // bit 7: KERS/EBS disabled
		b.kersEnabled = false
	} else if byte0&0x40 != 0 { // bit 6: KERS/EBS enabled
		b.kersEnabled = true
	}

	return nil
}

func (b *BoschECU) handleGearFrame(frame can.Frame) error {
	if frame.Length < 1 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 1", frame.ID, frame.Length)
		return nil
	}

	// Gear number (1-3)
	b.gear = frame.Data[0]

	// Bytes 1-6 carry per-gear current/torque ratios (0-100 %). The full
	// frame is 7 bytes; short frames just leave the ratios as-is.
	if frame.Length >= 7 {
		b.gearRatioHighCurrent = frame.Data[1]
		b.gearRatioMidCurrent = frame.Data[2]
		b.gearRatioLowCurrent = frame.Data[3]
		b.gearRatioHighTorque = frame.Data[4]
		b.gearRatioMidTorque = frame.Data[5]
		b.gearRatioLowTorque = frame.Data[6]
	}

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
	// live measurement. The echo uses the same 10 mV / 10 mA per-LSB steps as
	// the EBS Set frame, so scale by 10 to get mV / mA.
	ebsVoltage := binary.BigEndian.Uint16(frame.Data[0:2])
	ebsCurrent := binary.BigEndian.Uint16(frame.Data[2:4])

	b.acceptedRegenVoltage = int(ebsVoltage) * 10
	b.acceptedRegenCurrent = int(ebsCurrent) * 10

	b.logger.Debug("ECU EBS: voltage=%dmV, current=%dmA", b.acceptedRegenVoltage, b.acceptedRegenCurrent)

	return nil
}

func (b *BoschECU) handleStatus5Frame(frame can.Frame) error {
	if frame.Length < 8 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 8", frame.ID, frame.Length)
		return nil
	}

	// Status5 layout (8 bytes, big-endian):
	//   [0:4] reserved (historically warranty_date)
	//   [4]   motor rated power (BCD, kW)
	//   [5]   motor max speed   (BCD, km/h)
	//   [6]   base SW version   (BCD, high.low nibbles)
	//   [7]   app SW version    (BCD, high.low nibbles)
	b.warrantyDate = binary.BigEndian.Uint32(frame.Data[0:4])
	b.firmwareVersion = binary.BigEndian.Uint32(frame.Data[4:8])

	b.motorRatedPowerKW = bcdByteToDecimal(frame.Data[4])
	b.motorMaxSpeedKMH = bcdByteToDecimal(frame.Data[5])
	b.swBaseVersion = frame.Data[6]
	b.swAppVersion = frame.Data[7]

	b.logger.Debug("ECU firmware: %dkW / %dkm/h / base=%s / app=%s",
		b.motorRatedPowerKW, b.motorMaxSpeedKMH,
		formatBCDVersion(b.swBaseVersion), formatAppVersion(b.swAppVersion))

	return nil
}

func (b *BoschECU) handleMaxVoltageFrame(frame can.Frame) error {
	if frame.Length < 2 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 2", frame.ID, frame.Length)
		return nil
	}
	b.ovThresholdMV = uint32(binary.BigEndian.Uint16(frame.Data[0:2])) * 10
	b.logger.Debug("ECU over-voltage threshold: %d mV", b.ovThresholdMV)
	return nil
}

func (b *BoschECU) handleMinVoltageFrame(frame can.Frame) error {
	if frame.Length < 2 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 2", frame.ID, frame.Length)
		return nil
	}
	b.uvThresholdMV = uint32(binary.BigEndian.Uint16(frame.Data[0:2])) * 10
	b.logger.Debug("ECU under-voltage threshold: %d mV", b.uvThresholdMV)
	return nil
}

func (b *BoschECU) handleSpeedLimitFrame(frame can.Frame) error {
	if frame.Length < 1 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 1", frame.ID, frame.Length)
		return nil
	}
	b.speedLimitRatio = frame.Data[0]
	b.logger.Debug("ECU speed limit ratio: %d %%", b.speedLimitRatio)
	return nil
}

func (b *BoschECU) handleWheelCircumferenceFrame(frame can.Frame) error {
	if frame.Length < 1 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 1", frame.ID, frame.Length)
		return nil
	}
	b.wheelCircumferenceCM = frame.Data[0]
	b.logger.Debug("ECU wheel circumference: %d cm", b.wheelCircumferenceCM)
	return nil
}

func (b *BoschECU) handleMaxPhaseCurrentFrame(frame can.Frame) error {
	if frame.Length < 2 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 2", frame.ID, frame.Length)
		return nil
	}
	b.maxPhaseCurrentMA = uint32(binary.BigEndian.Uint16(frame.Data[0:2])) * 10
	b.logger.Debug("ECU max phase current: %d mA", b.maxPhaseCurrentMA)
	return nil
}

func (b *BoschECU) handleStartupPhaseCurrentFrame(frame can.Frame) error {
	if frame.Length < 2 {
		b.logger.Warn("Short CAN frame 0x%X: got %d bytes, need 2", frame.ID, frame.Length)
		return nil
	}
	b.startupPhaseCurrentMA = uint32(binary.BigEndian.Uint16(frame.Data[0:2])) * 10
	b.logger.Debug("ECU startup phase current: %d mA", b.startupPhaseCurrentMA)
	return nil
}

// bcdByteToDecimal converts a two-digit BCD byte to its decimal value.
// 0x45 -> 45. Falls back to the raw value if either nibble is out of BCD range.
func bcdByteToDecimal(b byte) uint8 {
	high := b >> 4
	low := b & 0x0F
	if high > 9 || low > 9 {
		return b
	}
	return high*10 + low
}

// formatBCDVersion renders a BCD-style version byte as "<hi>.<lo>", using
// hex digits so non-decimal nibbles survive the round-trip (some firmwares
// leak hex into the low nibble once the BCD digit space is exhausted).
func formatBCDVersion(b byte) string {
	return fmt.Sprintf("%X.%X", b>>4, b&0x0F)
}

// formatAppVersion renders the application SW version byte as its
// decimal value. The byte appears to be a revision number rather than
// a major.minor pair, so 0x0C becomes "12" rather than "0.C" or "0C".
func formatAppVersion(b byte) string {
	return fmt.Sprintf("%d", b)
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
	return b.boostActive
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

func (b *BoschECU) GetECUStatusEnabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ecuStatusEnabled
}

func (b *BoschECU) GetBoostActive() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.boostActive
}

func (b *BoschECU) GetGearModeEnabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.gearModeEnabled
}

func (b *BoschECU) GetGearRatios() GearRatios {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return GearRatios{
		HighCurrent: b.gearRatioHighCurrent,
		MidCurrent:  b.gearRatioMidCurrent,
		LowCurrent:  b.gearRatioLowCurrent,
		HighTorque:  b.gearRatioHighTorque,
		MidTorque:   b.gearRatioMidTorque,
		LowTorque:   b.gearRatioLowTorque,
	}
}

func (b *BoschECU) GetSoftwareVersion() SoftwareVersion {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return SoftwareVersion{
		MotorRatedPowerKW: b.motorRatedPowerKW,
		MotorMaxSpeedKMH:  b.motorMaxSpeedKMH,
		BaseVersion:       formatBCDVersion(b.swBaseVersion),
		AppVersion:        formatAppVersion(b.swAppVersion),
	}
}

func (b *BoschECU) GetConfigReport() ConfigReport {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return ConfigReport{
		OverVoltageThresholdMV:  b.ovThresholdMV,
		UnderVoltageThresholdMV: b.uvThresholdMV,
		SpeedLimitRatio:         b.speedLimitRatio,
		WheelCircumferenceCM:    b.wheelCircumferenceCM,
		MaxPhaseCurrentMA:       b.maxPhaseCurrentMA,
		StartupPhaseCurrentMA:   b.startupPhaseCurrentMA,
		EBSVoltageMV:            uint32(b.acceptedRegenVoltage),
		EBSCurrentMA:            uint32(b.acceptedRegenCurrent),
	}
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
