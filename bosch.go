package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/brutella/can"
)

const (
	// Receive frame IDs
	frameStatus1             uint32 = 0x7E0
	frameStatus2             uint32 = 0x7E1
	frameStatus3             uint32 = 0x7E2
	frameStatus4             uint32 = 0x7E3
	frameGear                uint32 = 0x7E4
	frameEBSStatus           uint32 = 0x7E5
	frameStatus5             uint32 = 0x7E8 // Motor power / max speed / SW version
	frameMaxVoltage          uint32 = 0x7E9 // Over-voltage threshold (10 mV units)
	frameMinVoltage          uint32 = 0x7EA // Under-voltage threshold (10 mV units)
	frameSpeedLimit          uint32 = 0x7EB // Speed limit (%)
	frameWheelCircumference  uint32 = 0x7EC // Wheel circumference (cm)
	frameMaxPhaseCurrent     uint32 = 0x7EE // Max phase current (10 mA units)
	frameStartupPhaseCurrent uint32 = 0x7EF // Startup phase current (10 mA units)

	// Transmit frame IDs
	frameControl     uint32 = 0x4E0
	frameGearControl uint32 = 0x4E1 // Set per-gear ratio
	frameEBSSet      uint32 = 0x4E2
	frameStatusReq   uint32 = 0x4EF

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

// GearRatios are the per-gear current and torque scaling factors (0-100%)
// reported by the ECU on the Gear frame (0x7E4).
type GearRatios struct {
	HighCurrent, MidCurrent, LowCurrent uint8
	HighTorque, MidTorque, LowTorque    uint8
}

// SoftwareVersion is the decoded firmware identification block from Status5
// (0x7E8).
type SoftwareVersion struct {
	MotorRatedPowerKW uint8  // Motor rated power in kW
	MotorMaxSpeedKMH  uint8  // Motor max speed in km/h
	BaseVersion       string // e.g. "4.0"
	AppVersion        string // e.g. "12" (revision number, decimal value of the byte)
}

// ConfigReport captures the ECU's operating configuration as broadcast at
// boot or in response to a status request (0x4EF).
type ConfigReport struct {
	OverVoltageThresholdMV  uint32 // Battery voltage at which ECU cuts output
	UnderVoltageThresholdMV uint32 // Battery voltage at which ECU stops
	SpeedLimitRatio         uint8  // Speed limit (%)
	WheelCircumferenceCM    uint8  // Wheel circumference (cm)
	MaxPhaseCurrentMA       uint32 // Peak phase current (mA)
	StartupPhaseCurrentMA   uint32 // Startup phase current (mA)
	EBSVoltageMV            uint32 // Regen target voltage (mV)
	EBSCurrentMA            uint32 // Regen target current (mA)
}

type ECU struct {
	mu sync.RWMutex

	// CAN bus for sending control frames. An interface rather than *can.Bus so
	// tests can capture what would go on the wire.
	bus canPublisher

	log *Logger

	// Configuration
	gearRatioValues []uint8 // Per-gear ratio values (1-3 entries) sent via 0x4E1

	// Status fields — all protected by mu.
	voltage              int // mV
	current              int // mA (negative = regen)
	rpm                  uint16
	speed                uint16 // km/h, calibrated + averaged
	rawSpeed             uint16 // km/h, straight from frame byte
	throttleOn           bool
	brakeOn              bool
	temperature          int8
	faultCode            uint32
	odometer             uint32 // meters, calibrated
	kersECU              bool   // KERS state as reported by ECU (Status4)
	kersActive           bool   // KERS state as commanded by service
	boostEnabled         bool   // commanded boost (drives the control frame)
	boostActive          bool   // boost state the ECU acknowledges in Status4 (paired-bit decode)
	kersCurrent          uint16 // KERS regen current in mA (EBS Set frame)
	kersVoltage          uint16 // KERS regen voltage in mV (EBS Set frame)
	acceptedRegenVoltage int    // EBS regen voltage cap the ECU accepted, in mV (EBS Status frame echo)
	acceptedRegenCurrent int    // EBS regen current limit the ECU accepted, in mA (EBS Status frame echo)
	gear                 uint8
	firmwareVersion      uint32
	warrantyDate         uint32
	gearsSentOnPower     bool // whether gearRatioValues have been sent since the ECU last powered on
	// powered mirrors vehicle[engine-power] && vehicle[main-power]. The ECU is
	// only supplied in parked and ready-to-drive; frames sent while it is dark
	// are never acknowledged, and enough unacknowledged frames walk the TX error
	// counter up to 256 and latch the controller bus-off.
	powered bool

	// Status bits reported by the ECU (paired enable/disable flags, Status4)
	ecuStatusEnabled bool
	gearModeEnabled  bool

	// Per-gear current and torque ratios (0-100 %), from the Gear frame (0x7E4)
	gearRatioHighCurrent uint8
	gearRatioMidCurrent  uint8
	gearRatioLowCurrent  uint8
	gearRatioHighTorque  uint8
	gearRatioMidTorque   uint8
	gearRatioLowTorque   uint8

	// Decoded software-version components, from Status5 (0x7E8)
	motorRatedPowerKW uint8 // Motor rated power in kW
	motorMaxSpeedKMH  uint8 // Motor max speed in km/h
	swBaseVersion     uint8 // Base SW version byte (high nibble.low nibble)
	swAppVersion      uint8 // Application SW version byte (decimal revision)

	// Reported ECU configuration values (0x7E9-0x7EF)
	ovThresholdMV         uint32 // Over-voltage cut-off threshold in mV
	uvThresholdMV         uint32 // Under-voltage cut-off threshold in mV
	speedLimitRatio       uint8  // Speed limit as a percentage
	wheelCircumferenceCM  uint8  // Wheel circumference in cm
	maxPhaseCurrentMA     uint32 // Maximum phase current in mA
	startupPhaseCurrentMA uint32 // Startup phase current in mA

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

// canPublisher is the part of *can.Bus the ECU uses.
type canPublisher interface {
	Publish(can.Frame) error
}

func newECU(bus *can.Bus, log *Logger, gearRatioValues []uint8) *ECU {
	return &ECU{
		bus:             bus,
		log:             log,
		gearRatioValues: gearRatioValues,
		lastFrameTime:   time.Now(),
		kersCurrent:     DefaultKersCurrent,
		kersVoltage:     DefaultKersVoltage,
	}
}

func (b *ECU) HandleFrame(frame can.Frame) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.lastFrameTime = time.Now()
	// Hearing from the ECU is proof it has power, and is quicker than waiting
	// for the watchdog's next read of the vehicle hash. Go through the same edge
	// handling, or anything commanded while the ECU was dark stays dropped: the
	// watchdog's later SetPowered(true) would see no change and skip the re-send.
	if !b.powered {
		b.powered = true
		b.log.Info("ECU power: true (first frame)")
		b.sendKersStateLocked()
	}

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
	case frameMaxVoltage:
		b.handleMaxVoltage(frame)
	case frameMinVoltage:
		b.handleMinVoltage(frame)
	case frameSpeedLimit:
		b.handleSpeedLimit(frame)
	case frameWheelCircumference:
		b.handleWheelCircumference(frame)
	case frameMaxPhaseCurrent:
		b.handleMaxPhaseCurrent(frame)
	case frameStartupPhaseCurrent:
		b.handleStartupPhaseCurrent(frame)
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

	// Send configured gear ratios once per power-up, detected by the first
	// Status1 frame. Built inline rather than via SetGear: HandleFrame
	// already holds b.mu here, and SetGear takes it too.
	if !b.gearsSentOnPower && len(b.gearRatioValues) > 0 {
		b.gearsSentOnPower = true
		for i, ratio := range b.gearRatioValues {
			gear := uint8(i + 1)
			gearFrame := can.Frame{ID: frameGearControl, Length: 1}
			gearFrame.Data[0] = ratio
			if err := b.publish(gearFrame, "gear"); err != nil {
				b.log.Error("Failed to send gear %d on power-up: %v", gear, err)
			} else {
				b.log.Debug("Sent gear %d (ratio: %d) on ECU power-up", gear, ratio)
			}
		}
	}
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
		b.kersECU = false
	} else if byte0&0x40 != 0 { // bit 6: KERS/EBS enabled
		b.kersECU = true
	}
}

