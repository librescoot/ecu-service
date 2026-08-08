package main

import (
	"fmt"
	"strconv"
	"sync"

	ipc "github.com/librescoot/redis-ipc"
)

type IPCRx struct {
	client  *ipc.Client
	log     *Logger
	battery *BatteryTracker
	kers    *KERSController
	ecu     *ECU

	// KERS power (current) settings; applyKersPower picks single vs dual based
	// on how many batteries are active.
	mu              sync.Mutex
	kersPowerSingle uint16
	kersPowerDual   uint16
	hasDualPower    bool
}

func newIPCRx(client *ipc.Client, log *Logger, battery *BatteryTracker, kers *KERSController, ecu *ECU) *IPCRx {
	return &IPCRx{
		client:          client,
		log:             log,
		battery:         battery,
		kers:            kers,
		ecu:             ecu,
		kersPowerSingle: DefaultKersCurrent,
		kersPowerDual:   DefaultKersCurrent,
	}
}

// Start subscribes to Redis channels and syncs initial state.
// Watches are started in background goroutines; this call returns immediately.
func (rx *IPCRx) Start() {
	rx.watchVehicle()
	rx.watchBattery(0)
	rx.watchBattery(1)
	rx.watchSettings()
}

func (rx *IPCRx) watchVehicle() {
	w := rx.client.NewHashWatcher("vehicle")
	w.OnField("state", func(state string) error {
		rx.log.Info("Vehicle state: %s", state)
		rx.kers.SetReadyToDrive(state == "ready-to-drive")
		return nil
	})
	if err := w.StartWithSync(); err != nil {
		rx.log.Error("vehicle watcher: %v", err)
	}
}

func (rx *IPCRx) watchBattery(idx int) {
	key := fmt.Sprintf("battery:%d", idx)
	w := rx.client.NewHashWatcher(key)

	// Any field change triggers a full re-read of the battery state.
	w.OnAny(func(_, _ string) error {
		return rx.readBattery(idx)
	})

	if err := w.StartWithSync(); err != nil {
		rx.log.Error("battery:%d watcher: %v", idx, err)
	}
}

func (rx *IPCRx) readBattery(idx int) error {
	key := fmt.Sprintf("battery:%d", idx)
	fields, err := rx.client.HGetAll(key)
	if err != nil {
		return fmt.Errorf("HGetAll %s: %w", key, err)
	}
	active := fields["state"] == "active"
	temp := parseTempState(fields["temperature-state"])
	rx.log.Debug("Battery %d: active=%v temp=%s", idx, active, fields["temperature-state"])
	rx.battery.SetState(idx, active, temp)
	rx.kers.SetTempState(rx.battery.ActiveTempState())
	// Battery count may have changed; re-pick single vs dual KERS power.
	rx.applyKersPower()
	return nil
}

func (rx *IPCRx) watchSettings() {
	w := rx.client.NewHashWatcher("settings")
	w.OnField("engine-ecu.boost", func(val string) error {
		rx.log.Info("Boost setting: %s", val)
		rx.ecu.SetBoostEnabled(val == "true")
		return nil
	})
	// KERS enable/disable; defaults to enabled when unset or any non-"false".
	w.OnField("engine-ecu.kers", func(val string) error {
		enabled := val != "false"
		rx.log.Info("KERS enabled setting: %s (enabled=%v)", val, enabled)
		rx.kers.SetSettingsEnabled(enabled)
		return nil
	})
	w.OnField("engine-ecu.kers-power", func(val string) error {
		mA, err := strconv.Atoi(val)
		if err != nil {
			rx.log.Error("invalid engine-ecu.kers-power %q: %v", val, err)
			return nil
		}
		rx.mu.Lock()
		rx.kersPowerSingle = uint16(mA)
		rx.mu.Unlock()
		rx.applyKersPower()
		return nil
	})
	w.OnField("engine-ecu.kers-power-dual", func(val string) error {
		mA, err := strconv.Atoi(val)
		if err != nil {
			rx.log.Error("invalid engine-ecu.kers-power-dual %q: %v", val, err)
			return nil
		}
		rx.mu.Lock()
		rx.kersPowerDual = uint16(mA)
		rx.hasDualPower = true
		rx.mu.Unlock()
		rx.applyKersPower()
		return nil
	})
	w.OnField("engine-ecu.kers-voltage", func(val string) error {
		mV, err := strconv.Atoi(val)
		if err != nil {
			rx.log.Error("invalid engine-ecu.kers-voltage %q: %v", val, err)
			return nil
		}
		rx.ecu.SetKersVoltage(uint16(mV))
		return nil
	})
	if err := w.StartWithSync(); err != nil {
		rx.log.Error("settings watcher: %v", err)
	}
}

// applyKersPower picks the single- or dual-battery KERS current based on how
// many batteries are active and pushes it to the ECU. The dual value is only
// used once it has been explicitly configured.
func (rx *IPCRx) applyKersPower() {
	rx.mu.Lock()
	single, dual, hasDual := rx.kersPowerSingle, rx.kersPowerDual, rx.hasDualPower
	rx.mu.Unlock()

	current := single
	if hasDual && rx.battery.DualActive() {
		current = dual
	}
	rx.ecu.SetKersCurrent(current)
}
