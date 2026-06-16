package main

import (
	"encoding/binary"
	"math"
	"testing"

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
	ecu.HandleFrame(makeFrame(frameStatus4, []byte{0x04})) // bit 2
	if !ecu.BoostEnabled() {
		t.Error("expected boost reported from ECU")
	}
	ecu.HandleFrame(makeFrame(frameStatus4, []byte{0x00}))
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

func TestMapFault_Unknown(t *testing.T) {
	f, _ := MapFault(0xFF)
	if f != FaultNone {
		t.Errorf("unknown code should map to FaultNone, got %d", f)
	}
}

func TestMapFault_Zero(t *testing.T) {
	f, _ := MapFault(0)
	if f != FaultNone {
		t.Errorf("zero should map to FaultNone, got %d", f)
	}
}
