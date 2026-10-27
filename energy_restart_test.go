package main

import (
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func energyTestApp(t *testing.T, tx *IPCTx) *App {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a := &App{log: newLogger(LogLevelNone), ecu: newTestECU(), ipcTx: tx}
	a.kers = newKERSControllerWithDelay(ctx, time.Millisecond, func(bool) {}, func(KERSReason) {})
	a.diag = newDiagnosticsWithDurations(ctx, a.log, time.Millisecond, time.Millisecond, func(Fault, FaultConfig) {})
	return a
}

func TestEnergyRestartThenFrames(t *testing.T) {
	tx, mr := newTestTx(t)
	if err := tx.SendStatus(Status{EnergyConsumed: 349617, EnergyRecovered: 24708}); err != nil {
		t.Fatal(err)
	}
	// A new transmitter and ECU represent the restarted process.
	tx = newIPCTx(context.Background(), tx.client, newLogger(LogLevelNone))
	a := energyTestApp(t, tx)
	if err := a.loadEnergyTotals(); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 8)
	binary.BigEndian.PutUint16(data[:2], 4800)
	binary.BigEndian.PutUint16(data[2:4], 30000)
	a.handleFrameAt(makeFrame(frameStatus1, data), time.Now())
	for field, want := range map[string]string{"energy:consumed": "349617", "energy:recovered": "24708"} {
		if got := mr.HGet(ecuHashKey, field); got != want {
			t.Fatalf("first frame %s = %s, want %s", field, got, want)
		}
	}
	// Force a known elapsed interval without sleeping; integration still runs through a real frame.
	a.ecu.lastPowerUpdate = time.Now().Add(-time.Second)
	a.handleFrameAt(makeFrame(frameStatus1, data), time.Now())
	consumed, _ := strconv.ParseUint(mr.HGet(ecuHashKey, "energy:consumed"), 10, 64)
	if consumed <= 349617 {
		t.Fatalf("consumed did not increase: %d", consumed)
	}
	neg := int16(-30000)
	binary.BigEndian.PutUint16(data[2:4], uint16(neg))
	a.ecu.lastPowerUpdate = time.Now().Add(-time.Second)
	a.handleFrameAt(makeFrame(frameStatus1, data), time.Now())
	recovered, _ := strconv.ParseUint(mr.HGet(ecuHashKey, "energy:recovered"), 10, 64)
	if recovered <= 24708 {
		t.Fatalf("recovered did not increase: %d", recovered)
	}
	if got := mr.HGet(ecuHashKey, "energy:consumed"); got != strconv.FormatUint(consumed, 10) {
		t.Fatalf("consumed changed during recovery: %s", got)
	}
}

func TestEnergyFirstBootThenFrame(t *testing.T) {
	tx, mr := newTestTx(t)
	a := energyTestApp(t, tx)
	if err := a.loadEnergyTotals(); err != nil {
		t.Fatal(err)
	}
	a.handleFrameAt(makeFrame(frameStatus1, make([]byte, 8)), time.Now())
	for _, field := range []string{"energy:consumed", "energy:recovered"} {
		if got := mr.HGet(ecuHashKey, field); got != "0" {
			t.Fatalf("%s = %q", field, got)
		}
	}
}

func TestEnergyInvalidAndReadFailureStopStartup(t *testing.T) {
	for _, value := range []string{"-1", "garbage", "1.5", "18446744073709551616", ""} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			tx, mr := newTestTx(t)
			mr.HSet(ecuHashKey, "energy:consumed", "123")
			mr.HSet(ecuHashKey, "energy:recovered", value)
			a := energyTestApp(t, tx)
			if err := a.loadEnergyTotals(); err == nil {
				t.Fatal("invalid energy accepted")
			}
			if got := mr.HGet(ecuHashKey, "energy:consumed"); got != "123" {
				t.Fatalf("retained value changed: %s", got)
			}
		})
	}
	t.Run("read failure", func(t *testing.T) {
		tx, mr := newTestTx(t)
		a := energyTestApp(t, tx)
		mr.SetError("read failure")
		if err := a.loadEnergyTotals(); err == nil {
			t.Fatal("read failure accepted")
		}
	})
}

func TestEnergyMissingFieldAndFractionalIntegration(t *testing.T) {
	tx, mr := newTestTx(t)
	mr.HSet(ecuHashKey, "energy:consumed", "10")
	a := energyTestApp(t, tx)
	if err := a.loadEnergyTotals(); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 8)
	binary.BigEndian.PutUint16(data[:2], 3600)
	binary.BigEndian.PutUint16(data[2:4], 100)
	a.handleFrameAt(makeFrame(frameStatus1, data), time.Now())
	for i := 0; i < 3; i++ {
		a.ecu.lastPowerUpdate = time.Now().Add(-50 * time.Millisecond)
		a.handleFrameAt(makeFrame(frameStatus1, data), time.Now())
	}
	if a.ecu.energyConsumedFrac <= 0 || a.ecu.EnergyConsumed() != 11 {
		t.Fatalf("fractional integration = %d + %f", a.ecu.EnergyConsumed(), a.ecu.energyConsumedFrac)
	}
	if got := mr.HGet(ecuHashKey, "energy:recovered"); got != "0" {
		t.Fatalf("missing recovered = %q", got)
	}
}

func TestNewAppRejectsInvalidEnergyBeforeCAN(t *testing.T) {
	_, mr := newTestTx(t)
	mr.HSet(ecuHashKey, "energy:consumed", "invalid")
	host, port, err := net.SplitHostPort(mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewApp(context.Background(), Options{RedisServer: host, RedisPort: p, CANDevice: "no-such-can", LogLevel: LogLevelNone})
	if err == nil || !strings.Contains(err.Error(), "invalid energy:consumed") {
		t.Fatalf("startup error = %v", err)
	}
	if got := mr.HGet(ecuHashKey, "energy:consumed"); got != "invalid" {
		t.Fatalf("invalid total changed: %q", got)
	}
}
