package main

import (
	"testing"
	"time"

	"github.com/brutella/can"
)

// TestDrivingStallRaisesE20OnDeadSocket reproduces the corrected mission: the ECU
// CAN data stall happens WHILE DRIVING, not at standstill.
//
// Scenario:
//   - Powered, MOVING vehicle (Speed()>0) with ECU Status frames arriving at ~5 Hz.
//   - After a few frames the socket dies MID-DRIVE. The reconnect loop (runCANBusLoop)
//     churns rebuilding the bus; each cycle it calls ECU.UpdateBus(newBus).
//   - ECU.UpdateBus (bosch.go:611-617) sets lastFrameTime=time.Now() UNCONDITIONALLY,
//     so every reconnect rewrites the "freshness" clock even though NO CAN frame actually
//     arrives on the rebuilt socket.
//   - CommLostWatcher.check() measures staleness via TimeSinceLastFrame(); the per-reconnect
//     clock reset keeps stale==false, so E20 never raises -> no 0x4EF replay/recovery
//     and the live feed stays frozen forever while driving.
//
// Invariant: any powered stall longer than commLostRaiseAfter eventually raises E20 (and the
// feed can then recover). With the current UpdateBus the reconnect clock-reset defeats staleness,
// so this test FAILS on current code.
//
// NOTE: runCANBusLoop itself is not run here verbatim because it calls
// can.NewBusForInterfaceWithName (a real SocketCAN device) which cannot run headless. This
// test faithfully reproduces its per-reconnect semantics — the exact ECU.UpdateBus(newBus)
// call that masks the no-frame interval — and drives CommLostWatcher.check() deterministically.
func TestDrivingStallRaisesE20OnDeadSocket(t *testing.T) {
	hash := &fakeHash{fields: map[string]string{
		"state":        "ready-to-drive",
		"engine-power": "on",
		"main-power":   "on",
	}}
	bus := &fakeBusRWC{}
	ecu := newTestECU()
	ecu.bus = can.NewBus(bus)

	// Phase 1: moving at ~5 Hz — frames land, Speed()>0.
	data := make([]byte, 8)
	data[6] = 60 // raw speed -> calibrated >0
	for range 5 {
		ecu.HandleFrame(makeFrame(frameStatus1, data))
		time.Sleep(commLostTick) // ~5 Hz
	}
	if ecu.Speed() == 0 {
		t.Fatalf("precondition: driving speed must be >0, got %d", ecu.Speed())
	}

	// Phase 2: KILL the socket mid-drive. No more frames arrive.
	// The reconnect loop churns rebuilding the bus; each cycle calls UpdateBus,
	// which resets lastFrameTime (bosch.go:615). Over the whole window the ECU
	// genuinely sends nothing, but staleness is masked.
	var raised bool
	w := newCommLostWatcher(hash, ecu, newLogger(LogLevelNone), func(raise bool) {
		raised = raise
	})
	// Pre-seed so the power-on grace window has already lapsed (excludes H5).
	w.prevEcuPowered = true
	w.powerOnEdge = time.Now().Add(-commLostPowerOnGrace - time.Second)

	// Simulate reconnect cycles over a window comfortably beyond the raise threshold.
	deadline := time.Now().Add(commLostRaiseAfter + 2*commLostTick)
	for time.Now().Before(deadline) {
		// The reconnect loop rewires the bus to a fresh socket each cycle (H1/H2).
		ecu.UpdateBus(can.NewBus(&fakeBusRWC{}))
		w.check()
		time.Sleep(commLostTick)
	}

	if !raised || !w.published {
		t.Fatalf(
			"driving stall never recovers: moving ECU went silent for >%v (mid-drive socket dead) "+
				"but E20 was never raised. ECU.UpdateBus resets lastFrameTime on every reconnect "+
				"(bosch.go:615), so TimeSinceLastFrame stays under the threshold and CommLostWatcher "+
				"never sees the no-frame interval -> no replay -> feed frozen forever. raised=%v published=%v",
			commLostRaiseAfter, raised, w.published)
	}
}
