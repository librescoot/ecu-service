package main

import (
	"errors"
	"testing"
	"time"
)

func TestCommLostProbeBoundedOutage(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.powerCmd = powerOn
	ecu.speed = 15
	now := time.Now()
	ecu.lastFrameTime = now
	w := newTestCommLostWatcher(ecu, pastGrace)
	for tick := 0; tick <= 120; tick++ {
		elapsed := time.Duration(tick) * commLostTick
		raised := w.evaluateAt(true, now.Add(elapsed))
		if want := elapsed > commLostRaiseAfter; raised != want {
			t.Fatalf("at %v: E20=%t, want %t", elapsed, raised, want)
		}
	}
	if ids := bus.ids(); len(ids) != 1 || ids[0] != frameStatusReq {
		t.Fatalf("one-minute outage sent %#x", ids)
	}
	if !ecu.LastFrameTime().Equal(now) {
		t.Fatal("probe fabricated frame freshness")
	}
}

func TestCommLostProbeReplyAndCooldown(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.powerCmd = powerOn
	ecu.stateAssertedToECU = true
	ecu.stateAckedByECU = true
	ecu.speed = 15
	now := time.Now()
	ecu.lastFrameTime = now.Add(-commLostProbeAfter)
	w := newTestCommLostWatcher(ecu, pastGrace)
	if w.evaluateAt(true, now) {
		t.Fatal("probe must not raise E20")
	}
	// An accepted frame is sufficient evidence of communication, even without speed.
	if !ecu.HandleFrame(makeFrame(frameStatus2, []byte{20, 0, 0, 0, 0, 0})) {
		t.Fatal("Status2 rejected")
	}
	ecu.lastFrameTime = now.Add(time.Millisecond)
	if w.evaluateAt(true, now.Add(commLostProbeWait)) {
		t.Fatal("reply did not prevent E20")
	}
	if len(bus.ids()) != 1 {
		t.Fatal("reply triggered a probe before cooldown elapsed")
	}
	if w.evaluateAt(true, now.Add(commLostProbeInterval)) {
		t.Fatal("second episode needs a response window")
	}
	if len(bus.ids()) != 2 {
		t.Fatalf("new silence did not receive a probe: %#x", bus.ids())
	}
	if !w.evaluateAt(true, now.Add(commLostProbeInterval+commLostProbeWait)) {
		t.Fatal("unanswered second probe did not raise E20")
	}
}

func TestCommLostProbeGates(t *testing.T) {
	for _, name := range []string{"off", "stopped", "booting", "fresh"} {
		t.Run(name, func(t *testing.T) {
			ecu, bus := newGatedECU()
			ecu.powerCmd = powerOn
			ecu.speed = 15
			now := time.Now()
			ecu.lastFrameTime = now.Add(-time.Minute)
			w := newTestCommLostWatcher(ecu, pastGrace)
			powered := true
			switch name {
			case "off":
				powered = false
			case "stopped":
				ecu.speed = 0
			case "booting":
				w.powerOnEdge = now.Add(-time.Second)
			case "fresh":
				ecu.lastFrameTime = now
			}
			if w.evaluateAt(powered, now) {
				t.Fatal("ineligible ECU raised E20")
			}
			if len(bus.ids()) != 0 {
				t.Fatal("ineligible ECU received a probe")
			}
		})
	}
}

func TestCommLostProbeRejectsInvalidReplies(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.powerCmd = powerOn
	ecu.speed = 15
	now := time.Now()
	ecu.lastFrameTime = now.Add(-time.Minute)
	w := newTestCommLostWatcher(ecu, pastGrace)
	w.evaluateAt(true, now)
	ecu.HandleFrame(makeFrame(frameStatus1, []byte{1}))
	ecu.HandleFrame(makeFrame(0x123, make([]byte, 8)))
	if !w.evaluateAt(true, now.Add(commLostProbeWait)) {
		t.Fatal("invalid traffic masked E20")
	}
	if len(bus.ids()) != 1 {
		t.Fatal("invalid traffic rearmed probe")
	}
}

func TestCommLostProbeFailedSendIsBounded(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.powerCmd = powerOn
	ecu.speed = 15
	bus.err = errors.New("CAN unavailable")
	now := time.Now()
	ecu.lastFrameTime = now.Add(-time.Minute)
	w := newTestCommLostWatcher(ecu, pastGrace)
	w.evaluateAt(true, now)
	if !w.probePending {
		t.Fatal("failed send must count as the episode's attempt")
	}
	for i := 1; i <= 10; i++ {
		if !w.evaluateAt(true, now.Add(time.Duration(i)*commLostProbeInterval)) {
			t.Fatal("failed send masked E20")
		}
		if !w.probeAt.Equal(now) {
			t.Fatal("failed send was retried during the outage")
		}
	}
}

func TestCommLostProbePowerCycle(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.powerCmd = powerOn
	ecu.speed = 15
	now := time.Now()
	ecu.lastFrameTime = now.Add(-time.Minute)
	w := newTestCommLostWatcher(ecu, pastGrace)
	w.evaluateAt(true, now)
	if !w.evaluateAt(true, now.Add(commLostProbeWait)) {
		t.Fatal("missing initial E20")
	}
	if w.evaluateAt(false, now.Add(2*time.Second)) {
		t.Fatal("power-off did not clear verdict")
	}
	poweredOn := now.Add(3 * time.Second)
	if w.evaluateAt(true, poweredOn) {
		t.Fatal("power-on did not grant grace")
	}
	afterGrace := poweredOn.Add(commLostPowerOnGrace)
	if w.evaluateAt(true, afterGrace) {
		t.Fatal("new power cycle needs a probe response window")
	}
	if len(bus.ids()) != 2 {
		t.Fatalf("power cycle did not rearm probe: %#x", bus.ids())
	}
	if !w.evaluateAt(true, afterGrace.Add(commLostProbeWait)) {
		t.Fatal("missing E20 after unanswered probe")
	}
}
