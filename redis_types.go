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
	KersOn  bool
	BoostOn bool
}

type RedisStatus5 struct {
	FirmwareVersion uint32
	Gear            uint8
}

// EBS regen caps the ECU accepted (CAN 0x7E5 echo), distinct from the
// commanded kers-power / kers-voltage setpoints, plus the derived regen
// availability view.
type RedisEBS struct {
	AcceptedVoltage int // mV
	AcceptedCurrent int // mA
	RegenAvailable  bool
	RegenReason     string // none/cold/hot/off/full
	RegenExpected   int    // mA
}
