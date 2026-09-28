package main

import (
	"context"
	"testing"
	"time"
)

func TestDiagnosticsResetWaitsForPublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	d := newDiagnosticsWithDurations(ctx, newLogger(LogLevelNone), time.Millisecond, time.Millisecond, func(Fault, FaultConfig) {
		close(entered)
		<-release
	})
	d.Update(1)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback not invoked")
	}
	done := make(chan struct{})
	go func() { d.Reset(); close(done) }()
	select {
	case <-done:
		t.Fatal("Reset returned before publication completed")
	case <-time.After(20 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Reset did not complete")
	}
}

func TestDiagnosticsResetCancelsPendingClear(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan Fault, 2)
	d := newDiagnosticsWithDurations(ctx, newLogger(LogLevelNone), time.Millisecond, 30*time.Millisecond, func(f Fault, _ FaultConfig) { changes <- f })
	d.Update(1)
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("fault not committed")
	}
	d.Update(0)
	d.Reset()
	select {
	case f := <-changes:
		t.Fatalf("pending clear survived reset: %d", f)
	case <-time.After(60 * time.Millisecond):
	}
}
