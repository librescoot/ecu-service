package main

import (
	"bytes"
	"encoding/binary"
	"log"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/brutella/can"
)

// makeFrame builds a CAN frame with the given ID and data bytes.
func makeFrame(id uint32, data []byte) can.Frame {
	f := can.Frame{ID: id, Length: uint8(len(data))}
	copy(f.Data[:], data)
	return f
}

// newTestECU returns a ECU suitable for unit testing (no CAN bus needed).
func newTestECU() *ECU {
	return &ECU{log: newLogger(LogLevelNone)}
}

// --- SpeedBuffer ---

func TestSpeedBuffer_SingleValue(t *testing.T) {
	var buf speedBuffer
	avg := buf.movingAverage(100)
	if avg != 100.0 {
		t.Errorf("expected 100.0, got %f", avg)
	}
}

func TestSpeedBuffer_WindowFill(t *testing.T) {
	var buf speedBuffer
	buf.movingAverage(100)
	buf.movingAverage(200)
	avg := buf.movingAverage(300)
	// (100+200+300)/3 = 200
	if avg != 200.0 {
		t.Errorf("expected 200.0, got %f", avg)
	}
}

func TestSpeedBuffer_WindowSlide(t *testing.T) {
	var buf speedBuffer
	buf.movingAverage(100)
	buf.movingAverage(200)
	buf.movingAverage(300)
	avg := buf.movingAverage(400) // evicts 100 → (200+300+400)/3
	if avg != 300.0 {
		t.Errorf("expected 300.0, got %f", avg)
	}
}

func TestSpeedBuffer_Reset(t *testing.T) {
	var buf speedBuffer
	buf.movingAverage(100)
	buf.movingAverage(200)
	buf.reset()
	avg := buf.movingAverage(50)
	if avg != 50.0 {
		t.Errorf("expected 50.0 after reset, got %f", avg)
	}
}

// --- Status1 (0x7E0) ---

func TestStatus1_Voltage(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 8)
	binary.BigEndian.PutUint16(data[0:2], 4800) // raw 4800 → 48000 mV
	ecu.HandleFrame(makeFrame(frameStatus1, data))
	if ecu.Voltage() != 48000 {
		t.Errorf("voltage: expected 48000, got %d", ecu.Voltage())
	}
}

func TestStatus1_Current(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 8)
	binary.BigEndian.PutUint16(data[2:4], 500) // 500 → 5000 mA
	ecu.HandleFrame(makeFrame(frameStatus1, data))
	if ecu.Current() != 5000 {
		t.Errorf("current: expected 5000, got %d", ecu.Current())
	}
}

func TestStatus1_NegativeCurrent(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 8)
	neg200 := int16(-200)
	binary.BigEndian.PutUint16(data[2:4], uint16(neg200))
	ecu.HandleFrame(makeFrame(frameStatus1, data))
	if ecu.Current() != -2000 {
		t.Errorf("current: expected -2000, got %d", ecu.Current())
	}
}

func TestStatus1_RPM(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 8)
	binary.BigEndian.PutUint16(data[4:6], 3000)
	ecu.HandleFrame(makeFrame(frameStatus1, data))
	if ecu.RPM() != 3000 {
		t.Errorf("RPM: expected 3000, got %d", ecu.RPM())
	}
}

func TestStatus1_RawSpeed(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 8)
	data[6] = 45
	ecu.HandleFrame(makeFrame(frameStatus1, data))
	if ecu.RawSpeed() != 45 {
		t.Errorf("raw speed: expected 45, got %d", ecu.RawSpeed())
	}
}

func TestStatus1_SpeedCalibrated(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 8)
	data[6] = 100
	ecu.HandleFrame(makeFrame(frameStatus1, data))
	// Single sample: 100 * 1.03 * 1.155556 ≈ 119, rounded to nearest.
	raw := float64(100) * CalibrationFactor * SpeedToleranceFactor
	expected := uint16(math.Round(raw))
	if ecu.Speed() != expected {
		t.Errorf("speed: expected %d, got %d", expected, ecu.Speed())
	}
}

func TestStatus1_ThrottleOn(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 8)
	data[7] = 0x01
	ecu.HandleFrame(makeFrame(frameStatus1, data))
	if !ecu.ThrottleOn() {
		t.Error("expected throttle on")
	}
	if ecu.BrakeOn() {
		t.Error("expected brake off")
	}
}

func TestStatus1_BrakeOn(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 8)
	data[7] = 0x02
	ecu.HandleFrame(makeFrame(frameStatus1, data))
	if ecu.ThrottleOn() {
		t.Error("expected throttle off")
	}
	if !ecu.BrakeOn() {
		t.Error("expected brake on")
	}
}

func TestStatus1_BothThrottleAndBrake(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 8)
	data[7] = 0x03
	ecu.HandleFrame(makeFrame(frameStatus1, data))
	if !ecu.ThrottleOn() || !ecu.BrakeOn() {
		t.Error("expected both throttle and brake on")
	}
}

func TestStatus1_ZeroValues(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 8)
	ecu.HandleFrame(makeFrame(frameStatus1, data))
	if ecu.Voltage() != 0 || ecu.Current() != 0 || ecu.RPM() != 0 || ecu.Speed() != 0 {
		t.Error("all zero values expected for zero frame")
	}
}

