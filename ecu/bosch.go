package ecu

import (
	"context"
	"encoding/binary"
	"log"
	"sync"

	"github.com/brutella/can"
)

const (
	// Bosch ECU CAN IDs
	BoschStatus1FrameID   = 0x7E0
	BoschStatus2FrameID   = 0x7E1
	BoschStatus3FrameID   = 0x7E2
	BoschStatus4FrameID   = 0x7E3
	BoschEBSSetFrameID    = 0x4E2
	BoschControlMessageID = 0x4E0

	// Constants for KERS
	KersVoltage          = 56000 // 56V
	KersCurrent          = 10000 // 10A
	BoschGearModeEnable  = true
	BoschBoostModeEnable = false

	// Odometer calibration factor (as applied by unu service)
	OdometerCalibrationFactor = 1.07
)

type BoschECU struct {
	mu     sync.RWMutex
	logger *log.Logger
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
	throttleOn  bool
}

func NewBoschECU() ECUInterface {
	return &BoschECU{}
}

func (b *BoschECU) Initialize(ctx context.Context, config ECUConfig) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.logger = config.Logger
	b.bus = config.CANBus

	// Create cancellable context
	b.ctx, b.cancel = context.WithCancel(ctx)

	b.logger.Printf("Initialized Bosch ECU")
	return nil
}

func (b *BoschECU) HandleFrame(frame can.Frame) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch frame.ID {
	case BoschStatus1FrameID:
		return b.handleStatus1Frame(frame)
	case BoschStatus2FrameID:
		return b.handleStatus2Frame(frame)
	case BoschStatus3FrameID:
		return b.handleStatus3Frame(frame)
	case BoschStatus4FrameID:
		return b.handleStatus4Frame(frame)
	}

	return nil
}

func (b *BoschECU) handleStatus1Frame(frame can.Frame) error {
	if frame.Length < 8 {
		return nil
	}

	// Voltage (mV)
	b.voltage = int(binary.BigEndian.Uint16(frame.Data[0:2])) * 10

	// Current (mA)
	b.current = int(int16(binary.BigEndian.Uint16(frame.Data[2:4]))) * 10

	// RPM
	b.rpm = binary.BigEndian.Uint16(frame.Data[4:6])

	// Speed with calibration
	b.rawSpeed = uint16(frame.Data[6]) // Store raw speed
	calibratedSpeed := float64(b.rawSpeed) * CalibrationFactor * SpeedToleranceFactor
	b.speed = uint16(calibratedSpeed)

	if frame.Length >= 8 {
		b.throttleOn = (frame.Data[7] & 0x01) != 0
	} else {
		b.throttleOn = false
	}

	return nil
}

func (b *BoschECU) handleStatus2Frame(frame can.Frame) error {
	if frame.Length < 6 {
		return nil
	}

	// Temperature
	b.temperature = int8(frame.Data[0])

	// Fault code
	b.faultCode = binary.BigEndian.Uint32(frame.Data[2:6])

	return nil
}

func (b *BoschECU) handleStatus3Frame(frame can.Frame) error {
	if frame.Length < 4 {
		return nil
	}

	// Odometer (meters) - converting from 0.1km steps
	rawOdometer := binary.BigEndian.Uint32(frame.Data[0:4])
	b.odometer = uint32(float64(rawOdometer) * OdometerCalibrationFactor * 100)

	return nil
}

func (b *BoschECU) handleStatus4Frame(frame can.Frame) error {
	if frame.Length < 1 {
		return nil
	}

	// KERS status
	b.kersEnabled = (frame.Data[0] & 0x40) != 0

	return nil
}

func (b *BoschECU) SetKersEnabled(enabled bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.logger.Printf("Setting Bosch ECU KERS. boost=%v, gear=%v, kers=%v",
		BoschBoostModeEnable, BoschGearModeEnable, enabled)

	if enabled {
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

		if err := b.bus.Publish(ebsFrame); err != nil {
			return err
		}
	}

	// Send control message
	controlData := []byte{
		boolToByte(BoschGearModeEnable) |
			(boolToByte(BoschBoostModeEnable) << 1) |
			(boolToByte(enabled) << 2),
	}

	controlFrame := can.Frame{
		ID:     BoschControlMessageID,
		Length: 1,
		Data:   [8]byte{},
	}
	copy(controlFrame.Data[:], controlData)

	if err := b.bus.Publish(controlFrame); err != nil {
		return err
	}

	b.kersEnabled = enabled
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

func (b *BoschECU) Cleanup() {
	if b.cancel != nil {
		b.cancel()
	}
}
