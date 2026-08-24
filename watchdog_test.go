package main

import (
	"testing"
	"time"
)

// newTestCommLostWatcher returns a watcher wired to ecu, with prevEcuPowered
// and powerOnEdge already set as if the ECU powered on powerOnAge ago, so
// tests that aren't about the power-on grace window don't have to account
// for it.
func newTestCommLostWatcher(ecu *ECU, powerOnAge time.Duration) *CommLostWatcher {
	return &CommLostWatcher{
		ecu:            ecu,
		prevEcuPowered: true,
		powerOnEdge:    time.Now().Add(-powerOnAge),
	}
}

// TestCommLostWatcher_StandstillStaleRaises is the regression case for
// librescoot-z57t: a stale, powered ECU at speed 0 must raise E20. The old
// "moving" gate (w.ecu.Speed() != 0) suppressed this, so a bus-off at a
// standstill never surfaced the fault.
func TestCommLostWatcher_StandstillStaleRaises(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 0
	ecu.lastFrameTime = time.Now().Add(-(commLostRaiseAfter + time.Second))

	w := newTestCommLostWatcher(ecu, commLostRaiseAfter+time.Second)

	if !w.evaluate(true) {
		t.Fatal("expected E20 to raise for a stale, powered ECU at standstill")
	}
}

// TestCommLostWatcher_MovingStaleStillRaises is the pre-existing mid-ride
// case: it must keep working once the moving gate is gone.
func TestCommLostWatcher_MovingStaleStillRaises(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 15
	ecu.lastFrameTime = time.Now().Add(-(commLostRaiseAfter + time.Second))

	w := newTestCommLostWatcher(ecu, commLostRaiseAfter+time.Second)

	if !w.evaluate(true) {
		t.Fatal("expected E20 to raise for a stale, powered ECU mid-ride")
	}
}

func TestCommLostWatcher_FreshFrameDoesNotRaise(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 0
	ecu.lastFrameTime = time.Now()

	w := newTestCommLostWatcher(ecu, commLostRaiseAfter+time.Second)

	if w.evaluate(true) {
		t.Fatal("a fresh frame must not raise E20")
	}
}

// TestCommLostWatcher_PowerOnEdgeIgnoresCarriedOverFrameAge covers a fresh
// power-on with a stale lastFrameTime left over from the previous power
// cycle: staleness is measured from the more recent of {last frame,
// power-on edge}, so this must not raise the instant power comes on.
func TestCommLostWatcher_PowerOnEdgeIgnoresCarriedOverFrameAge(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 0
	ecu.lastFrameTime = time.Now().Add(-10 * time.Second)

	w := &CommLostWatcher{ecu: ecu} // zero value: this call is the power-on edge
	if w.evaluate(true) {
		t.Fatal("a carried-over stale frame timestamp must not raise E20 right after power-on")
	}
}

func TestCommLostWatcher_UnpoweredNeverRaises(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 0
	ecu.lastFrameTime = time.Now().Add(-(commLostRaiseAfter + time.Second))

	w := newTestCommLostWatcher(ecu, commLostRaiseAfter+time.Second)

	if w.evaluate(false) {
		t.Fatal("an unpowered ECU must never raise E20")
	}
}

// TestCommLostWatcher_NoFlapAtStandstillOnceRaised replays evaluate() across
// several ticks with the ECU still stale and stationary to make sure the
// verdict stays raised rather than flapping.
func TestCommLostWatcher_NoFlapAtStandstillOnceRaised(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 0
	ecu.lastFrameTime = time.Now().Add(-(commLostRaiseAfter + time.Second))

	w := newTestCommLostWatcher(ecu, commLostRaiseAfter+time.Second)

	for i := 0; i < 5; i++ {
		if !w.evaluate(true) {
			t.Fatalf("tick %d: E20 verdict flapped back to false while still stale", i)
		}
	}
}