func TestStatus1_ShortFrame(t *testing.T) {
	ecu := newTestECU()
	ecu.HandleFrame(makeFrame(frameStatus1, make([]byte, 4)))
	// Short frame: no panic, values remain zero.
	if ecu.Voltage() != 0 {
		t.Error("expected zero voltage after short frame")
	}
}

// --- Status2 (0x7E1) ---

func TestStatus2_Temperature(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 6)
	data[0] = 0x2D // 45°C
	ecu.HandleFrame(makeFrame(frameStatus2, data))
	if ecu.Temperature() != 45 {
		t.Errorf("temperature: expected 45, got %d", ecu.Temperature())
	}
}

func TestStatus2_FaultCode(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 6)
	binary.BigEndian.PutUint32(data[2:6], 0x03)
	ecu.HandleFrame(makeFrame(frameStatus2, data))
	if ecu.FaultCode() != 3 {
		t.Errorf("fault code: expected 3, got %d", ecu.FaultCode())
	}
}

func TestStatus2_SpuriousFault15Filtered(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 6)
	binary.BigEndian.PutUint32(data[2:6], 15)
	ecu.HandleFrame(makeFrame(frameStatus2, data))
	if ecu.FaultCode() != 0 {
		t.Errorf("fault 15 should be filtered to 0, got %d", ecu.FaultCode())
	}
}

func TestStatus2_NonZeroFaultMappedCorrectly(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 6)
	binary.BigEndian.PutUint32(data[2:6], 0x01)
	ecu.HandleFrame(makeFrame(frameStatus2, data))
	fault, cfg := MapFault(ecu.FaultCode())
	if fault != FaultBatteryOverVoltage {
		t.Errorf("expected FaultBatteryOverVoltage, got %d", fault)
	}
	if cfg.Severity != "critical" {
		t.Errorf("expected critical severity, got %s", cfg.Severity)
	}
}

// --- Status3 (0x7E2) ---

func TestStatus3_Odometer(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data[0:4], 1000) // 1000 × 0.1 km = 100 km
	ecu.HandleFrame(makeFrame(frameStatus3, data))
	expected := uint32(float64(1000) * odometerCalibration * 100)
	if ecu.Odometer() != expected {
		t.Errorf("odometer: expected %d, got %d", expected, ecu.Odometer())
	}
}

// --- Status4 (0x7E3) ---

func TestStatus4_KERSEnabled(t *testing.T) {
	ecu := newTestECU()
	ecu.HandleFrame(makeFrame(frameStatus4, []byte{0x40})) // bit 6
	if !ecu.kersECU {
		t.Error("expected KERS enabled from ECU")
	}
}

func TestStatus4_KERSDisabled(t *testing.T) {
	ecu := newTestECU()
	ecu.HandleFrame(makeFrame(frameStatus4, []byte{0x00}))
	if ecu.kersECU {
		t.Error("expected KERS disabled from ECU")
	}
}

func TestStatus4_BoostReported(t *testing.T) {
	ecu := newTestECU()
	ecu.HandleFrame(makeFrame(frameStatus4, []byte{0x04})) // bit 2: boost enabled
	if !ecu.BoostEnabled() {
		t.Error("expected boost reported from ECU")
	}
	// Byte 0 is a paired enable/disable register: neither bit set leaves the
	// previous state latched, so clearing boost requires the disable bit.
	ecu.HandleFrame(makeFrame(frameStatus4, []byte{0x00}))
	if !ecu.BoostEnabled() {
		t.Error("expected boost to stay latched when neither enable nor disable bit is set")
	}
	ecu.HandleFrame(makeFrame(frameStatus4, []byte{0x08})) // bit 3: boost disabled
	if ecu.BoostEnabled() {
		t.Error("expected boost cleared from ECU")
	}
}

// --- Gear (0x7E4) ---

func TestGear_Values(t *testing.T) {
	for _, g := range []byte{1, 2, 3} {
		ecu := newTestECU()
		ecu.HandleFrame(makeFrame(frameGear, []byte{g}))
		if ecu.Gear() != g {
			t.Errorf("gear: expected %d, got %d", g, ecu.Gear())
		}
	}
}

func TestGear_WithRatios(t *testing.T) {
	ecu := newTestECU()
	// gear=1, then high/mid/low current ratios, high/mid/low torque ratios
	data := []byte{1, 100, 75, 50, 90, 65, 40}
	ecu.HandleFrame(makeFrame(frameGear, data))

	got := ecu.GearRatios()
	want := GearRatios{HighCurrent: 100, MidCurrent: 75, LowCurrent: 50, HighTorque: 90, MidTorque: 65, LowTorque: 40}
	if got != want {
		t.Errorf("ratios: expected %+v, got %+v", want, got)
	}
}

func TestGear_ShortFrameLeavesRatiosUnset(t *testing.T) {
	ecu := newTestECU()
	ecu.HandleFrame(makeFrame(frameGear, []byte{2}))
	if got := ecu.GearRatios(); got != (GearRatios{}) {
		t.Errorf("ratios: expected zero value on short frame, got %+v", got)
	}
}

// --- Status4 (0x7E3), paired-bit decode ---

