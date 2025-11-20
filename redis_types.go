package main

// Redis message types for engine ECU status updates
type RedisStatus1 struct {
	MotorVoltage    int
	MotorCurrent    int
	RPM             uint16
	Speed           uint16 // Already uint16, no change needed
	RawSpeed        uint16 // Raw speed before calibration
	ThrottleOn      bool
	InstantPower    int    // Instantaneous power in mW
	EnergyConsumed  uint64 // Cumulative energy consumed in mWh
	EnergyRecovered uint64 // Cumulative energy recovered via regen in mWh
}

type RedisStatus2 struct {
	Temperature int
}

type RedisStatus3 struct {
	Odometer uint32
}

type RedisStatus4 struct {
	KersOn bool
}

type RedisStatus5 struct {
	FirmwareVersion uint32
}
