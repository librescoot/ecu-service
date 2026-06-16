package main

import (
	"encoding/binary"
	"math"
	"sync"
	"time"

	"github.com/brutella/can"
)

const (
	// Receive frame IDs
	frameStatus1   uint32 = 0x7E0
	frameStatus2   uint32 = 0x7E1
	frameStatus3   uint32 = 0x7E2
	frameStatus4   uint32 = 0x7E3
	frameGear      uint32 = 0x7E4
	frameEBSStatus uint32 = 0x7E5
	frameStatus5   uint32 = 0x7E8

	// Transmit frame IDs
	frameControl   uint32 = 0x4E0
	frameEBSSet    uint32 = 0x4E2
	frameStatusReq uint32 = 0x4EF

	// CalibrationFactor corrects for odometer vs actual distance (empirically derived).
	CalibrationFactor = 1.03
	// SpeedToleranceFactor corrects raw ECU speed against GPS measurements (empirically derived).
	SpeedToleranceFactor = 1.155556

	// odometerCalibration is applied to the raw odometer reading (empirically derived).
	odometerCalibration = 1.07

	// speedWindowSize is the number of samples in the moving average.
	speedWindowSize = 3

	// staleTimeout is the maximum age of the last received CAN frame before data
	// is considered stale.
	staleTimeout = 2 * time.Second

	// maxPowerDeltaSeconds prevents energy integration across gaps (e.g. ECU off).
	maxPowerDeltaSeconds = 2.0

	// EBS regen defaults, configurable via Redis settings. Stored in mV / mA;
	// the EBS Set frame (0x4E2) encodes them as 10 mV / 10 mA per LSB.
	DefaultKersVoltage uint16 = 56000 // 56 V
	DefaultKersCurrent uint16 = 10000 // 10 A
	MinKersVoltage     uint16 = 42000 // 42 V
	MaxKersVoltage     uint16 = 58000 // 58 V
)

type speedBuffer struct {
	data  [speedWindowSize]uint16
	head  uint8
	count uint8
	sum   uint16
}

func (b *speedBuffer) reset() {
	*b = speedBuffer{}
}

func (b *speedBuffer) movingAverage(sample uint16) float64 {
	var evicted uint16
	if b.count >= speedWindowSize {
		b.count = speedWindowSize
		evicted = b.data[b.head]
	} else {
		b.count++
	}
	b.data[b.head] = sample
	b.sum = b.sum - evicted + sample
	b.head = (b.head + 1) % speedWindowSize
	return float64(b.sum) / float64(b.count)
}

type ECU struct {
	mu sync.RWMutex

	// CAN bus for sending control frames.
	bus *can.Bus

	log *Logger

	// Status fields — all protected by mu.
	voltage         int // mV
	current         int // mA (negative = regen)
	rpm             uint16
	speed           uint16 // km/h, calibrated + averaged
	rawSpeed        uint16 // km/h, straight from frame byte
	throttleOn      bool
	brakeOn         bool
	temperature     int8
	faultCode       uint32
	odometer        uint32 // meters, calibrated
	kersECU         bool   // KERS state as reported by ECU (Status4)
	kersActive      bool   // KERS state as commanded by service
	boostEnabled    bool   // commanded boost (drives the control frame)
	boostReported   bool   // boost state the ECU acknowledges in Status4
	kersCurrent     uint16 // KERS regen current in mA (EBS Set frame)
	kersVoltage     uint16 // KERS regen voltage in mV (EBS Set frame)
	gear            uint8
	firmwareVersion uint32
	warrantyDate    uint32

	// Energy accounting.
	energyConsumed      uint64  // mWh consumed
	energyRecovered     uint64  // mWh recovered (regen)
	energyConsumedFrac  float64 // sub-mWh remainder carried across frames
	energyRecoveredFrac float64
	lastPowerUpdate     time.Time

	// Stale-frame detection.
	lastFrameTime time.Time

	speedBuf speedBuffer
}

func newECU(bus *can.Bus, log *Logger) *ECU {
	return &ECU{
		bus:           bus,
		log:           log,
		lastFrameTime: time.Now(),
		kersCurrent:   DefaultKersCurrent,
		kersVoltage:   DefaultKersVoltage,
	}
}

func (b *ECU) HandleFrame(frame can.Frame) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.lastFrameTime = time.Now()

	switch frame.ID {
	case frameStatus1:
		b.handleStatus1(frame)
	case frameStatus2:
		b.handleStatus2(frame)
	case frameStatus3:
		b.handleStatus3(frame)
	case frameStatus4:
		b.handleStatus4(frame)
	case frameGear:
		b.handleGear(frame)
	case frameEBSStatus:
		b.handleEBSStatus(frame)
	case frameStatus5:
		b.handleStatus5(frame)
	}
}

