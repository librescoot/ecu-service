package main

import (
	"context"
	"testing"
	"time"
)

func TestKersSettingEnabled(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"enabled", true},
		{"disabled", false},
		{"false", false},
		{"true", true},
		{"", true},
		{"off", true},
	}

	for _, tt := range tests {
		if got := kersSettingEnabled(tt.val); got != tt.want {
			t.Errorf("kersSettingEnabled(%q) = %v, want %v", tt.val, got, tt.want)
		}
	}
}

func newHandlerTestRx() (*IPCRx, *ECU, *BatteryTracker) {
	ecu := newTestECU()
	ecu.kersCurrent = DefaultKersCurrent
	ecu.kersVoltage = DefaultKersVoltage
	battery := &BatteryTracker{}
	return newIPCRx(nil, newLogger(LogLevelNone), battery, nil, ecu), ecu, battery
}

func TestKersNumericHandlersRejectNegativeAndOverflow(t *testing.T) {
	rx, ecu, _ := newHandlerTestRx()
	if err := rx.handleKersPowerSingle("20000"); err != nil {
		t.Fatal(err)
	}
	if err := rx.handleKersVoltage("54000"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"-1", "65536"} {
		rx.handleKersPowerSingle(invalid)
		rx.handleKersVoltage(invalid)
	}
	if ecu.kersCurrent != 20000 {
		t.Fatalf("invalid current changed prior value to %d", ecu.kersCurrent)
	}
	if ecu.kersVoltage != 54000 {
		t.Fatalf("invalid voltage changed prior value to %d", ecu.kersVoltage)
	}
}

func TestKersNumericDeletionDefaults(t *testing.T) {
	rx, ecu, battery := newHandlerTestRx()
	battery.SetState(0, true, TempIdeal)
	battery.SetState(1, true, TempIdeal)
	rx.handleKersPowerSingle("19000")
	rx.handleKersPowerDual("30000")
	if ecu.kersCurrent != 30000 {
		t.Fatalf("dual current = %d, want 30000", ecu.kersCurrent)
	}
	rx.handleKersPowerDual("")
	if ecu.kersCurrent != 19000 || rx.hasDualPower {
		t.Fatalf("deleted dual did not fall back to single: current=%d hasDual=%v", ecu.kersCurrent, rx.hasDualPower)
	}
	rx.handleKersPowerSingle("")
	rx.handleKersVoltage("54000")
	rx.handleKersVoltage("")
	if ecu.kersCurrent != DefaultKersCurrent || ecu.kersVoltage != DefaultKersVoltage {
		t.Fatalf("deletions = current %d voltage %d, want defaults %d/%d", ecu.kersCurrent, ecu.kersVoltage, DefaultKersCurrent, DefaultKersVoltage)
	}
}

func TestRedisSettingsReplaceAndClearEventsReloadAllFields(t *testing.T) {
	tx, mr := newTestTx(t)
	mr.HSet("settings", "engine-ecu.boost", "true")
	mr.HSet("settings", "engine-ecu.kers", "disabled")
	mr.HSet("settings", "engine-ecu.kers-power", "20000")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	battery := &BatteryTracker{}
	ecu := newTestECU()
	ecu.kersCurrent = DefaultKersCurrent
	kers := newKERSControllerWithDelay(ctx, time.Millisecond, func(bool) {}, func(KERSReason) {})
	rx := newIPCRx(tx.client, newLogger(LogLevelNone), battery, kers, ecu)
	rx.Start()

	waitFor := func(desc string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}
	state := func() (boost bool, current uint16, disabled bool) {
		ecu.mu.RLock()
		boost, current = ecu.boostEnabled, ecu.kersCurrent
		ecu.mu.RUnlock()
		kers.mu.Lock()
		disabled = kers.settingsDisabled
		kers.mu.Unlock()
		return
	}
	waitFor("initial settings sync", func() bool {
		boost, current, disabled := state()
		return boost && current == 20000 && disabled
	})

	pipe := tx.raw.Pipeline()
	pipe.Del(ctx, "settings")
	pipe.Publish(ctx, "settings", "cleared")
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor("clear defaults", func() bool {
		boost, current, disabled := state()
		return !boost && current == DefaultKersCurrent && !disabled
	})

	pipe = tx.raw.Pipeline()
	pipe.HSet(ctx, "settings", map[string]any{
		"engine-ecu.boost":      "true",
		"engine-ecu.kers-power": "22000",
	})
	pipe.Publish(ctx, "settings", "replaced")
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor("replacement reload", func() bool {
		boost, current, disabled := state()
		return boost && current == 22000 && !disabled
	})
}

func TestRedisHDelNotificationsResetBatteryAndSettings(t *testing.T) {
	tx, mr := newTestTx(t)
	mr.HSet("battery:0", "state", "active")
	mr.HSet("battery:0", "temperature-state", "ideal")
	mr.HSet("settings", "engine-ecu.kers-power", "20000")
	mr.HSet("settings", "engine-ecu.kers-voltage", "54000")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	battery := &BatteryTracker{}
	ecu := newTestECU()
	ecu.kersCurrent = DefaultKersCurrent
	ecu.kersVoltage = DefaultKersVoltage
	kers := newKERSControllerWithDelay(ctx, time.Millisecond, func(bool) {}, func(KERSReason) {})
	rx := newIPCRx(tx.client, newLogger(LogLevelNone), battery, kers, ecu)
	rx.Start()

	kersSettings := func() (uint16, uint16) {
		ecu.mu.RLock()
		defer ecu.mu.RUnlock()
		return ecu.kersCurrent, ecu.kersVoltage
	}
	current, voltage := kersSettings()
	if battery.ActiveTempState() != TempIdeal || current != 20000 || voltage != 54000 {
		t.Fatalf("initial sync failed: temp=%v current=%d voltage=%d", battery.ActiveTempState(), current, voltage)
	}

	pipe := tx.raw.Pipeline()
	pipe.HDel(ctx, "battery:0", "state")
	pipe.Publish(ctx, "battery:0", "state")
	pipe.HDel(ctx, "settings", "engine-ecu.kers-power", "engine-ecu.kers-voltage")
	pipe.Publish(ctx, "settings", "engine-ecu.kers-power")
	pipe.Publish(ctx, "settings", "engine-ecu.kers-voltage")
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, voltage = kersSettings()
		if battery.ActiveTempState() == TempUnknown && current == DefaultKersCurrent && voltage == DefaultKersVoltage {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	current, voltage = kersSettings()
	t.Fatalf("HDEL did not reset state: temp=%v current=%d voltage=%d", battery.ActiveTempState(), current, voltage)
}
