package ecu

import (
	"encoding/binary"
	"testing"

	"github.com/brutella/can"
)

// testLogger implements Logger for testing
type testLogger struct{}

func (l *testLogger) Printf(format string, v ...interface{}) {}
func (l *testLogger) Debug(format string, v ...interface{})   {}
func (l *testLogger) Info(format string, v ...interface{})    {}
func (l *testLogger) Warn(format string, v ...interface{})    {}
func (l *testLogger) Error(format string, v ...interface{})   {}
func (l *testLogger) DebugCAN(direction string, id uint32, data []byte, length uint8) {
}

// --- SpeedBuffer tests ---

func TestSpeedBuffer_SingleValue(t *testing.T) {
	var buf SpeedBuffer
	avg := buf.MovingAverage(100)
	if avg != 100.0 {
		t.Errorf("expected 100.0, got %f", avg)
	}
}

func TestSpeedBuffer_WindowFill(t *testing.T) {
	var buf SpeedBuffer
	buf.MovingAverage(100)
	buf.MovingAverage(200)
	avg := buf.MovingAverage(300)
	// Window full: (100+200+300)/3 = 200
	if avg != 200.0 {
		t.Errorf("expected 200.0, got %f", avg)
	}
}

func TestSpeedBuffer_WindowSlide(t *testing.T) {
	var buf SpeedBuffer
	buf.MovingAverage(100) // [100, 0, 0] count=1
	buf.MovingAverage(200) // [100, 200, 0] count=2
	buf.MovingAverage(300) // [100, 200, 300] count=3
	avg := buf.MovingAverage(400) // replaces 100: [400, 200, 300] sum=900
	expected := 300.0
	if avg != expected {
		t.Errorf("expected %f, got %f", expected, avg)
	}
}

func TestSpeedBuffer_Reset(t *testing.T) {
	var buf SpeedBuffer
	buf.MovingAverage(100)
	buf.MovingAverage(200)
	buf.Reset()
	avg := buf.MovingAverage(50)
	if avg != 50.0 {
		t.Errorf("expected 50.0 after reset, got %f", avg)
	}
}

func TestSpeedBuffer_ZeroInput(t *testing.T) {
	var buf SpeedBuffer
	buf.MovingAverage(100)
	avg := buf.MovingAverage(0)
	// (100+0)/2 = 50
	if avg != 50.0 {
		t.Errorf("expected 50.0, got %f", avg)
	}
}

func TestSpeedBuffer_MaxUint16Values(t *testing.T) {
	var buf SpeedBuffer
	buf.MovingAverage(65535)
	buf.MovingAverage(65535)
	avg := buf.MovingAverage(65535)
	// sum field is uint16, so 3*65535=196605 wraps to 65533
	// In practice this doesn't occur: Bosch speed is a single byte (max 255)
	expected := float64(65533) / 3.0
	if avg != expected {
		t.Errorf("expected %f, got %f", expected, avg)
	}
}

// --- calculateSpeed tests ---

func TestCalculateSpeed_Zero(t *testing.T) {
	b := &BaseECU{}
	speed := b.calculateSpeed(0)
	if speed != 0 {
		t.Errorf("expected 0 for zero input, got %d", speed)
	}
}

func TestCalculateSpeed_NonZero(t *testing.T) {
	b := &BaseECU{}
	speed := b.calculateSpeed(100)
	// 100 * CalibrationFactor(1.03) * SpeedToleranceFactor(1.155556) = ~119
	expected := uint16(119)
	if speed != expected {
		t.Errorf("expected %d, got %d", expected, speed)
	}
}

func TestCalculateSpeed_ZeroResetsBuffer(t *testing.T) {
	b := &BaseECU{}
	b.calculateSpeed(100)
	b.calculateSpeed(200)
	b.calculateSpeed(0) // resets buffer
	speed := b.calculateSpeed(50)
	// After reset, buffer has only one value (50)
	// 50 * 1.03 * 1.155556 = ~59.5
	expected := uint16(59)
	if speed != expected {
		t.Errorf("expected %d, got %d", expected, speed)
	}
}

// --- Fault mapping tests ---

func TestMapBoschFault(t *testing.T) {
	tests := []struct {
		code     uint32
		expected ECUFault
	}{
		{0x01, FaultBatteryOverVoltage},
		{0x02, FaultBatteryUnderVoltage},
		{0x03, FaultMotorShortCircuit},
		{0x0B, FaultOverTemperature},
		{0x10, FaultInternal15vAbnormal},
		{0x00, FaultNone},
		{0xFF, FaultNone},
	}

	for _, tt := range tests {
		result := MapBoschFault(tt.code)
		if result != tt.expected {
			t.Errorf("MapBoschFault(0x%X): expected %d, got %d", tt.code, tt.expected, result)
		}
	}
}

func TestMapVotolFault(t *testing.T) {
	tests := []struct {
		code     uint32
		expected ECUFault
	}{
		{0x01, FaultMotorStalled},
		{0x02, FaultHallSensorAbnormal},
		{0x20, FaultOverTemperature},
		{0x00, FaultNone},
		{0xFF, FaultNone},
	}

	for _, tt := range tests {
		result := MapVotolFault(tt.code)
		if result != tt.expected {
			t.Errorf("MapVotolFault(0x%X): expected %d, got %d", tt.code, tt.expected, result)
		}
	}
}

