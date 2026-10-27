package main

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/redis/go-redis/v9"
	"strconv"
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
	if err := a.loadEnergyTotals(context.Background()); err != nil {
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
	if err := a.loadEnergyTotals(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.handleFrameAt(makeFrame(frameStatus1, make([]byte, 8)), time.Now())
	for _, field := range []string{"energy:consumed", "energy:recovered"} {
		if got := mr.HGet(ecuHashKey, field); got != "0" {
			t.Fatalf("%s = %q", field, got)
		}
	}
}

func TestEnergyInvalidAndReadFailure(t *testing.T) {
	for _, value := range []string{"-1", "garbage", "1.5", "18446744073709551616", ""} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			tx, mr := newTestTx(t)
			mr.HSet(ecuHashKey, "energy:consumed", "123")
			mr.HSet(ecuHashKey, "energy:recovered", value)
			a := energyTestApp(t, tx)
			if err := a.loadEnergyTotals(context.Background()); err == nil {
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
		if err := a.loadEnergyTotals(context.Background()); err == nil {
			t.Fatal("read failure accepted")
		}
	})
}

func TestEnergyMissingFieldAndFractionalIntegration(t *testing.T) {
	tx, mr := newTestTx(t)
	mr.HSet(ecuHashKey, "energy:consumed", "10")
	a := energyTestApp(t, tx)
	if err := a.loadEnergyTotals(context.Background()); err != nil {
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

type energyReadErrorHook struct{ fail atomic.Bool }

func (h *energyReadErrorHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *energyReadErrorHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *energyReadErrorHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "hmget" && h.fail.Load() {
			return errors.New("injected energy read error")
		}
		return next(ctx, cmd)
	}
}

func TestEnergyDeferredPublicationAndRetry(t *testing.T) {
	for _, failure := range []string{"invalid", "read error"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			tx, mr := newTestTx(t)
			mr.HSet(ecuHashKey, "energy:consumed", "100")
			mr.HSet(ecuHashKey, "energy:recovered", "200")
			hook := &energyReadErrorHook{}
			if failure == "invalid" {
				mr.HSet(ecuHashKey, "energy:recovered", "bad")
			} else {
				hook.fail.Store(true)
				tx.raw.AddHook(hook)
			}
			a := energyTestApp(t, tx)
			a.opts.OdometerFile = filepath.Join(t.TempDir(), "odometer.json")
			if err := a.loadEnergyTotals(context.Background()); err == nil {
				t.Fatal("expected restore failure")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); a.restoreEnergyTotals(ctx) }()

			data := make([]byte, 8)
			binary.BigEndian.PutUint16(data[:2], 4800)
			binary.BigEndian.PutUint16(data[2:4], 30000)
			data[6] = 30
			a.handleFrameAt(makeFrame(frameStatus1, data), time.Now())
			a.ecu.mu.Lock()
			a.ecu.lastPowerUpdate = time.Now().Add(-time.Second)
			a.ecu.mu.Unlock()
			a.handleFrameAt(makeFrame(frameStatus1, data), time.Now())
			neg := int16(-30000)
			binary.BigEndian.PutUint16(data[2:4], uint16(neg))
			a.ecu.mu.Lock()
			a.ecu.lastPowerUpdate = time.Now().Add(-time.Second)
			a.ecu.mu.Unlock()
			a.handleFrameAt(makeFrame(frameStatus1, data), time.Now())
			a.handleFrameAt(makeFrame(frameStatus2, []byte{20, 0, 0, 0, 0, 1}), time.Now())
			a.handleFrameAt(makeFrame(frameStatus3, []byte{0, 0, 0, 10}), time.Now())
			consumed, recovered, ready := a.ecu.energyTotals()
			if ready || consumed == 0 || recovered == 0 {
				t.Fatalf("pending totals = %d, %d, %v", consumed, recovered, ready)
			}
			if got := mr.HGet(ecuHashKey, "speed"); got == "" || got == "0" {
				t.Fatalf("speed = %q", got)
			}
			if got := mr.HGet(ecuHashKey, "fault:code"); got != "1" {
				t.Fatalf("fault = %q", got)
			}
			if got := mr.HGet(ecuHashKey, "odometer"); got == "" || got == "0" {
				t.Fatalf("odometer = %q", got)
			}
			if got := mr.HGet(ecuHashKey, "energy:consumed"); got != "100" {
				t.Fatalf("overwrote consumed: %q", got)
			}
			wantRecovered := "200"
			if failure == "invalid" {
				wantRecovered = "bad"
			}
			if got := mr.HGet(ecuHashKey, "energy:recovered"); got != wantRecovered {
				t.Fatalf("overwrote recovered: %q", got)
			}
			if failure == "invalid" {
				mr.HSet(ecuHashKey, "energy:recovered", "200")
			} else {
				hook.fail.Store(false)
			}
			select {
			case <-done:
			case <-time.After(energyRestoreInterval + 3*time.Second):
				t.Fatal("restore did not retry")
			}
			c, r, ready := a.ecu.energyTotals()
			if !ready || c != 100+consumed || r != 200+recovered {
				t.Fatalf("restored totals = %d, %d, %v", c, r, ready)
			}
			if err := a.loadEnergyTotals(context.Background()); err != nil {
				t.Fatal(err)
			}
			c2, r2, _ := a.ecu.energyTotals()
			if c2 != c || r2 != r {
				t.Fatal("baseline applied twice")
			}
			a.handleFrameAt(makeFrame(frameStatus2, []byte{20, 0, 0, 0, 0, 1}), time.Now())
			if got := mr.HGet(ecuHashKey, "energy:consumed"); got != strconv.FormatUint(c, 10) {
				t.Fatalf("published consumed = %q", got)
			}
			if got := mr.HGet(ecuHashKey, "energy:recovered"); got != strconv.FormatUint(r, 10) {
				t.Fatalf("published recovered = %q", got)
			}
		})
	}
}