func (b *ECU) handleGear(frame can.Frame) {
	if frame.Length < 1 {
		b.log.Warn("Gear frame too short: %d bytes", frame.Length)
		return
	}
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

	b.log.Debug("Gear: %d", b.gear)
}

func (b *ECU) handleEBSStatus(frame can.Frame) {
	if frame.Length < 4 {
		return
	}
	v := binary.BigEndian.Uint16(frame.Data[0:2])
	c := binary.BigEndian.Uint16(frame.Data[2:4])
	// The EBS Status frame echoes the regen voltage/current cap the ECU
	// accepted, after its own clamping of the EBS Set command. This is the
	// stored config, not a live measurement. The echo uses the same
	// 10 mV / 10 mA per-LSB steps as the EBS Set frame, so scale by 10 to get
	// mV / mA.
	b.acceptedRegenVoltage = int(v) * 10
	b.acceptedRegenCurrent = int(c) * 10
	b.log.Debug("EBS status: voltage=%dmV current=%dmA", b.acceptedRegenVoltage, b.acceptedRegenCurrent)
}

func (b *ECU) handleStatus5(frame can.Frame) {
	if frame.Length < 8 {
		b.log.Warn("Status5 frame too short: %d bytes", frame.Length)
		return
	}
	// Status5 layout (8 bytes, big-endian):
	//   [0:4] reserved (historically warranty_date)
	//   [4]   motor rated power (BCD, kW)
	//   [5]   motor max speed   (BCD, km/h)
	//   [6]   base SW version   (BCD, high.low nibbles)
	//   [7]   app SW version    (decimal revision number)
	b.warrantyDate = binary.BigEndian.Uint32(frame.Data[0:4])
	b.firmwareVersion = binary.BigEndian.Uint32(frame.Data[4:8])

	b.motorRatedPowerKW = bcdByteToDecimal(frame.Data[4])
	b.motorMaxSpeedKMH = bcdByteToDecimal(frame.Data[5])
	b.swBaseVersion = frame.Data[6]
	b.swAppVersion = frame.Data[7]

	b.log.Info("ECU firmware 0x%08X (warranty 0x%08X) %dkW / %dkm/h / base=%s / app=%s",
		b.firmwareVersion, b.warrantyDate, b.motorRatedPowerKW, b.motorMaxSpeedKMH,
		formatBCDVersion(b.swBaseVersion), formatAppVersion(b.swAppVersion))
}