func TestStatus4_AllBits(t *testing.T) {
	// 0x9A = 0b10011010: bit1 (ECU disabled), bit3 (boost disabled),
	// bit4 (gear mode enabled), bit7 (KERS disabled).
	ecu := newTestECU()
	ecu.HandleFrame(makeFrame(frameStatus4, []byte{0x9A}))

	if ecu.ECUStatusEnabled() {
		t.Error("ECU should be reported disabled")
	}
	if ecu.BoostActive() {
		t.Error("boost should be reported disabled")
	}
	if !ecu.GearModeEnabled() {
		t.Error("gear mode should be reported enabled")
	}
	if ecu.KersECUEnabled() {
		t.Error("KERS should be reported disabled")
	}
}

func TestStatus4_DisableBitWins(t *testing.T) {
	// Both bits set in a pair: the disable bit must take precedence so we
	// never falsely report a feature as on.
	ecu := newTestECU()
	ecu.HandleFrame(makeFrame(frameStatus4, []byte{0x03})) // ECU enabled + disabled
	if ecu.ECUStatusEnabled() {
		t.Error("disable bit should win over enable bit")
	}
}

// --- Status5 (0x7E8) ---

func TestStatus5_FirmwareAndWarranty(t *testing.T) {
	ecu := newTestECU()
	data := make([]byte, 8)
	binary.BigEndian.PutUint32(data[0:4], 0x20240115)
	binary.BigEndian.PutUint32(data[4:8], 0xDEADBEEF)
	ecu.HandleFrame(makeFrame(frameStatus5, data))
	if ecu.FirmwareVersion() != 0xDEADBEEF {
		t.Errorf("firmware: expected 0xDEADBEEF, got 0x%X", ecu.FirmwareVersion())
	}
	if ecu.WarrantyDate() != 0x20240115 {
		t.Errorf("warranty: expected 0x20240115, got 0x%X", ecu.WarrantyDate())
	}
}

func TestStatus5_ShortFrameIgnored(t *testing.T) {
	ecu := newTestECU()
	ecu.HandleFrame(makeFrame(frameStatus5, make([]byte, 4)))
	if ecu.FirmwareVersion() != 0 {
		t.Errorf("firmware should be 0 after short frame, got 0x%X", ecu.FirmwareVersion())
	}
}

func TestStatus5_SoftwareVersion(t *testing.T) {
	ecu := newTestECU()
	// reserved=0, motor=4kW (0x04), max=45km/h (0x45), base=v4.0 (0x40), app rev 12 (0x0C)
	data := []byte{0x00, 0x00, 0x00, 0x00, 0x04, 0x45, 0x40, 0x0C}
	ecu.HandleFrame(makeFrame(frameStatus5, data))

	got := ecu.SoftwareVersion()
	want := SoftwareVersion{MotorRatedPowerKW: 4, MotorMaxSpeedKMH: 45, BaseVersion: "4.0", AppVersion: "12"}
	if got != want {
		t.Errorf("software version: expected %+v, got %+v", want, got)
	}
}

func TestBCDByteToDecimal(t *testing.T) {
	tests := []struct {
		in   byte
		want uint8
	}{
		{0x04, 4},
		{0x45, 45},
		{0x99, 99},
		{0x00, 0},
		{0xAB, 0xAB}, // non-BCD digits: passthrough
	}
	for _, tt := range tests {
		if got := bcdByteToDecimal(tt.in); got != tt.want {
			t.Errorf("bcdByteToDecimal(0x%02X): expected %d, got %d", tt.in, tt.want, got)
		}
	}
}

// --- Config frames (0x7E9-0x7EF) ---

func TestConfigFrames(t *testing.T) {
	ecu := newTestECU()

	ecu.HandleFrame(makeFrame(frameMaxVoltage, []byte{0x1B, 0x75}))          // 0x1B75 * 10 = 70290 mV
	ecu.HandleFrame(makeFrame(frameMinVoltage, []byte{0x0F, 0x99}))          // 0x0F99 * 10 = 39930 mV
	ecu.HandleFrame(makeFrame(frameSpeedLimit, []byte{0x64}))                // 100 %
	ecu.HandleFrame(makeFrame(frameWheelCircumference, []byte{0x7E}))        // 126 cm
	ecu.HandleFrame(makeFrame(frameMaxPhaseCurrent, []byte{0x52, 0x08}))     // 0x5208 * 10 = 210000 mA
	ecu.HandleFrame(makeFrame(frameStartupPhaseCurrent, []byte{0x03, 0xE8})) // 0x03E8 * 10 = 10000 mA

	got := ecu.ConfigReport()
	want := ConfigReport{
		OverVoltageThresholdMV:  70290,
		UnderVoltageThresholdMV: 39930,
		SpeedLimitRatio:         100,
		WheelCircumferenceCM:    126,
		MaxPhaseCurrentMA:       210000,
		StartupPhaseCurrentMA:   10000,
	}
	if got != want {
		t.Errorf("config report: expected %+v, got %+v", want, got)
	}
}

func TestEBSStatus_MirrorsAcceptedSettings(t *testing.T) {
	ecu := newTestECU()
	// 56 V / 10 A in 10-unit increments
	data := []byte{0x15, 0xE0, 0x03, 0xE8}
	ecu.HandleFrame(makeFrame(frameEBSStatus, data))

	got := ecu.ConfigReport()
	if got.EBSVoltageMV != 56000 {
		t.Errorf("EBS voltage: expected 56000, got %d", got.EBSVoltageMV)
	}
	if got.EBSCurrentMA != 10000 {
		t.Errorf("EBS current: expected 10000, got %d", got.EBSCurrentMA)
	}
}

