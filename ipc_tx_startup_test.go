package main

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/redis/go-redis/v9"
)

// The startup default write used to zero energy:consumed and
// energy:recovered, so every service restart destroyed the running
// totals: the ECU does not re-report them until it is powered, so
// nothing put them back. These tests pin both halves of the fix.
var testCtx = context.Background()

func newTestTx(t *testing.T) (*IPCTx, *redis.Client) {
	t.Helper()
	// The methods under test hardcode the engine-ecu key, so run against a
	// scratch DB rather than whatever is in db 0.
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: 9})
	if err := rdb.Ping(testCtx).Err(); err != nil {
		t.Skipf("no local Redis: %v", err)
	}
	rdb.Del(testCtx, "engine-ecu")
	return NewIPCTx(NewLeveledLogger(log.New(io.Discard, "", 0), LogLevelError), rdb), rdb
}

func TestStartupDefaultsPreserveCumulativeCounters(t *testing.T) {
	tx, rdb := newTestTx(t)
	defer rdb.Del(testCtx, "engine-ecu")
	defer rdb.Close()

	// Simulate a running service: real totals plus live volatile values.
	if err := rdb.HSet(testCtx, "engine-ecu", map[string]interface{}{
		"energy:consumed":  349617,
		"energy:recovered": 24708,
		"odometer":         536284,
		"motor:voltage":    48380,
		"rpm":              1200,
		"throttle":         "on",
	}).Err(); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	// Restart.
	if err := tx.SendDefaultStatus1(); err != nil {
		t.Fatalf("SendDefaultStatus1() failed: %v", err)
	}
	if err := tx.SeedOdometer(); err != nil {
		t.Fatalf("SeedOdometer() failed: %v", err)
	}

	got := rdb.HGetAll(testCtx, "engine-ecu").Val()

	for field, want := range map[string]string{
		"energy:consumed":  "349617",
		"energy:recovered": "24708",
		"odometer":         "536284",
	} {
		if got[field] != want {
			t.Errorf("cumulative %s = %q after restart, want %q (restart wiped it)", field, got[field], want)
		}
	}

	for field, want := range map[string]string{
		"motor:voltage": "0",
		"rpm":           "0",
		"throttle":      "off",
	} {
		if got[field] != want {
			t.Errorf("volatile %s = %q, want %q", field, got[field], want)
		}
	}
}

func TestStartupDefaultsSeedOnColdBoot(t *testing.T) {
	tx, rdb := newTestTx(t)
	defer rdb.Del(testCtx, "engine-ecu")
	defer rdb.Close()

	// Redis is wiped on every reboot, so a cold boot starts with nothing.
	if err := tx.SendDefaultStatus1(); err != nil {
		t.Fatalf("SendDefaultStatus1() failed: %v", err)
	}
	if err := tx.SeedOdometer(); err != nil {
		t.Fatalf("SeedOdometer() failed: %v", err)
	}

	got := rdb.HGetAll(testCtx, "engine-ecu").Val()
	for _, field := range []string{"energy:consumed", "energy:recovered", "odometer"} {
		if got[field] != "0" {
			t.Errorf("cold boot %s = %q, want \"0\"", field, got[field])
		}
	}
}