func (b *ECU) handleMaxVoltage(frame can.Frame) {
	if frame.Length < 2 {
		b.log.Warn("MaxVoltage frame too short: %d bytes", frame.Length)
		return
	}
	b.ovThresholdMV = uint32(binary.BigEndian.Uint16(frame.Data[0:2])) * 10
	b.log.Debug("ECU over-voltage threshold: %d mV", b.ovThresholdMV)
}

func (b *ECU) handleMinVoltage(frame can.Frame) {
	if frame.Length < 2 {
		b.log.Warn("MinVoltage frame too short: %d bytes", frame.Length)
		return
	}
	b.uvThresholdMV = uint32(binary.BigEndian.Uint16(frame.Data[0:2])) * 10
	b.log.Debug("ECU under-voltage threshold: %d mV", b.uvThresholdMV)
}

func (b *ECU) handleSpeedLimit(frame can.Frame) {
	if frame.Length < 1 {
		b.log.Warn("SpeedLimit frame too short: %d bytes", frame.Length)
		return
	}
	b.speedLimitRatio = frame.Data[0]
	b.log.Debug("ECU speed limit ratio: %d %%", b.speedLimitRatio)
}

func (b *ECU) handleWheelCircumference(frame can.Frame) {
	if frame.Length < 1 {
		b.log.Warn("WheelCircumference frame too short: %d bytes", frame.Length)
		return
	}
	b.wheelCircumferenceCM = frame.Data[0]
	b.log.Debug("ECU wheel circumference: %d cm", b.wheelCircumferenceCM)
}

func (b *ECU) handleMaxPhaseCurrent(frame can.Frame) {
	if frame.Length < 2 {
		b.log.Warn("MaxPhaseCurrent frame too short: %d bytes", frame.Length)
		return
	}
	b.maxPhaseCurrentMA = uint32(binary.BigEndian.Uint16(frame.Data[0:2])) * 10
	b.log.Debug("ECU max phase current: %d mA", b.maxPhaseCurrentMA)
}

func (b *ECU) handleStartupPhaseCurrent(frame can.Frame) {
	if frame.Length < 2 {
		b.log.Warn("StartupPhaseCurrent frame too short: %d bytes", frame.Length)
		return
	}
	b.startupPhaseCurrentMA = uint32(binary.BigEndian.Uint16(frame.Data[0:2])) * 10
	b.log.Debug("ECU startup phase current: %d mA", b.startupPhaseCurrentMA)
}

// bcdByteToDecimal converts a two-digit BCD byte to its decimal value.
// 0x45 -> 45. Falls back to the raw value if either nibble is out of BCD range.
func bcdByteToDecimal(v byte) uint8 {
	high := v >> 4
	low := v & 0x0F
	if high > 9 || low > 9 {
		return v
	}
	return high*10 + low
}

// formatBCDVersion renders a BCD-style version byte as "<hi>.<lo>", using
// hex digits so non-decimal nibbles survive the round-trip (some firmwares
// leak hex into the low nibble once the BCD digit space is exhausted).
func formatBCDVersion(v byte) string {
	return fmt.Sprintf("%X.%X", v>>4, v&0x0F)
}