// --- MapFault ---

func TestMapFault_AllCodes(t *testing.T) {
	cases := []struct {
		code uint32
		want Fault
	}{
		{0x01, FaultBatteryOverVoltage},
		{0x02, FaultBatteryUnderVoltage},
		{0x03, FaultMotorShortCircuit},
		{0x04, FaultMotorStalled},
		{0x05, FaultHallSensorAbnormal},
		{0x06, FaultMOSFETCheckError},
		{0x07, FaultMotorOpenCircuit},
		{0x0A, FaultPowerOnSelfCheckError},
		{0x0B, FaultOverTemperature},
		{0x0C, FaultThrottleAbnormal},
		{0x0D, FaultMotorTempProtection},
		{0x0E, FaultThrottleActiveAtPowerUp},
		{0x10, FaultInternal15vAbnormal},
	}
	for _, c := range cases {
		f, _ := MapFault(c.code)
		if f != c.want {
			t.Errorf("MapFault(0x%X): expected %d, got %d", c.code, c.want, f)
		}
	}
}

// TestMapFault_Unknown pins the behaviour that an unrecognised code is exposed
// rather than swallowed. Returning FaultNone here sent ReportFault down its
// clear path, which deletes the fault set and emits a code-0 all-clear: a
// controller reporting something we had never seen was announced downstream as
// a healthy vehicle. 0x08, 0x09 and 0x0F all sat in that hole.
func TestMapFault_Unknown(t *testing.T) {
	f, cfg := MapFault(0xFF)
	if f != Fault(0xFF) {
		t.Errorf("unknown code should be surfaced under its own number, got %d", f)
	}
	if !cfg.Unknown {
		t.Error("unknown code should be flagged Unknown so callers can log it")
	}
	if cfg.Description == "" {
		t.Error("unknown code should carry a description, not an empty string")
	}
}

// TestHandleStatus2_BrakeAppliedIsSuppressedButLogged pins the E15 handling.
// The vehicle asserts the engine brake itself in every state except drive, so
// the controller reporting it back is expected. It must not reach fault:code,
// because the dashboard toasts any non-zero code and the rider would get a
// warning every time they park. It must still be logged, so the frequency can
// be counted from field logs rather than erased.
func TestHandleStatus2_BrakeAppliedIsSuppressedButLogged(t *testing.T) {
	var buf bytes.Buffer
	ecu, _ := newGatedECU()
	ecu.log = &Logger{l: log.New(&buf, "", 0), level: LogLevelInfo}

	brakeApplied := makeFrame(frameStatus2, []byte{20, 0, 0, 0, 0, faultCodeBrakeApplied, 0, 0})
	for i := 0; i < 4; i++ {
		ecu.HandleFrame(brakeApplied)
	}
	if ecu.faultCode != 0 {
		t.Fatalf("brake-applied code reached fault:code as %d, want 0", ecu.faultCode)
	}
	if n := strings.Count(buf.String(), "brake applied"); n != 1 {
		t.Errorf("four brake-applied frames logged %d times, want 1", n)
	}

	// A real fault in the same field must still come through untouched.
	realFault := makeFrame(frameStatus2, []byte{20, 0, 0, 0, 0, 0x0B, 0, 0})
	ecu.HandleFrame(realFault)
	if ecu.faultCode != 0x0B {
		t.Errorf("real fault code came through as %d, want 11", ecu.faultCode)
	}
	if !strings.Contains(buf.String(), "no longer reports brake applied") {
		t.Error("leaving the brake-applied state should be logged too")
	}
}

func TestMapFault_Zero(t *testing.T) {
	f, _ := MapFault(0)
	if f != FaultNone {
		t.Errorf("zero should map to FaultNone, got %d", f)
	}
}

// --- ECU power gating ---
//
// The ECU is only supplied in parked and ready-to-drive. Frames sent while it is
// dark are never acknowledged, and enough of them latch the CAN controller
// bus-off, so nothing may go on the wire until power is confirmed.

// fakeBus records the frames the ECU tries to send.
type fakeBus struct {
	sent []can.Frame
	err  error
}

func (f *fakeBus) Publish(frame can.Frame) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, frame)
	return nil
}

func (f *fakeBus) ids() []uint32 {
	out := make([]uint32, len(f.sent))
	for i, fr := range f.sent {
		out[i] = fr.ID
	}
	return out
}

// powerUpAndAssert powers the ECU and advances past the assertion hold, then
// makes the blind attempt the watchdog tick would make. The hold means the
// power edge no longer transmits by itself, so tests that care about the
// re-applied state have to reach the tick that follows it.
func powerUpAndAssert(ecu *ECU) {
	ecu.SetPowered(true)
	ecu.mu.Lock()
	ecu.powerOnAt = time.Now().Add(-ecuAssertHoldAfterPowerOn)
	ecu.mu.Unlock()
	ecu.ApplyCommandedState()
}

// newGatedECU returns an ECU wired to a fakeBus, unpowered.
func newGatedECU() (*ECU, *fakeBus) {
	bus := &fakeBus{}
	return &ECU{log: newLogger(LogLevelNone), bus: bus, kersVoltage: DefaultKersVoltage}, bus
}

