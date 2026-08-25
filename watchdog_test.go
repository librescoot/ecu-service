package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// newTestCommLostWatcher returns a watcher wired to ecu, with prevEcuPowered
// and powerOnEdge already set as if the ECU powered on powerOnAge ago, so
// tests that aren't about the power-on grace window don't have to account
// for it.
// pastGrace is a power-on age comfortably outside the grace window, for the
// tests that are about staleness rather than about the power-on window. Derived
// from the constants so it tracks them if either moves.
const pastGrace = commLostPowerOnGrace + commLostRaiseAfter

func newTestCommLostWatcher(ecu *ECU, powerOnAge time.Duration) *CommLostWatcher {
	return &CommLostWatcher{
		ecu:            ecu,
		log:            newLogger(LogLevelNone),
		prevEcuPowered: true,
		powerOnEdge:    time.Now().Add(-powerOnAge),
	}
}

// TestCommLostWatcher_StandstillStaleDoesNotRaise pins the reinstated speed
// gate. Field logs showed powered controllers going quiet at a standstill often
// enough that raising E20 for it dashes the cluster for a condition the rider
// cannot act on and which clears itself as soon as they move off. The at-rest
// case is logged instead of raised.
func TestCommLostWatcher_StandstillStaleDoesNotRaise(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 0
	ecu.lastFrameTime = time.Now().Add(-pastGrace)

	w := newTestCommLostWatcher(ecu, pastGrace)

	if w.evaluate(true) {
		t.Fatal("a stale ECU at speed 0 must not raise E20; it is logged, not shown")
	}
	if !w.silentAtRest {
		t.Error("the at-rest case must be recorded so it can be counted in the field")
	}
}

// TestCommLostWatcher_SilentAtRestLogsOnceThenRecovers covers the edge tracking:
// the check runs at 2Hz and a quiet ECU stays quiet, so the log line must fire
// on the transition rather than on the condition.
func TestCommLostWatcher_SilentAtRestLogsOnceThenRecovers(t *testing.T) {
	var buf bytes.Buffer
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 0
	ecu.lastFrameTime = time.Now().Add(-pastGrace)

	w := newTestCommLostWatcher(ecu, pastGrace)
	w.log = &Logger{l: log.New(&buf, "", 0), level: LogLevelInfo}

	for i := 0; i < 5; i++ {
		w.evaluate(true)
	}
	if n := strings.Count(buf.String(), "ECU silent at rest"); n != 1 {
		t.Fatalf("five stale ticks logged the at-rest line %d times, want 1", n)
	}

	// A frame arrives: the condition ends and that is worth one line too.
	ecu.lastFrameTime = time.Now()
	w.evaluate(true)
	if w.silentAtRest {
		t.Error("silentAtRest should clear once frames return")
	}
	if n := strings.Count(buf.String(), "no longer silent at rest"); n != 1 {
		t.Errorf("recovery logged %d times, want 1", n)
	}
}

// TestCommLostWatcher_MovingStaleStillRaises is the pre-existing mid-ride
// case: it must keep working once the moving gate is gone.
func TestCommLostWatcher_MovingStaleStillRaises(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 15
	ecu.lastFrameTime = time.Now().Add(-pastGrace)

	w := newTestCommLostWatcher(ecu, pastGrace)

	if !w.evaluate(true) {
		t.Fatal("expected E20 to raise for a stale, powered ECU mid-ride")
	}
}

func TestCommLostWatcher_FreshFrameDoesNotRaise(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 0
	ecu.lastFrameTime = time.Now()

	w := newTestCommLostWatcher(ecu, pastGrace)

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
	ecu.lastFrameTime = time.Now().Add(-pastGrace)

	w := &CommLostWatcher{ecu: ecu, log: newLogger(LogLevelNone)} // zero value: this call is the power-on edge
	if w.evaluate(true) {
		t.Fatal("a carried-over stale frame timestamp must not raise E20 right after power-on")
	}
}

func TestCommLostWatcher_UnpoweredNeverRaises(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 0
	ecu.lastFrameTime = time.Now().Add(-pastGrace)

	w := newTestCommLostWatcher(ecu, pastGrace)

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
	ecu.speed = 12
	ecu.lastFrameTime = time.Now().Add(-pastGrace)

	w := newTestCommLostWatcher(ecu, pastGrace)

	for i := 0; i < 5; i++ {
		if !w.evaluate(true) {
			t.Fatalf("tick %d: E20 verdict flapped back to false while still stale", i)
		}
	}
}

// TestCommLostWatcher_GraceClearsColdStart guards the threshold itself. The
// grace window exists to cover the ECU's boot, so it has to sit above the
// measured cold-start time with room to spare. If someone tightens either
// constant without re-measuring, this is where it fails.
func TestCommLostWatcher_GraceClearsColdStart(t *testing.T) {
	if commLostPowerOnGrace <= ecuColdStartWorst {
		t.Fatalf("grace %v does not clear the measured worst-case ECU cold start %v", commLostPowerOnGrace, ecuColdStartWorst)
	}
	if margin := commLostPowerOnGrace - ecuColdStartWorst; margin < time.Second {
		t.Errorf("grace %v leaves only %v over the measured cold start %v", commLostPowerOnGrace, margin, ecuColdStartWorst)
	}
}

// TestCommLostWatcher_ColdStartDoesNotRaise is the regression case: the ECU has
// been commanded on, has not sent its first frame yet, and is still inside its
// normal boot time. Raising here is what put dashes on the cluster at every
// power-on, because the cluster renders faultCode 20 as "—" for speed.
func TestCommLostWatcher_ColdStartDoesNotRaise(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 12 // moving, so only the grace window can suppress the raise
	// Carried over from the previous power cycle, as it is in the real service.
	ecu.lastFrameTime = time.Now().Add(-time.Minute)

	w := newTestCommLostWatcher(ecu, ecuColdStartWorst)

	if w.evaluate(true) {
		t.Fatalf("E20 raised %v after power-on, inside the ECU's measured cold start", ecuColdStartWorst)
	}
}

// TestCommLostWatcher_SilentEcuRaisesAfterGrace is the other half: widening the
// grace must not turn a genuinely dead ECU into a permanently silent watchdog.
func TestCommLostWatcher_SilentEcuRaisesAfterGrace(t *testing.T) {
	ecu, _ := newGatedECU()
	ecu.powered = true
	ecu.speed = 12 // mid-ride: the case the fault exists for
	ecu.lastFrameTime = time.Now().Add(-time.Minute)

	w := newTestCommLostWatcher(ecu, commLostPowerOnGrace+100*time.Millisecond)

	if !w.evaluate(true) {
		t.Fatal("an ECU that never sent a frame must raise E20 once the grace window expires")
	}
}
