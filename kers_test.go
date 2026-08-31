package main

import (
	"context"
	"testing"
	"time"
)

func TestKERSDuplicateReadyNotificationDoesNotRestartDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	enabled := make(chan bool, 2)
	k := newKERSControllerWithDelay(ctx, 100*time.Millisecond, func(v bool) { enabled <- v }, func(KERSReason) {})
	k.SetTempState(TempIdeal)
	k.SetReadyToDrive(true)
	time.Sleep(70 * time.Millisecond)
	k.SetReadyToDrive(true)

	select {
	case got := <-enabled:
		if !got {
			t.Fatal("ideal battery did not enable KERS")
		}
	case <-time.After(65 * time.Millisecond):
		t.Fatal("duplicate ready notification restarted engine-on timer")
	}
}

func TestKERSLeavingReadyCancelsAndReentryGetsFullDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	enabled := make(chan bool, 2)
	k := newKERSControllerWithDelay(ctx, 40*time.Millisecond, func(v bool) { enabled <- v }, func(KERSReason) {})
	k.SetTempState(TempIdeal)
	k.SetReadyToDrive(true)
	time.Sleep(25 * time.Millisecond)
	k.SetReadyToDrive(false)
	k.SetReadyToDrive(true)

	select {
	case got := <-enabled:
		t.Fatalf("KERS callback %v arrived from stale ready timer", got)
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case got := <-enabled:
		if !got {
			t.Fatal("reentry did not enable KERS")
		}
	case <-time.After(35 * time.Millisecond):
		t.Fatal("reentry did not honor its own delay")
	}
}

func TestKERSDropsCallbacksComputedBeforeNewerState(t *testing.T) {
	var enabled []bool
	k := &KERSController{onEnable: func(v bool) { enabled = append(enabled, v) }}
	k.generation = 2
	k.runActions(kersActions{generation: 1, callEnable: true, enabled: true})
	k.runActions(kersActions{generation: 2, callEnable: true, enabled: false})
	if len(enabled) != 1 || enabled[0] {
		t.Fatalf("callbacks = %v, want only newest disable", enabled)
	}
}

func TestKERSUnknownTemperatureDisablesWhenReadyAndStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	enabled := make(chan bool, 3)
	k := newKERSControllerWithDelay(ctx, time.Millisecond, func(v bool) { enabled <- v }, func(KERSReason) {})
	k.SetTempState(TempIdeal)
	k.SetReadyToDrive(true)
	select {
	case <-enabled:
	case <-time.After(time.Second):
		t.Fatal("KERS did not become ready")
	}

	k.SetTempState(TempUnknown)
	select {
	case got := <-enabled:
		if got {
			t.Fatal("unknown temperature enabled KERS")
		}
	case <-time.After(time.Second):
		t.Fatal("unknown temperature did not disable KERS")
	}
	if got := k.Reason(); got != KERSReasonUnknown {
		t.Fatalf("reason = %q, want unknown", got)
	}
}

func TestBatteryActiveTemperaturePrecedenceIsSlotIndependent(t *testing.T) {
	cases := []struct {
		a, b TempState
		want TempState
	}{
		{TempHot, TempCold, TempHot},
		{TempCold, TempHot, TempHot},
		{TempCold, TempUnknown, TempCold},
		{TempUnknown, TempCold, TempCold},
		{TempUnknown, TempIdeal, TempUnknown},
		{TempIdeal, TempUnknown, TempUnknown},
	}
	for _, tc := range cases {
		tracker := &BatteryTracker{}
		tracker.SetState(0, true, tc.a)
		tracker.SetState(1, true, tc.b)
		if got := tracker.ActiveTempState(); got != tc.want {
			t.Errorf("states (%v,%v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}

	tracker := &BatteryTracker{}
	if got := tracker.ActiveTempState(); got != TempUnknown {
		t.Fatalf("no active batteries = %v, want unknown", got)
	}
}