func TestPublish_DropsFrameWhileUnpowered(t *testing.T) {
	ecu, bus := newGatedECU()

	ecu.RequestStatus()

	if len(bus.sent) != 0 {
		t.Fatalf("unpowered ECU sent %d frame(s): %#x", len(bus.sent), bus.ids())
	}
}

func TestPublish_SendsFrameWhenPowered(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.SetPowered(true)
	bus.sent = nil // discard the power-up KERS re-apply

	ecu.RequestStatus()

	if len(bus.sent) != 1 || bus.sent[0].ID != frameStatusReq {
		t.Fatalf("expected one 0x%X frame, got %#x", frameStatusReq, bus.ids())
	}
}

func TestSetKersEnabled_DroppedWhileUnpoweredThenReappliedOnPowerUp(t *testing.T) {
	ecu, bus := newGatedECU()

	// Commanded while the ECU is dark: recorded, but nothing on the wire.
	ecu.SetKersEnabled(true)
	if len(bus.sent) != 0 {
		t.Fatalf("unpowered ECU sent %d frame(s): %#x", len(bus.sent), bus.ids())
	}
	if !ecu.kersActive {
		t.Fatal("commanded KERS state was not recorded while unpowered")
	}

	// Power arrives, and the tick after the boot hold puts the commanded state
	// out without the caller re-asking, since frames are dropped not queued.
	powerUpAndAssert(ecu)

	ids := bus.ids()
	if len(ids) != 2 || ids[0] != frameEBSSet || ids[1] != frameControl {
		t.Fatalf("expected EBS Set then Control on power-up, got %#x", ids)
	}
	if ids := bus.sent[1].Data[0] & 0x04; ids == 0 {
		t.Error("Control frame does not carry the KERS-enabled bit")
	}
}

func TestSetPowered_DoesNotReapplyOnPowerDown(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.SetPowered(true)
	bus.sent = nil

	ecu.SetPowered(false)

	if len(bus.sent) != 0 {
		t.Fatalf("power-down sent %d frame(s): %#x", len(bus.sent), bus.ids())
	}
}

func TestSetPowered_IgnoresRepeatedSameState(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.kersActive = true
	ecu.SetPowered(true)
	first := len(bus.sent)

	ecu.SetPowered(true) // watchdog polls every 500ms; only edges matter

	if len(bus.sent) != first {
		t.Fatalf("repeated SetPowered(true) re-sent frames: %#x", bus.ids())
	}
}

// TestHandleFrame_DoesNotWriteThePowerCommand separates the two facts that used
// to share one flag. A frame is evidence the controller is alive; it is not
// vehicle-service telling us the rail is up, and it must never be recorded as
// such.
func TestHandleFrame_DoesNotWriteThePowerCommand(t *testing.T) {
	ecu, _ := newGatedECU()
	if ecu.powerCmd != powerUnknown {
		t.Fatal("a fresh ECU should have no power command yet")
	}

	ecu.HandleFrame(makeFrame(frameStatus1, []byte{0, 0, 0, 0, 0, 0, 0, 0}))

	if ecu.powerCmd != powerUnknown {
		t.Errorf("a received frame wrote the power command: %s", ecu.powerCmd)
	}
}

// TestHandleFrame_CannotReopenACommandedOffGate is the regression case. When the
// rail is cut, frames already in flight arrive after the command says off. They
// used to flip the gate back open, and the watchdog closed it again on its next
// tick, so the pair oscillated at 2Hz and transmitted into a controller that was
// losing supply on every flip.
func TestHandleFrame_CannotReopenACommandedOffGate(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.SetPowered(true)
	ecu.SetBoostEnabled(true)
	ecu.SetPowered(false)
	bus.sent = nil

	// Ten frames still on the wire from the collapsing controller.
	for i := 0; i < 10; i++ {
		ecu.HandleFrame(makeFrame(frameStatus1, []byte{0, 0, 0, 0, 0, 0, 0, 0}))
	}

	if len(bus.sent) != 0 {
		t.Fatalf("in-flight frames caused %d transmission(s) into a commanded-off ECU: %#x", len(bus.sent), bus.ids())
	}
	if ecu.powerCmd != powerOff {
		t.Errorf("in-flight frames moved the power command to %s", ecu.powerCmd)
	}
}

// TestSetPowered_OffThenOnReappliesState covers the other half: state commanded
// while the rail was down must be re-applied when it comes back, because the
// controller comes up with none of it.
func TestSetPowered_OffThenOnReappliesState(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.SetPowered(true)
	ecu.SetBoostEnabled(true)
	ecu.SetPowered(false)
	bus.sent = nil

	powerUpAndAssert(ecu)

	if len(bus.sent) == 0 {
		t.Fatal("power returning did not re-apply the commanded state")
	}
	ctrl := bus.sent[len(bus.sent)-1]
	if ctrl.ID != frameControl || ctrl.Data[0]&0x02 == 0 {
		t.Errorf("re-applied frame does not carry the commanded boost bit: %#x", bus.ids())
	}
}