func (b *ECU) handleStatus1(frame can.Frame) {
	if frame.Length < 8 {
		b.log.Warn("Status1 frame too short: %d bytes", frame.Length)
		return
	}

	b.voltage = int(binary.BigEndian.Uint16(frame.Data[0:2])) * 10
	b.current = int(int16(binary.BigEndian.Uint16(frame.Data[2:4]))) * 10
	b.rpm = binary.BigEndian.Uint16(frame.Data[4:6])
	b.rawSpeed = uint16(frame.Data[6])
	b.speed = b.calibratedSpeed(b.rawSpeed)
	b.throttleOn = frame.Data[7]&0x01 != 0
	b.brakeOn = frame.Data[7]&0x02 != 0

	b.updatePower()
}

func (b *ECU) handleStatus2(frame can.Frame) {
	if frame.Length < 6 {
		b.log.Warn("Status2 frame too short: %d bytes", frame.Length)
		return
	}

	b.temperature = int8(frame.Data[0])

	code := binary.BigEndian.Uint32(frame.Data[2:6])
	// Filter spurious fault code 15 (software brake in parking mode).
	if code == 15 {
		code = 0
	}
	b.faultCode = code
}

func (b *ECU) handleStatus3(frame can.Frame) {
	if frame.Length < 4 {
		b.log.Warn("Status3 frame too short: %d bytes", frame.Length)
		return
	}

	raw := binary.BigEndian.Uint32(frame.Data[0:4])
	// Raw unit is 0.1 km; convert to meters with calibration factor.
	b.odometer = uint32(float64(raw) * odometerCalibration * 100)
}

func (b *ECU) handleStatus4(frame can.Frame) {
	if frame.Length < 1 {
		b.log.Warn("Status4 frame too short: %d bytes", frame.Length)
		return
	}
	// ebs_enabled (bit 6) and boost_mode_enabled (bit 2) as acknowledged by the ECU.
	b.kersECU = frame.Data[0]&0x40 != 0
	b.boostReported = frame.Data[0]&0x04 != 0
}

func (b *ECU) handleGear(frame can.Frame) {
	if frame.Length < 1 {
		b.log.Warn("Gear frame too short: %d bytes", frame.Length)
		return
	}
	b.gear = frame.Data[0]
	b.log.Debug("Gear: %d", b.gear)
}

func (b *ECU) handleEBSStatus(frame can.Frame) {
	if frame.Length < 4 {
		return
	}
	v := binary.BigEndian.Uint16(frame.Data[0:2])
	c := binary.BigEndian.Uint16(frame.Data[2:4])
	b.log.Debug("EBS status: voltage=%dmV current=%dmA", int(v)*10, int(c)*10)
}

func (b *ECU) handleStatus5(frame can.Frame) {
	if frame.Length < 8 {
		b.log.Warn("Status5 frame too short: %d bytes", frame.Length)
		return
	}
	b.warrantyDate = binary.BigEndian.Uint32(frame.Data[0:4])
	b.firmwareVersion = binary.BigEndian.Uint32(frame.Data[4:8])
	b.log.Info("ECU firmware 0x%08X (warranty 0x%08X)", b.firmwareVersion, b.warrantyDate)
}

func (b *ECU) calibratedSpeed(raw uint16) uint16 {
	if raw == 0 {
		b.speedBuf.reset()
		return 0
	}
	avg := b.speedBuf.movingAverage(raw)
	return uint16(math.Round(avg * CalibrationFactor * SpeedToleranceFactor))
}

func (b *ECU) updatePower() {
	now := time.Now()
	if b.lastPowerUpdate.IsZero() {
		b.lastPowerUpdate = now
		return
	}
	dt := now.Sub(b.lastPowerUpdate).Seconds()
	b.lastPowerUpdate = now
	if dt > maxPowerDeltaSeconds {
		return
	}

	// power in mW = (mV × mA) / 1000
	powerMW := int64(b.voltage) * int64(b.current) / 1000
	delta := float64(powerMW) * dt / 3600.0
	// Carry the sub-mWh remainder across frames so the per-frame truncation
	// doesn't systematically undercount (at ~10 Hz, up to ~1 mWh/frame would
	// otherwise be dropped).
	if delta > 0 {
		b.energyConsumedFrac += delta
		whole := uint64(b.energyConsumedFrac)
		b.energyConsumed += whole
		b.energyConsumedFrac -= float64(whole)
	} else {
		b.energyRecoveredFrac += -delta
		whole := uint64(b.energyRecoveredFrac)
		b.energyRecovered += whole
		b.energyRecoveredFrac -= float64(whole)
	}
}

