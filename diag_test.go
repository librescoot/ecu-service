package main

import (
	"context"
	"testing"
	"time"
)

func TestDiagnosticsRepeatedPendingFaultDoesNotStarveDebounce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan Fault, 2)
	d := newDiagnosticsWithDurations(ctx, newLogger(LogLevelNone), 30*time.Millisecond, 30*time.Millisecond,
		func(f Fault, _ FaultConfig) { changes <- f })

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		d.Update(uint32(FaultOverTemperature))
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case got := <-changes:
		if got != FaultOverTemperature {
			t.Fatalf("committed %v, want %v", got, FaultOverTemperature)
		}
	default:
		t.Fatal("repeated identical fault reports starved debounce")
	}
}

func TestDiagnosticsFaultTransitionRestartsStabilityWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan Fault, 2)
	d := newDiagnosticsWithDurations(ctx, newLogger(LogLevelNone), 40*time.Millisecond, 40*time.Millisecond,
		func(f Fault, _ FaultConfig) { changes <- f })

	d.Update(uint32(FaultOverTemperature))
	time.Sleep(25 * time.Millisecond)
	d.Update(uint32(FaultBatteryOverVoltage))
	select {
	case got := <-changes:
		t.Fatalf("fault %v committed before replacement was stable", got)
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case got := <-changes:
		if got != FaultBatteryOverVoltage {
			t.Fatalf("committed %v, want replacement %v", got, FaultBatteryOverVoltage)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("replacement fault did not commit")
	}
}

func TestDiagnosticsUnknownFaultRetainsMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type change struct {
		fault Fault
		cfg   FaultConfig
	}
	changes := make(chan change, 1)
	d := newDiagnosticsWithDurations(ctx, newLogger(LogLevelNone), time.Millisecond, time.Millisecond,
		func(f Fault, cfg FaultConfig) { changes <- change{f, cfg} })

	const code = uint32(999)
	d.Update(code)
	select {
	case got := <-changes:
		if uint32(got.fault) != code || !got.cfg.Unknown || got.cfg.Description == "" {
			t.Fatalf("unknown fault metadata = (%d, %#v)", got.fault, got.cfg)
		}
	case <-time.After(time.Second):
		t.Fatal("unknown fault did not commit")
	}
}

func TestDiagnosticsRepeatedPendingClearDoesNotStarveTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan Fault, 3)
	d := newDiagnosticsWithDurations(ctx, newLogger(LogLevelNone), 10*time.Millisecond, 30*time.Millisecond,
		func(f Fault, _ FaultConfig) { changes <- f })

	d.Update(uint32(FaultOverTemperature))
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("fault did not commit")
	}

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		d.Update(0)
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case got := <-changes:
		if got != FaultNone {
			t.Fatalf("cleared to %v, want none", got)
		}
	default:
		t.Fatal("repeated identical clear reports starved clear timer")
	}
}