// --- Bosch CAN frame parsing tests ---

func makeCANFrame(id uint32, data []byte) can.Frame {
	f := can.Frame{
		ID:     id,
		Length: uint8(len(data)),
	}
	copy(f.Data[:], data)
	return f
}

func newTestBoschECU() *BoschECU {
	b := &BoschECU{}
	b.logger = &testLogger{}
	return b
}

func TestBoschStatus1_Parse(t *testing.T) {
	b := newTestBoschECU()
	data := make([]byte, 8)
	// Voltage: 4800 * 10 = 48000 mV
	binary.BigEndian.PutUint16(data[0:2], 4800)
	// Current: 500 * 10 = 5000 mA
	binary.BigEndian.PutUint16(data[2:4], 500)
	// RPM: 3000
	binary.BigEndian.PutUint16(data[4:6], 3000)
	// Speed: 45 km/h
	data[6] = 45
	// Throttle on
	data[7] = 0x01

	err := b.HandleFrame(makeCANFrame(BoschStatus1FrameID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	if b.GetVoltage() != 48000 {
		t.Errorf("voltage: expected 48000, got %d", b.GetVoltage())
	}
	if b.GetCurrent() != 5000 {
		t.Errorf("current: expected 5000, got %d", b.GetCurrent())
	}
	if b.GetRPM() != 3000 {
		t.Errorf("RPM: expected 3000, got %d", b.GetRPM())
	}
	if b.GetRawSpeed() != 45 {
		t.Errorf("raw speed: expected 45, got %d", b.GetRawSpeed())
	}
	if !b.GetThrottleOn() {
		t.Error("throttle: expected on")
	}
}

func TestBoschStatus1_NegativeCurrent(t *testing.T) {
	b := newTestBoschECU()
	data := make([]byte, 8)
	binary.BigEndian.PutUint16(data[0:2], 4800)
	// Negative current (regen): -200 as int16
	neg200 := int16(-200)
	binary.BigEndian.PutUint16(data[2:4], uint16(neg200))
	binary.BigEndian.PutUint16(data[4:6], 0)
	data[6] = 0

	err := b.HandleFrame(makeCANFrame(BoschStatus1FrameID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	if b.GetCurrent() != -2000 {
		t.Errorf("current: expected -2000, got %d", b.GetCurrent())
	}
}

func TestBoschStatus1_ShortFrame(t *testing.T) {
	b := newTestBoschECU()
	data := make([]byte, 4) // too short

	err := b.HandleFrame(makeCANFrame(BoschStatus1FrameID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	// Values should remain zero
	if b.GetVoltage() != 0 {
		t.Errorf("voltage should be 0 after short frame, got %d", b.GetVoltage())
	}
}

func TestBoschStatus2_Parse(t *testing.T) {
	b := newTestBoschECU()
	data := make([]byte, 6)
	data[0] = 0x2D // Temperature: 45°C
	// Fault code at bytes 2-5
	binary.BigEndian.PutUint32(data[2:6], 0x03)

	err := b.HandleFrame(makeCANFrame(BoschStatus2FrameID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	if b.GetTemperature() != 45 {
		t.Errorf("temperature: expected 45, got %d", b.GetTemperature())
	}
	if b.GetFaultCode() != 3 {
		t.Errorf("fault code: expected 3, got %d", b.GetFaultCode())
	}
}

func TestBoschStatus2_SpuriousFault15(t *testing.T) {
	b := newTestBoschECU()
	data := make([]byte, 6)
	binary.BigEndian.PutUint32(data[2:6], 15)

	err := b.HandleFrame(makeCANFrame(BoschStatus2FrameID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	if b.GetFaultCode() != 0 {
		t.Errorf("fault 15 should be filtered to 0, got %d", b.GetFaultCode())
	}
}

func TestBoschStatus3_Odometer(t *testing.T) {
	b := newTestBoschECU()
	data := make([]byte, 4)
	// Raw odometer in 0.1km steps
	binary.BigEndian.PutUint32(data[0:4], 1000) // 100km

	err := b.HandleFrame(makeCANFrame(BoschStatus3FrameID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	// Expected: 1000 * 1.07 * 100 = 107000 meters
	expected := uint32(float64(1000) * OdometerCalibrationFactor * 100)
	if b.GetOdometer() != expected {
		t.Errorf("odometer: expected %d, got %d", expected, b.GetOdometer())
	}
}

func TestBoschStatus4_KERS(t *testing.T) {
	b := newTestBoschECU()
	data := []byte{0x40} // bit 6 set = KERS on

	err := b.HandleFrame(makeCANFrame(BoschStatus4FrameID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	if !b.GetKersEnabled() {
		t.Error("KERS should be enabled")
	}
}

func TestBoschGear(t *testing.T) {
	b := newTestBoschECU()
	data := []byte{2}

	err := b.HandleFrame(makeCANFrame(BoschGearFrameID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	if b.GetGear() != 2 {
		t.Errorf("gear: expected 2, got %d", b.GetGear())
	}
}

func TestBoschFirmwareVersion(t *testing.T) {
	b := newTestBoschECU()
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data[0:4], 0xDEADBEEF)

	err := b.HandleFrame(makeCANFrame(BoschStatus5FrameID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	if b.GetFirmwareVersion() != 0xDEADBEEF {
		t.Errorf("firmware: expected 0xDEADBEEF, got 0x%X", b.GetFirmwareVersion())
	}
}

func TestBoschActiveFaults(t *testing.T) {
	b := newTestBoschECU()
	data := make([]byte, 6)
	// Fault code 0x03 = motor short circuit
	binary.BigEndian.PutUint32(data[2:6], 0x03)

	b.HandleFrame(makeCANFrame(BoschStatus2FrameID, data))

	faults := b.GetActiveFaults()
	if _, ok := faults[FaultMotorShortCircuit]; !ok {
		t.Error("expected FaultMotorShortCircuit in active faults")
	}
}

func TestBoschUnknownFrame(t *testing.T) {
	b := newTestBoschECU()
	data := make([]byte, 8)

	err := b.HandleFrame(makeCANFrame(0x123, data))
	if err != nil {
		t.Fatalf("unknown frame should not error, got: %v", err)
	}
}

// --- Votol CAN frame parsing tests ---

func newTestVotolECU() *VotolECU {
	v := &VotolECU{}
	v.logger = &testLogger{}
	return v
}

func TestVotolControllerDisplay_Parse(t *testing.T) {
	v := newTestVotolECU()
	data := make([]byte, 8)
	// RPM at bytes 2-3 (little-endian)
	binary.LittleEndian.PutUint16(data[2:4], 2000)
	// Voltage at bytes 4-5 (0.1V/bit)
	binary.LittleEndian.PutUint16(data[4:6], 480) // 48.0V
	// Current at bytes 6-7 (0.1A/bit)
	binary.LittleEndian.PutUint16(data[6:8], 50) // 5.0A

	err := v.HandleFrame(makeCANFrame(VotolControllerDisplayID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	if v.GetRPM() != 2000 {
		t.Errorf("RPM: expected 2000, got %d", v.GetRPM())
	}
	if v.GetVoltage() != 48000 {
		t.Errorf("voltage: expected 48000 mV, got %d", v.GetVoltage())
	}
	if v.GetCurrent() != 5000 {
		t.Errorf("current: expected 5000 mA, got %d", v.GetCurrent())
	}
	// Speed from RPM: 2000 * 0.0783744 = ~156
	expectedSpeed := uint16(156)
	if v.GetSpeed() != expectedSpeed {
		t.Errorf("speed: expected %d, got %d", expectedSpeed, v.GetSpeed())
	}
}

func TestVotolControllerDisplay_NegativeCurrent(t *testing.T) {
	v := newTestVotolECU()
	data := make([]byte, 8)
	binary.LittleEndian.PutUint16(data[2:4], 1000)
	binary.LittleEndian.PutUint16(data[4:6], 480)
	// Negative current (regen)
	neg100 := int16(-100)
	binary.LittleEndian.PutUint16(data[6:8], uint16(neg100))

	err := v.HandleFrame(makeCANFrame(VotolControllerDisplayID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	if v.GetCurrent() != -10000 {
		t.Errorf("current: expected -10000, got %d", v.GetCurrent())
	}
}

func TestVotolControllerStatus_Parse(t *testing.T) {
	v := newTestVotolECU()
	data := make([]byte, 8)
	data[0] = 0x2D // Temperature: 45°C
	data[6] = 0x05 // Fault bits

	err := v.HandleFrame(makeCANFrame(VotolControllerStatusID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	if v.GetTemperature() != 45 {
		t.Errorf("temperature: expected 45, got %d", v.GetTemperature())
	}
	if v.GetFaultCode() != 5 {
		t.Errorf("fault code: expected 5, got %d", v.GetFaultCode())
	}
}

func TestVotolActiveFaults_MultipleBits(t *testing.T) {
	v := newTestVotolECU()
	data := make([]byte, 8)
	// Bit 0 (0x01) = FaultMotorStalled, Bit 1 (0x02) = FaultHallSensorAbnormal
	data[6] = 0x03

	v.HandleFrame(makeCANFrame(VotolControllerStatusID, data))

	faults := v.GetActiveFaults()
	if _, ok := faults[FaultMotorStalled]; !ok {
		t.Error("expected FaultMotorStalled")
	}
	if _, ok := faults[FaultHallSensorAbnormal]; !ok {
		t.Error("expected FaultHallSensorAbnormal")
	}
}

func TestVotolShortFrame(t *testing.T) {
	v := newTestVotolECU()
	data := make([]byte, 4)

	err := v.HandleFrame(makeCANFrame(VotolControllerDisplayID, data))
	if err != nil {
		t.Fatalf("HandleFrame error: %v", err)
	}

	if v.GetRPM() != 0 {
		t.Errorf("RPM should be 0 after short frame, got %d", v.GetRPM())
	}
}
