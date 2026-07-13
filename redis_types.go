package main

// Redis message types for engine ECU status updates
type RedisStatus1 struct {
	MotorVoltage    int
	MotorCurrent    int
	RPM             uint16
	Speed           uint16
	RawSpeed        uint16
	ThrottleOn      bool
	BrakeOn         bool
	Power           int    // Instantaneous power in mW
	EnergyConsumed  uint64 // Cumulative energy consumed in mWh
	EnergyRecovered uint64 // Cumulative energy recovered in mWh
}

type RedisStatus2 struct {
	Temperature      int
	FaultCode        uint32
	FaultDescription string
}

type RedisStatus3 struct {
	Odometer uint32
}

type RedisStatus4 struct {
	KersOn          bool
	BoostOn         bool
	EcuEnabled      bool
	BoostActive     bool
	GearModeEnabled bool
}

type RedisStatus5 struct {
	FirmwareVersion   uint32
	Gear              uint8
	MotorRatedPowerKW uint8
	MotorMaxSpeedKMH  uint8
	SWBaseVersion     string
	SWAppVersion      string
	HighGearCurrent   uint8
	MidGearCurrent    uint8
	LowGearCurrent    uint8
	HighGearTorque    uint8
	MidGearTorque     uint8
	LowGearTorque     uint8
}

// RedisECUConfig carries the ECU configuration values broadcast at boot
// or in response to a status request (0x4EF). Values are 0 until the ECU
// reports them.
type RedisECUConfig struct {
	OverVoltageThresholdMV  uint32
	UnderVoltageThresholdMV uint32
	SpeedLimitRatio         uint8
	WheelCircumferenceCM    uint8
	MaxPhaseCurrentMA       uint32
	StartupPhaseCurrentMA   uint32
	EBSVoltageMV            uint32
	EBSCurrentMA            uint32
}

// EBS regen caps the ECU accepted (CAN 0x7E5 echo), distinct from the
// commanded kers-power / kers-voltage setpoints, plus the derived regen
// availability view.
type RedisEBS struct {
	AcceptedVoltage int // mV
	AcceptedCurrent int // mA
	RegenAvailable  bool
	RegenReason     string // none/cold/hot/off/standstill/full
	RegenExpected   int    // mA
}
