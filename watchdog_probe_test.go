package main

import (
	"errors"
	"testing"
	"time"
)

func TestCommLostBoundedRetries(t *testing.T) {
	for _, speed := range []uint16{0, 15} {
		for _, failSend := range []bool{false, true} {
			ecu, bus := newGatedECU()
			ecu.powerCmd, ecu.speed = powerOn, speed
			if failSend {
				bus.err = errors.New("CAN unavailable")
			}
			now := time.Now()
			ecu.lastFrameTime = now
			w := newTestCommLostWatcher(ecu, pastGrace)
			for tick := 0; tick <= 120; tick++ {
				elapsed := time.Duration(tick) * commLostTick
				raised := w.evaluateAt(true, now.Add(elapsed))
				want := elapsed >= commLostProbeAfter+commLostProbeInterval+commLostProbeWait
				if raised != want {
					t.Fatalf("speed=%d fail=%t at %v: raised=%t want=%t", speed, failSend, elapsed, raised, want)
				}
			}
			if w.probeAttempts != 2 {
				t.Fatalf("attempts=%d", w.probeAttempts)
			}
			if !failSend && len(bus.ids()) != 2 {
				t.Fatalf("outage frames=%#x", bus.ids())
			}
			if !ecu.LastFrameTime().Equal(now) {
				t.Fatal("send fabricated freshness")
			}
		}
	}
}

func TestCommLostProbeReplies(t *testing.T) {
	for _, onRetry := range []bool{false, true} {
		ecu, bus := newGatedECU()
		ecu.powerCmd, ecu.speed = powerOn, 15
		ecu.stateAssertedToECU, ecu.stateAckedByECU = true, true
		now := time.Now()
		ecu.lastFrameTime = now.Add(-time.Minute)
		w := newTestCommLostWatcher(ecu, pastGrace)
		w.evaluateAt(true, now)
		ecu.HandleFrame(makeFrame(frameStatus1, []byte{1}))
		ecu.HandleFrame(makeFrame(0x123, make([]byte, 8)))
		if onRetry {
			w.evaluateAt(true, now.Add(commLostProbeInterval))
			now = now.Add(commLostProbeInterval)
		}
		if !ecu.HandleFrame(makeFrame(frameStatus2, []byte{20, 0, 0, 0, 0, 0})) {
			t.Fatal("valid frame rejected")
		}
		ecu.lastFrameTime = now.Add(time.Millisecond)
		if w.evaluateAt(true, now.Add(commLostProbeWait)) {
			t.Fatal("valid reply did not prevent E20")
		}
		want := 1
		if onRetry {
			want = 2
		}
		if len(bus.ids()) != want {
			t.Fatal("reply bypassed cooldown")
		}
	}
}

func TestCommLostRecovery(t *testing.T) {
	for _, mode := range []string{"single", "sustained", "probe"} {
		t.Run(mode, func(t *testing.T) {
			ecu, bus := newGatedECU()
			ecu.powerCmd = powerOn
			now := time.Now()
			ecu.lastFrameTime = now.Add(-time.Minute)
			w := newTestCommLostWatcher(ecu, pastGrace)
			w.evaluateAt(true, now)
			w.evaluateAt(true, now.Add(commLostProbeInterval))
			now = now.Add(commLostProbeInterval + commLostProbeWait)
			if !w.evaluateAt(true, now) {
				t.Fatal("outage not raised")
			}
			now = now.Add(time.Second)
			ecu.lastFrameTime = now
			if !w.evaluateAt(true, now) {
				t.Fatal("single frame cleared fault")
			}
			switch mode {
			case "single":
				for i := 1; i <= 20; i++ {
					if !w.evaluateAt(true, now.Add(time.Duration(i)*commLostTick)) {
						t.Fatal("isolated frame cleared fault")
					}
				}
				if len(bus.ids()) != 3 {
					t.Fatalf("recovery probes not bounded: %#x", bus.ids())
				}
			case "sustained":
				ecu.lastFrameTime = now.Add(commLostTick)
				if !w.evaluateAt(true, ecu.lastFrameTime) {
					t.Fatal("recovery cleared too early")
				}
				ecu.lastFrameTime = now.Add(commLostRecoveryTime)
				if w.evaluateAt(true, ecu.lastFrameTime) {
					t.Fatal("sustained recovery not accepted")
				}
			case "probe":
				probeAt := w.probeAt.Add(commLostProbeInterval)
				if !w.evaluateAt(true, probeAt) {
					t.Fatal("probe send cleared fault")
				}
				ecu.lastFrameTime = probeAt.Add(time.Millisecond)
				if w.evaluateAt(true, ecu.lastFrameTime) {
					t.Fatal("confirmed recovery not accepted")
				}
			}
		})
	}
}

func TestCommLostTransportAndPower(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.powerCmd = powerOn
	now := time.Now()
	ecu.lastFrameTime = now
	w := newTestCommLostWatcher(ecu, pastGrace)
	w.transportCause = func(time.Time) string { return "CAN bus-off" }
	if !w.evaluateAt(true, now) || w.failureCause != "CAN bus-off" {
		t.Fatal("transport failure not attributed")
	}
	if len(bus.ids()) != 0 {
		t.Fatal("probed failed transport")
	}
	if w.evaluateAt(false, now.Add(time.Second)) {
		t.Fatal("power-off did not end monitoring")
	}
	if w.evaluateAt(true, now.Add(2*time.Second)) {
		t.Fatal("power-on grace missing")
	}
	w.transportCause = nil
	afterGrace := now.Add(2*time.Second + commLostPowerOnGrace)
	ecu.lastFrameTime = now.Add(-time.Minute)
	if w.evaluateAt(true, afterGrace) {
		t.Fatal("must probe after boot grace")
	}
	w.evaluateAt(true, afterGrace.Add(commLostProbeInterval))
	if !w.evaluateAt(true, afterGrace.Add(commLostProbeInterval+commLostProbeWait)) {
		t.Fatal("missing post-boot E20")
	}
}

func TestCommLostProbeGates(t *testing.T) {
	for _, mode := range []string{"off", "booting", "fresh"} {
		ecu, bus := newGatedECU()
		ecu.powerCmd = powerOn
		ecu.speed = 15
		now := time.Now()
		ecu.lastFrameTime = now.Add(-time.Minute)
		w := newTestCommLostWatcher(ecu, pastGrace)
		if mode == "booting" {
			w.powerOnEdge = now.Add(-time.Second)
		}
		if mode == "fresh" {
			ecu.lastFrameTime = now
		}
		if w.evaluateAt(mode != "off", now) || len(bus.ids()) != 0 {
			t.Fatalf("gate %s failed", mode)
		}
	}
}
