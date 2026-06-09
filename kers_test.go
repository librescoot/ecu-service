package main

import (
	"io"
	"log"
	"testing"
	"time"
)

// Ready-to-drive must not enable KERS at the edge: writing to the ECU within
// ~1s of engine-on can wedge its CAN interface, so the engine-on state and the
// KERS write are deferred to the timer callback.
func TestKersEngineOnDeferredToTimer(t *testing.T) {
	k := &KERS{
		log:              NewLeveledLogger(log.New(io.Discard, "", 0), LogLevelError),
		temperatureState: BatteryTemperatureStateIdeal,
		vehicleStopped:   true,
		vehicleState:     VehicleStateEngineNotReady,
	}
	k.engineOnTimer = time.NewTimer(KersEngineOnDelayS)
	k.engineOnTimer.Stop()

	var calls []bool
	k.kersCallback = func(enable bool) error {
		calls = append(calls, enable)
		return nil
	}

	k.HandleVehicleStateChange(VehicleStateEngineReady)

	if k.vehicleState != VehicleStateEngineNotReady {
		t.Errorf("vehicleState = %v at the ready-to-drive edge; must stay not-ready until the engine-on delay elapses", k.vehicleState)
	}
	if len(calls) != 0 {
		t.Errorf("KERS callback fired at the edge (%v); the ECU write must wait for the engine-on delay", calls)
	}

	k.engineOnTimer.Stop()
}