func TestHandleFrame_PowerUpEdgeReappliesCommandedState(t *testing.T) {
	ecu, bus := newGatedECU()

	// Commanded while dark, so dropped.
	ecu.SetBoostEnabled(true)
	if len(bus.sent) != 0 {
		t.Fatalf("unpowered ECU sent %d frame(s): %#x", len(bus.sent), bus.ids())
	}

	// The ECU starts talking before the first vehicle hash read lands. With no
	// command either way, a frame is the only evidence available and is allowed
	// to authorise the re-send, or anything commanded while it was unreachable
	// stays dropped for good.
	ecu.HandleFrame(makeFrame(frameStatus1, []byte{0, 0, 0, 0, 0, 0, 0, 0}))

	ids := bus.ids()
	if len(ids) == 0 {
		t.Fatal("first frame did not re-apply the state commanded while dark")
	}
	ctrl := bus.sent[len(bus.sent)-1]
	if ctrl.ID != frameControl {
		t.Fatalf("expected a Control frame, got %#x", ids)
	}
	if ctrl.Data[0]&0x02 == 0 {
		t.Error("Control frame does not carry the boost bit commanded while dark")
	}

	// And the watchdog catching up must not send it a second time.
	n := len(bus.sent)
	ecu.SetPowered(true)
	if len(bus.sent) != n {
		t.Errorf("watchdog re-sent after the frame edge already did: %#x", bus.ids())
	}
}

func TestSetKersCurrent_CachedValueLandsInEBSFrameOnEnable(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.SetPowered(true)
	bus.sent = nil

	// Regen settings only cache; they reach the ECU in the EBS Set frame that
	// accompanies the next enable.
	ecu.SetKersCurrent(20000)
	ecu.SetKersVoltage(54000)
	if len(bus.sent) != 0 {
		t.Fatalf("regen setters transmitted on their own: %#x", bus.ids())
	}

	ecu.SetKersEnabled(true)

	if len(bus.sent) == 0 || bus.sent[0].ID != frameEBSSet {
		t.Fatalf("expected an EBS Set frame first, got %#x", bus.ids())
	}
	gotV := binary.BigEndian.Uint16(bus.sent[0].Data[0:2])
	gotI := binary.BigEndian.Uint16(bus.sent[0].Data[2:4])
	if gotV != 54000/10 || gotI != 20000/10 {
		t.Errorf("EBS Set carried voltage=%d current=%d, want %d/%d",
			gotV, gotI, 54000/10, 20000/10)
	}
}

func TestSetGear_DroppedWhileUnpowered(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.gearRatioValues = []uint8{10, 20, 30}

	if err := ecu.SetGear(2); err != nil {
		t.Fatalf("SetGear returned %v", err)
	}

	if len(bus.sent) != 0 {
		t.Fatalf("unpowered ECU sent a gear frame: %#x", bus.ids())
	}
}

// TestHandleStatus5_LogsFirmwareOnlyOnChange pins the log-on-change behaviour.
// A powered ECU rebroadcasts Status5 about once a second forever, and every
// field in it is fixed for the life of the controller, so logging it on every
// frame filled 176 of 2966 lines in one reporter's journal with a single
// repeated payload.
func TestHandleStatus5_LogsFirmwareOnlyOnChange(t *testing.T) {
	var buf bytes.Buffer
	ecu, _ := newGatedECU()
	ecu.log = &Logger{l: log.New(&buf, "", 0), level: LogLevelInfo}

	status5 := makeFrame(frameStatus5, []byte{0x00, 0x00, 0x00, 0x00, 0x04, 0x45, 0x40, 0x0C})
	for i := 0; i < 5; i++ {
		ecu.HandleFrame(status5)
	}
	if n := strings.Count(buf.String(), "ECU firmware"); n != 1 {
		t.Fatalf("five identical Status5 frames logged the firmware block %d times, want 1", n)
	}

	// A genuinely different controller (or a firmware update) must still surface.
	changed := makeFrame(frameStatus5, []byte{0x00, 0x00, 0x00, 0x00, 0x04, 0x45, 0x40, 0x0D})
	ecu.HandleFrame(changed)
	if n := strings.Count(buf.String(), "ECU firmware"); n != 2 {
		t.Errorf("a changed Status5 payload logged %d times total, want 2", n)
	}
}

// TestSendKersState_EBSSetGoesOutEvenWhenRegenIsOff pins that the regen caps are
// configuration rather than an enable. The enable is bit 2 of the Control frame;
// the caps describe the envelope the controller may use if and when regen is
// allowed. Sending them only while regen was already on left a controller that
// powered up with regen disallowed never told its own limits.
func TestSendKersState_EBSSetGoesOutEvenWhenRegenIsOff(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.kersCurrent = DefaultKersCurrent
	powerUpAndAssert(ecu) // regen has never been enabled

	var ebs, ctrl *can.Frame
	for i := range bus.sent {
		switch bus.sent[i].ID {
		case frameEBSSet:
			ebs = &bus.sent[i]
		case frameControl:
			ctrl = &bus.sent[i]
		}
	}

	if ebs == nil {
		t.Fatalf("no EBS Set frame with regen off: %#x", bus.ids())
	}
	if got := binary.BigEndian.Uint16(ebs.Data[0:2]); got != DefaultKersVoltage/10 {
		t.Errorf("EBS Set voltage cap = %d, want %d", got, DefaultKersVoltage/10)
	}
	if got := binary.BigEndian.Uint16(ebs.Data[2:4]); got != DefaultKersCurrent/10 {
		t.Errorf("EBS Set current cap = %d, want %d", got, DefaultKersCurrent/10)
	}

	// Critical: configuring the caps must not arm regen.
	if ctrl == nil {
		t.Fatalf("no Control frame: %#x", bus.ids())
	}
	if ctrl.Data[0]&0x04 != 0 {
		t.Errorf("Control frame armed regen (bit 2) when it was never enabled: %#02x", ctrl.Data[0])
	}
}