type energyBlockedReadHook struct {
	started chan struct{}
	release chan struct{}
}

func (h *energyBlockedReadHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *energyBlockedReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *energyBlockedReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "hmget" {
			close(h.started)
			select {
			case <-h.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return next(ctx, cmd)
	}
}

func TestEnergyConcurrentFramesDuringRestore(t *testing.T) {
	tx, mr := newTestTx(t)
	mr.HSet(ecuHashKey, "energy:consumed", "10000")
	mr.HSet(ecuHashKey, "energy:recovered", "20000")
	hook := &energyBlockedReadHook{started: make(chan struct{}), release: make(chan struct{})}
	tx.raw.AddHook(hook)
	a := energyTestApp(t, tx)
	done := make(chan error, 1)
	go func() { done <- a.loadEnergyTotals(context.Background()) }()
	<-hook.started
	data := make([]byte, 8)
	binary.BigEndian.PutUint16(data[:2], 3600)
	binary.BigEndian.PutUint16(data[2:4], 1000)
	frame := makeFrame(frameStatus1, data)
	a.ecu.HandleFrame(frame)
	step := func() {
		a.ecu.mu.Lock()
		a.ecu.lastPowerUpdate = time.Now().Add(-100 * time.Millisecond)
		a.ecu.mu.Unlock()
		a.ecu.HandleFrame(frame)
	}
	step()
	before, _, ready := a.ecu.energyTotals()
	if ready || before < 9 {
		t.Fatalf("pending = %d, ready %v", before, ready)
	}
	close(hook.release)
	for i := 0; i < 100; i++ {
		step()
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c, r, ready := a.ecu.energyTotals()
	if !ready || c < 10000+before+900 || c >= 12000 || r != 20000 {
		t.Fatalf("concurrent totals = %d %d %v, pending %d", c, r, ready, before)
	}
	if a.ecu.restoreEnergyTotals(10000, 20000) {
		t.Fatal("second restoration applied")
	}
	c2, r2, _ := a.ecu.energyTotals()
	if c2 != c || r2 != r {
		t.Fatal("second restoration changed totals")
	}
}

func TestNewAppDoesNotReadEnergyBeforeCAN(t *testing.T) {
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
	_, err = NewApp(context.Background(), Options{RedisServer: host, RedisPort: p, CANDevice: "no-such-can", LogLevel: LogLevelNone, OdometerFile: filepath.Join(t.TempDir(), "odometer")})
	if err == nil || !strings.Contains(err.Error(), "CAN bus open") {
		t.Fatalf("startup error = %v", err)
	}
	if got := mr.HGet(ecuHashKey, "energy:consumed"); got != "invalid" {
		t.Fatalf("invalid total changed: %q", got)
	}
}
