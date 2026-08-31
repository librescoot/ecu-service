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
		rx.ecu.SetParked(state == "parked")
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
	w.OnField("engine-ecu.boost", rx.handleBoostSetting)
	w.OnField("engine-ecu.kers", rx.handleKersEnabledSetting)
	w.OnField("engine-ecu.kers-power", rx.handleKersPowerSingle)
	w.OnField("engine-ecu.kers-power-dual", rx.handleKersPowerDual)
	w.OnField("engine-ecu.kers-voltage", rx.handleKersVoltage)
	// HashPublisher.ReplaceAll/Clear publish event-only messages rather than
	// one event per field. Re-read the complete hash so absent values receive
	// the same defaults as individual HDEL notifications.
	w.OnEvent("replaced", rx.readSettings)
	w.OnEvent("cleared", rx.readSettings)
	if err := w.StartWithSync(); err != nil {
		rx.log.Error("settings watcher: %v", err)
	}
}

func (rx *IPCRx) readSettings() error {
	fields, err := rx.client.HGetAll("settings")
	if err != nil {
		return fmt.Errorf("read settings: %w", err)
	}
	if err := rx.handleBoostSetting(fields["engine-ecu.boost"]); err != nil {
		return err
	}
	if err := rx.handleKersEnabledSetting(fields["engine-ecu.kers"]); err != nil {
		return err
	}
	if err := rx.handleKersPowerSingle(fields["engine-ecu.kers-power"]); err != nil {
		return err
	}
	if err := rx.handleKersPowerDual(fields["engine-ecu.kers-power-dual"]); err != nil {
		return err
	}
	return rx.handleKersVoltage(fields["engine-ecu.kers-voltage"])
}

func (rx *IPCRx) handleBoostSetting(val string) error {
	rx.log.Info("Boost setting: %s", val)
	rx.ecu.SetBoostEnabled(val == "true")
	return nil
}

func (rx *IPCRx) handleKersEnabledSetting(val string) error {
	enabled := kersSettingEnabled(val)
	rx.log.Info("KERS enabled setting: %s (enabled=%v)", val, enabled)
	rx.kers.SetSettingsEnabled(enabled)
	return nil
}

func parseKersUint16(field, val string) (uint16, error) {
	parsed, err := strconv.ParseUint(val, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", field, val, err)
	}
	return uint16(parsed), nil
}

func (rx *IPCRx) handleKersPowerSingle(val string) error {
	mA := DefaultKersCurrent
	if val != "" {
		parsed, err := parseKersUint16("engine-ecu.kers-power", val)
		if err != nil {
			rx.log.Error("%v", err)
			return nil
		}
		mA = parsed
	}
	rx.mu.Lock()
	rx.kersPowerSingle = mA
	rx.mu.Unlock()
	rx.applyKersPower()
	return nil
}

func (rx *IPCRx) handleKersPowerDual(val string) error {
	if val == "" {
		rx.mu.Lock()
		rx.kersPowerDual = DefaultKersCurrent
		rx.hasDualPower = false
		rx.mu.Unlock()
		rx.applyKersPower()
		return nil
	}
	mA, err := parseKersUint16("engine-ecu.kers-power-dual", val)
	if err != nil {
		rx.log.Error("%v", err)
		return nil
	}
	rx.mu.Lock()
	rx.kersPowerDual = mA
	rx.hasDualPower = true
	rx.mu.Unlock()
	rx.applyKersPower()
	return nil
}

func (rx *IPCRx) handleKersVoltage(val string) error {
	mV := DefaultKersVoltage
	if val != "" {
		parsed, err := parseKersUint16("engine-ecu.kers-voltage", val)
		if err != nil {
			rx.log.Error("%v", err)
			return nil
		}
		mV = parsed
	}
	rx.ecu.SetKersVoltage(mV)
	return nil
}

// kersSettingEnabled reads settings[engine-ecu.kers]. The schema declares the
// field an enum over "enabled"/"disabled", and that is what lsc, the BLE
// settings write and the docs all use, so "disabled" has to be the off value.
// "false" is accepted alongside it because that is what this service used to
// require, and scooters carrying it in their persisted settings would
// otherwise silently come back with regen enabled.
//
// Anything else, including an unset field, means enabled.
func kersSettingEnabled(val string) bool {
	return val != "disabled" && val != "false"
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