// TestGearRatios_ResentAfterEachPowerCycle covers a flag that claimed to mean
// "sent since the ECU last powered on" but was only ever set, never cleared. It
// therefore meant "sent since this process started", so the second and every
// later power-up left the controller running on its own gear defaults.
func TestGearRatios_ResentAfterEachPowerCycle(t *testing.T) {
	bus := &fakeBus{}
	ecu := &ECU{
		log:             newLogger(LogLevelNone),
		bus:             bus,
		kersVoltage:     DefaultKersVoltage,
		gearRatioValues: []uint8{10, 20, 30},
	}

	status1 := makeFrame(frameStatus1, []byte{0, 0, 0, 0, 0, 0, 0, 0})

	countGears := func() int {
		n := 0
		for _, f := range bus.sent {
			if f.ID == frameGearControl {
				n++
			}
		}
		return n
	}

	ecu.SetPowered(true)
	ecu.HandleFrame(status1)
	if got := countGears(); got != 3 {
		t.Fatalf("first power-up sent %d gear frames, want 3", got)
	}

	// Still once per power-up, not once per frame.
	ecu.HandleFrame(status1)
	if got := countGears(); got != 3 {
		t.Fatalf("a second Status1 in the same power cycle re-sent gears: %d", got)
	}

	// Power cycle: the controller comes back with defaults, so they must go again.
	ecu.SetPowered(false)
	bus.sent = nil
	ecu.SetPowered(true)
	ecu.HandleFrame(status1)
	if got := countGears(); got != 3 {
		t.Fatalf("after a power cycle the gear ratios were sent %d times, want 3", got)
	}
}

// logCapture returns a Logger writing into buf at Info level, so a test can
// assert on what actually reached the journal.
func logCapture(buf *bytes.Buffer) *Logger {
	return &Logger{l: log.New(buf, "", 0), level: LogLevelInfo}
}

func TestSendKersState_LogsOnlyWhenFramesGoOut(t *testing.T) {
	var buf bytes.Buffer
	bus := &fakeBus{}
	ecu := &ECU{log: logCapture(&buf), bus: bus, kersVoltage: DefaultKersVoltage}

	// Gate shut: the command is recorded, nothing is sent, and nothing claims
	// otherwise at Info.
	ecu.SetKersEnabled(true)
	if len(bus.sent) != 0 {
		t.Fatalf("unpowered ECU sent %d frame(s): %#x", len(bus.sent), bus.ids())
	}
	if strings.Contains(buf.String(), "KERS -> ECU") {
		t.Errorf("logged a KERS assertion while gated:\n%s", buf.String())
	}

	// Gate open: the re-assert goes out and says so.
	buf.Reset()
	powerUpAndAssert(ecu)
	if len(bus.sent) != 2 {
		t.Fatalf("expected EBS Set and Control on power-up, got %#x", bus.ids())
	}
	if !strings.Contains(buf.String(), "KERS -> ECU") {
		t.Errorf("frames went out without a log line:\n%s", buf.String())
	}
}

// TestApplyCommandedState_HeldUntilTheControllerHasBooted is the regression
// case for the boot race. The power edge used to assert immediately, putting
// both frames on the wire seconds before the controller could acknowledge
// them. CAN hardware then retransmits the unacknowledged frame until it lands
// or the controller latches bus-off, so one early frame was enough to walk the
// bus into ERROR-PASSIVE on every ride.
func TestApplyCommandedState_HeldUntilTheControllerHasBooted(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.SetKersEnabled(true)

	ecu.SetPowered(true)
	if len(bus.sent) != 0 {
		t.Fatalf("power edge transmitted into a booting controller: %#x", bus.ids())
	}

	// Still inside the hold.
	ecu.ApplyCommandedState()
	if len(bus.sent) != 0 {
		t.Fatalf("asserted %d frame(s) before the hold elapsed: %#x", len(bus.sent), bus.ids())
	}

	ecu.mu.Lock()
	ecu.powerOnAt = time.Now().Add(-ecuAssertHoldAfterPowerOn)
	ecu.mu.Unlock()
	ecu.ApplyCommandedState()

	if ids := bus.ids(); len(ids) != 2 || ids[0] != frameEBSSet || ids[1] != frameControl {
		t.Fatalf("expected EBS Set then Control after the hold, got %#x", ids)
	}
}

func TestApplyCommandedState_ParkedPowerUpDisablesKERS(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.SetKersEnabled(true)
	ecu.SetParked(true)
	ecu.SetPowered(true)
	ecu.mu.Lock()
	ecu.powerOnAt = time.Now().Add(-ecuAssertHoldAfterPowerOn)
	ecu.mu.Unlock()

	ecu.ApplyCommandedState()
	if got, want := len(bus.sent), 2; got != want {
		t.Fatalf("sent %d frames, want %d: %#x", got, want, bus.ids())
	}
	control := bus.sent[1]
	if control.ID != frameControl {
		t.Fatalf("second frame ID = %#x, want Control %#x", control.ID, frameControl)
	}
	if control.Data[0]&0x04 != 0 {
		t.Errorf("parked Control frame enabled KERS: %#x", control.Data[0])
	}
	if !ecu.KersPolicyEnabled() {
		t.Error("parked assertion changed the commanded KERS policy")
	}
}

