package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

func TestPowerUsesInt64ForLargePositiveAndNegativeValues(t *testing.T) {
	ecu := newTestECU()
	ecu.voltage = 2_000_000_000
	ecu.current = 2_000_000_000
	if got, want := ecu.Power(), int64(4_000_000_000_000_000); got != want {
		t.Fatalf("positive power = %d, want %d", got, want)
	}
	ecu.current = -2_000_000_000
	if got, want := ecu.Power(), int64(-4_000_000_000_000_000); got != want {
		t.Fatalf("negative power = %d, want %d", got, want)
	}
	var _ int64 = Status{}.Power
}

func TestUpdateBusDoesNotRefreshStaleECU(t *testing.T) {
	ecu := newTestECU()
	ecu.lastFrameTime = time.Now().Add(-staleTimeout - time.Second)
	ecu.UpdateBus(nil)
	if !ecu.IsStale() {
		t.Fatal("replacing bus made stale ECU appear fresh")
	}
}

func TestHandleFrameAcceptsOnlyKnownWellFormedFramesForLiveness(t *testing.T) {
	ecu, bus := newGatedECU()
	ecu.lastFrameTime = time.Now().Add(-staleTimeout - time.Second)

	if ecu.HandleFrame(makeFrame(0x123, []byte{1, 2, 3, 4})) {
		t.Fatal("unknown frame was accepted")
	}
	if ecu.sawFrame || !ecu.IsStale() || ecu.stateAckedByECU || len(bus.sent) != 0 {
		t.Fatal("unknown frame changed liveness, freshness, acknowledgement, or output")
	}

	if ecu.HandleFrame(makeFrame(frameStatus1, []byte{1, 2, 3})) {
		t.Fatal("malformed known frame was accepted")
	}
	if ecu.sawFrame || !ecu.IsStale() || ecu.stateAckedByECU || len(bus.sent) != 0 {
		t.Fatal("malformed frame changed liveness, freshness, acknowledgement, or output")
	}

	if !ecu.HandleFrame(makeFrame(frameStatus1, make([]byte, 8))) {
		t.Fatal("well-formed known frame was rejected")
	}
	if !ecu.sawFrame || ecu.IsStale() || !ecu.stateAckedByECU {
		t.Fatal("accepted frame did not update liveness, freshness, and acknowledgement")
	}
}

func TestMalformedKnownFrameRetainsWarning(t *testing.T) {
	var buf bytes.Buffer
	ecu := newTestECU()
	ecu.log = &Logger{l: log.New(&buf, "", 0), level: LogLevelInfo}
	ecu.HandleFrame(makeFrame(frameStatus1, []byte{1}))
	if !strings.Contains(buf.String(), "Status1 frame too short") {
		t.Fatalf("missing malformed-frame warning: %s", buf.String())
	}
}

func TestAppIgnoresUnknownAndMalformedFramesForStatusPublication(t *testing.T) {
	tx, mr := newTestTx(t)
	ecu := newTestECU()
	ecu.lastFrameTime = time.Now().Add(-staleTimeout - time.Second)
	a := &App{log: newLogger(LogLevelNone), ecu: ecu, ipcTx: tx}
	h := (*appHandler)(a)

	h.Handle(makeFrame(0x123, []byte{1}))
	h.Handle(makeFrame(frameStatus1, []byte{1}))
	if keys, err := mr.HKeys(ecuHashKey); err == nil && len(keys) != 0 {
		t.Fatalf("invalid traffic published status fields: %v", keys)
	}
	if got := a.frameCounts[0x123]; got != 1 {
		t.Fatalf("raw frame summary count = %d, want 1", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.kers = newKERSControllerWithDelay(ctx, time.Millisecond, func(bool) {}, func(KERSReason) {})
	a.diag = newDiagnosticsWithDurations(ctx, a.log, time.Millisecond, time.Millisecond, func(Fault, FaultConfig) {})
	h.Handle(makeFrame(frameStatus1, make([]byte, 8)))
	if got := mr.HGet(ecuHashKey, "power"); got != "0" {
		t.Fatalf("accepted frame did not publish status; power = %q", got)
	}
}