// formatAppVersion renders the application SW version byte as its decimal
// value. The byte appears to be a revision number rather than a major.minor
// pair, so 0x0C becomes "12" rather than "0.C" or "0C".
func formatAppVersion(v byte) string {
	return fmt.Sprintf("%d", v)
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

// publish sends a frame only while the ECU has power. Callers hold b.mu.
func (b *ECU) publish(frame can.Frame, what string) error {
	if b.bus == nil {
		return nil
	}
	if !b.powered {
		b.log.Debug("ECU unpowered, dropping %s frame", what)
		return nil
	}
	return b.bus.Publish(frame)
}

// SetPowered records whether the ECU is supplied, as seen in the vehicle hash.
// Frames raised while it was dark were dropped rather than queued, so the
// commanded KERS state is re-sent on the dark-to-powered edge. Receiving a frame
// also proves power and sets this directly, which beats waiting for the next
// watchdog poll.
func (b *ECU) SetPowered(powered bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if powered == b.powered {
		return
	}
	b.powered = powered
	b.log.Info("ECU power: %v", powered)
	if powered {
		b.sendKersStateLocked()
	}
}

// sendKersStateLocked emits the frames that carry the currently commanded KERS
// state. Callers hold b.mu.
func (b *ECU) sendKersStateLocked() {
	if b.kersActive {
		ebs := can.Frame{ID: frameEBSSet, Length: 4}
		binary.BigEndian.PutUint16(ebs.Data[0:2], b.kersVoltage/10)
		binary.BigEndian.PutUint16(ebs.Data[2:4], b.kersCurrent/10)
		b.log.DebugCAN("TX", ebs.ID, ebs.Data, ebs.Length)
		if err := b.publish(ebs, "EBS Set"); err != nil {
			b.log.Error("Failed to send EBS Set frame: %v", err)
		}
	}

	ctrl := can.Frame{ID: frameControl, Length: 1}
	ctrl.Data[0] = 0x01 | // gear mode always enabled (bit 0)
		boolBit(b.boostEnabled, 1) |
		boolBit(b.kersActive, 2)
	b.log.DebugCAN("TX", ctrl.ID, ctrl.Data, ctrl.Length)
	if err := b.publish(ctrl, "Control"); err != nil {
		b.log.Error("Failed to send Control frame: %v", err)
	}
}

// SetKersEnabled commands KERS on/off. Sends Control frame (0x4E0) and, when
// enabling, the EBS Set frame (0x4E2) to configure regen voltage/current.
func (b *ECU) SetKersEnabled(enabled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.log.Info("KERS: %v (boost=%v)", enabled, b.boostEnabled)
	b.kersActive = enabled
	b.sendKersStateLocked()
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
	if err := b.publish(ctrl, "Control"); err != nil {
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
	if err := b.publish(frame, "status request"); err != nil {
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

func (b *ECU) AcceptedRegenVoltage() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.acceptedRegenVoltage
}

func (b *ECU) AcceptedRegenCurrent() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.acceptedRegenCurrent
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
	return b.boostActive
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
func (b *ECU) ECUStatusEnabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ecuStatusEnabled
}
func (b *ECU) BoostActive() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.boostActive
}
func (b *ECU) GearModeEnabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.gearModeEnabled
}
func (b *ECU) GearRatios() GearRatios {
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
func (b *ECU) SoftwareVersion() SoftwareVersion {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return SoftwareVersion{
		MotorRatedPowerKW: b.motorRatedPowerKW,
		MotorMaxSpeedKMH:  b.motorMaxSpeedKMH,
		BaseVersion:       formatBCDVersion(b.swBaseVersion),
		AppVersion:        formatAppVersion(b.swAppVersion),
	}
}
func (b *ECU) ConfigReport() ConfigReport {
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

// SetGear sends the configured ratio for the given gear (1-based) to the ECU
// via the 0x4E1 control message. Not called while handling an incoming CAN
// frame — HandleFrame already holds b.mu, and this method takes it too.
func (b *ECU) SetGear(gear uint8) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.gearRatioValues) == 0 {
		return fmt.Errorf("no gear ratios configured")
	}

	if gear < 1 || gear > uint8(len(b.gearRatioValues)) {
		return fmt.Errorf("invalid gear %d: must be between 1 and %d", gear, len(b.gearRatioValues))
	}

	ratio := b.gearRatioValues[gear-1]

	b.log.Debug("Setting Bosch ECU gear %d (ratio: %d)", gear, ratio)

	gearFrame := can.Frame{ID: frameGearControl, Length: 1}
	gearFrame.Data[0] = ratio

	return b.publish(gearFrame, "gear")
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