// SetKersEnabled commands KERS on/off. Sends Control frame (0x4E0) and, when
// enabling, the EBS Set frame (0x4E2) to configure regen voltage/current.
func (b *ECU) SetKersEnabled(enabled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.log.Info("KERS → %v (boost=%v)", enabled, b.boostEnabled)

	if enabled {
		ebs := can.Frame{ID: frameEBSSet, Length: 4}
		binary.BigEndian.PutUint16(ebs.Data[0:2], b.kersVoltage/10)
		binary.BigEndian.PutUint16(ebs.Data[2:4], b.kersCurrent/10)
		b.log.DebugCAN("TX", ebs.ID, ebs.Data, ebs.Length)
		if err := b.bus.Publish(ebs); err != nil {
			b.log.Error("Failed to send EBS Set frame: %v", err)
		}
	}

	ctrl := can.Frame{ID: frameControl, Length: 1}
	ctrl.Data[0] = 0x01 | // gear mode always enabled (bit 0)
		boolBit(b.boostEnabled, 1) |
		boolBit(enabled, 2)
	b.log.DebugCAN("TX", ctrl.ID, ctrl.Data, ctrl.Length)
	if err := b.bus.Publish(ctrl); err != nil {
		b.log.Error("Failed to send Control frame: %v", err)
	}

	b.kersActive = enabled
}

// SetBoostEnabled updates the boost flag and sends an updated Control frame.
func (b *ECU) SetBoostEnabled(enabled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.boostEnabled = enabled
	b.log.Info("Boost → %v", enabled)

	ctrl := can.Frame{ID: frameControl, Length: 1}
	ctrl.Data[0] = 0x01 |
		boolBit(enabled, 1) |
		boolBit(b.kersActive, 2)
	b.log.DebugCAN("TX", ctrl.ID, ctrl.Data, ctrl.Length)
	if err := b.bus.Publish(ctrl); err != nil {
		b.log.Error("Failed to send Control frame: %v", err)
	}
}

// SetKersCurrent sets the KERS regen current (mA) used in the EBS Set frame on
// the next KERS enable.
func (b *ECU) SetKersCurrent(mA uint16) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if mA == b.kersCurrent {
		return // suppress redundant work/logs on frequent battery updates
	}
	b.kersCurrent = mA
	b.log.Info("KERS current set to %d mA", mA)
}

// SetKersVoltage sets the KERS regen voltage (mV), clamped to the safe range,
// used in the EBS Set frame on the next KERS enable.
func (b *ECU) SetKersVoltage(mV uint16) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if mV < MinKersVoltage || mV > MaxKersVoltage {
		b.log.Warn("KERS voltage %d mV out of range [%d, %d] — ignoring", mV, MinKersVoltage, MaxKersVoltage)
		return
	}
	if mV == b.kersVoltage {
		return // suppress redundant work/logs
	}
	b.kersVoltage = mV
	b.log.Info("KERS voltage set to %d mV", mV)
}

// KersECUEnabled returns the KERS state as reported by the ECU in Status4.
func (b *ECU) KersECUEnabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.kersECU
}

// RequestStatus sends the status-request frame (0x4EF) to trigger the ECU to
// send all status frames.
func (b *ECU) RequestStatus() {
	b.mu.Lock()
	defer b.mu.Unlock()

	frame := can.Frame{ID: frameStatusReq, Length: 0}
	b.log.DebugCAN("TX", frame.ID, frame.Data, frame.Length)
	if err := b.bus.Publish(frame); err != nil {
		b.log.Error("Failed to send status request: %v", err)
	}
}

// UpdateBus replaces the CAN bus reference after a reconnect and resets the
// stale-frame clock so control frames go out on the fresh socket.
func (b *ECU) UpdateBus(bus *can.Bus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bus = bus
	b.lastFrameTime = time.Now()
}

// IsStale returns true if no CAN frame has been received within staleTimeout.
func (b *ECU) IsStale() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return time.Since(b.lastFrameTime) > staleTimeout
}

// TimeSinceLastFrame returns how long ago the last CAN frame was received.
func (b *ECU) TimeSinceLastFrame() time.Duration {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return time.Since(b.lastFrameTime)
}

// Getters — all thread-safe.

func (b *ECU) Voltage() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.voltage
}
func (b *ECU) Current() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.current
}
func (b *ECU) RPM() uint16 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.rpm
}
func (b *ECU) Speed() uint16 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.speed
}
func (b *ECU) RawSpeed() uint16 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.rawSpeed
}
func (b *ECU) ThrottleOn() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.throttleOn
}
func (b *ECU) BrakeOn() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.brakeOn
}
func (b *ECU) Temperature() int8 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.temperature
}
func (b *ECU) FaultCode() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.faultCode
}
func (b *ECU) Odometer() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.odometer
}
func (b *ECU) KersActive() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.kersActive
}
func (b *ECU) BoostEnabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.boostReported
}
func (b *ECU) Gear() uint8 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.gear
}
func (b *ECU) FirmwareVersion() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.firmwareVersion
}
func (b *ECU) WarrantyDate() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.warrantyDate
}
func (b *ECU) Power() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.voltage * b.current / 1000 // mW
}
func (b *ECU) EnergyConsumed() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.energyConsumed
}
func (b *ECU) EnergyRecovered() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.energyRecovered
}

func boolBit(v bool, shift uint) byte {
	if v {
		return 1 << shift
	}
	return 0
}
