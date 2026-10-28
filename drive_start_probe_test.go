package main

import (
	"testing"
	"time"
)

func TestDriveStartProbesDespiteOtherFreshFrames(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.SetPowered(true)
	bus.sent = nil
	w := newTestCommLostWatcher(ecu, pastGrace)
	now := time.Now()
	for i := 0; i < 5; i++ {
		at := now.Add(time.Duration(i) * commLostTick)
		ecu.lastFrameTime = at
		if w.evaluateAt(true, at) {
			t.Fatal("fresh traffic raised E20")
		}
		w.requestDriveStartStatus("parked", "waiting-ecu", true, at)
	}
	if len(bus.sent) != 3 {
		t.Fatalf("five half-second ticks sent %d probes, want 3", len(bus.sent))
	}
	for _, id := range bus.ids() {
		if id != frameStatusReq {
			t.Fatalf("sent CAN ID %#x instead of status request", id)
		}
	}
	w.requestDriveStartStatus("parked", "timeout", true, now.Add(10*time.Second))
	w.requestDriveStartStatus("ready-to-drive", "waiting-ecu", true, now.Add(20*time.Second))
	if len(bus.sent) != 3 {
		t.Fatal("continued probing after timeout or drive entry")
	}
}

func TestDriveStartProbeGates(t *testing.T) {
	for _, tc := range []struct {
		name, state, status   string
		powered, failed, want bool
		powerAge              time.Duration
	}{
		{"waiting", "parked", "waiting-ecu", true, false, true, pastGrace},
		{"throttle held", "parked", "release-throttle", true, false, true, pastGrace},
		{"wheel moving", "parked", "stop-vehicle", true, false, true, pastGrace},
		{"no supply", "parked", "waiting-ecu", false, false, false, pastGrace},
		{"boot grace", "parked", "waiting-ecu", true, false, false, time.Second},
		{"transport fault", "parked", "waiting-ecu", true, true, false, pastGrace},
		{"battery activation", "parked", "waiting-battery", true, false, false, pastGrace},
		{"no request", "parked", "", true, false, false, pastGrace},
		{"during ride", "ready-to-drive", "release-throttle", true, false, false, pastGrace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ecu, bus := newGatedECU()
			ecu.SetPowered(true)
			bus.sent = nil
			w := newTestCommLostWatcher(ecu, tc.powerAge)
			w.failed = tc.failed
			w.requestDriveStartStatus(tc.state, tc.status, tc.powered, time.Now())
			if (len(bus.sent) == 1) != tc.want {
				t.Fatalf("frames=%v want probe=%v", bus.ids(), tc.want)
			}
		})
	}
}

func TestDriveStartDoesNotDuplicateWatchdogProbe(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.SetPowered(true)
	bus.sent = nil
	w := newTestCommLostWatcher(ecu, pastGrace)
	now := time.Now()
	w.probeAt = now
	w.requestDriveStartStatus("parked", "waiting-ecu", true, now)
	if len(bus.sent) != 0 {
		t.Fatal("duplicated watchdog probe")
	}
	w.requestDriveStartStatus("parked", "waiting-ecu", true, now.Add(driveStartProbeInterval))
	if len(bus.sent) != 1 {
		t.Fatal("did not probe after cooldown")
	}
}