// TestApplyCommandedState_RepeatsUntilTheControllerSpeaks covers the slow
// controller. It answers a Control frame with 0x7E4 but ignores a status
// request until it has booted, so the assertion is the probe and repeating it
// is what gets a 5s-boot board both configured and talking.
func TestApplyCommandedState_RepeatsUntilTheControllerSpeaks(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.SetKersEnabled(true)
	ecu.SetPowered(true)
	ecu.mu.Lock()
	ecu.powerOnAt = time.Now().Add(-ecuAssertHoldAfterPowerOn)
	ecu.mu.Unlock()

	for i := 0; i < 3; i++ {
		ecu.ApplyCommandedState()
	}
	if len(bus.sent) != 6 {
		t.Fatalf("three silent ticks produced %d frame(s), want 6: %#x", len(bus.sent), bus.ids())
	}

	// The controller speaks. That acknowledges the assertion, and further ticks
	// stop sending.
	ecu.HandleFrame(makeFrame(frameStatus1, []byte{0, 0, 0, 0, 0, 0, 0, 0}))
	bus.sent = nil
	for i := 0; i < 3; i++ {
		ecu.ApplyCommandedState()
	}
	if len(bus.sent) != 0 {
		t.Fatalf("kept asserting after the controller answered: %#x", bus.ids())
	}
}

// TestApplyCommandedState_GivesUpAtTheGraceWindow stops the blind probe where
// the watchdog starts reporting the silence as E20 instead. A frame arriving
// later still asserts, so giving up costs nothing.
func TestApplyCommandedState_GivesUpAtTheGraceWindow(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.SetKersEnabled(true)
	ecu.SetPowered(true)
	ecu.mu.Lock()
	ecu.powerOnAt = time.Now().Add(-commLostPowerOnGrace - time.Second)
	ecu.mu.Unlock()

	ecu.ApplyCommandedState()
	if len(bus.sent) != 0 {
		t.Fatalf("still probing past the grace window: %#x", bus.ids())
	}

	// A controller that turns up late is configured off its first frame.
	ecu.HandleFrame(makeFrame(frameStatus1, []byte{0, 0, 0, 0, 0, 0, 0, 0}))
	if ids := bus.ids(); len(ids) != 2 || ids[0] != frameEBSSet || ids[1] != frameControl {
		t.Fatalf("late controller was not configured off its first frame, got %#x", ids)
	}
}

// TestMayTransmit_UnknownPowerNeedsARealFrame guards the fabricated evidence.
// lastFrameTime is seeded at construction so the watchdog does not read a fresh
// process as an ECU silent since the epoch, which made "unknown power, but a
// frame arrived recently" true before any frame had arrived, and let the
// settings sync transmit into a rail that was down.
func TestMayTransmit_UnknownPowerNeedsARealFrame(t *testing.T) {
	ecu, bus := newGatedECU()

	if ecu.powerCmd != powerUnknown {
		t.Fatalf("fresh ECU power command is %s, want unknown", ecu.powerCmd)
	}
	ecu.SetBoostEnabled(true)
	if len(bus.sent) != 0 {
		t.Fatalf("transmitted %d frame(s) before any frame proved the ECU alive: %#x", len(bus.sent), bus.ids())
	}
}

// TestApplyCommandedState_RetriesDoNotRepeatTheAnnouncement keeps the assertion
// log readable. The retry runs on the watchdog tick, so a controller that takes
// 5s to boot draws around seven attempts, and announcing each one buries the
// two lines that carry information under identical noise on every power-on.
func TestApplyCommandedState_RetriesDoNotRepeatTheAnnouncement(t *testing.T) {
	var buf bytes.Buffer
	bus := &fakeBus{}
	ecu := &ECU{log: logCapture(&buf), bus: bus, kersVoltage: DefaultKersVoltage}
	ecu.SetPowered(true)
	ecu.mu.Lock()
	ecu.powerOnAt = time.Now().Add(-ecuAssertHoldAfterPowerOn)
	ecu.mu.Unlock()

	for i := 0; i < 5; i++ {
		ecu.ApplyCommandedState()
	}
	if len(bus.sent) != 10 {
		t.Fatalf("five attempts sent %d frame(s), want 10", len(bus.sent))
	}
	if got := strings.Count(buf.String(), "KERS -> ECU"); got != 1 {
		t.Errorf("five blind attempts announced %d times, want 1:\n%s", got, buf.String())
	}

	// The one the controller demonstrably heard gets a line of its own.
	buf.Reset()
	ecu.HandleFrame(makeFrame(frameStatus1, []byte{0, 0, 0, 0, 0, 0, 0, 0}))
	if got := strings.Count(buf.String(), "KERS -> ECU"); got != 1 {
		t.Errorf("the acknowledged assertion announced %d times, want 1:\n%s", got, buf.String())
	}
}
