package main

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/go-redis/redis/v8"
)

const IpcRxBatteryNameSize = 16

// BoostCallback is called when the boost setting changes
type BoostCallback func(enabled bool) error

// KersEnabledCallback is called when the KERS enabled/disabled setting changes
type KersEnabledCallback func(enabled bool)

// KersPowerCallback is called when the KERS power (current) setting changes
type KersPowerCallback func(current uint16) error

// KersVoltageCallback is called when the KERS voltage setting changes
type KersVoltageCallback func(voltage uint16) error

type IPCRx struct {
	log     *LeveledLogger
	redis   *redis.Client
	battery *Battery
	kers    *KERS
	mu      sync.RWMutex
	ctx     context.Context
	cancel  context.CancelFunc

	batterySubscriptions [BatteryCount]*redis.PubSub
	vehicleSubscription  *redis.PubSub
	settingsSubscription *redis.PubSub

	lastVehicleState string // Track previous state to avoid redundant processing

	boostCallback       BoostCallback
	kersEnabledCallback KersEnabledCallback
	kersPowerCallback   KersPowerCallback
	kersVoltageCallback KersVoltageCallback

	kersPowerSingle    uint16 // from settings:engine-ecu.kers-power
	kersPowerDual      uint16 // from settings:engine-ecu.kers-power-dual
	hasDualPower       bool   // true when kers-power-dual has been explicitly set
	lastAppliedCurrent uint16 // last value sent to ECU, to suppress redundant updates
}

func NewIPCRx(logger *LeveledLogger, redis *redis.Client, battery *Battery, kers *KERS) *IPCRx {
	ctx, cancel := context.WithCancel(context.Background())

	rx := &IPCRx{
		log:     logger,
		redis:   redis,
		battery: battery,
		kers:    kers,
		ctx:     ctx,
		cancel:  cancel,
	}

	// Setup initial subscriptions
	if err := rx.setupSubscriptions(); err != nil {
		rx.log.Error("Failed to setup subscriptions: %v", err)
		rx.Destroy()
		return nil
	}

	// Initial state reads
	rx.readInitialStates()

	return rx
}

func (rx *IPCRx) SetBoostCallback(callback BoostCallback) {
	rx.mu.Lock()
	rx.boostCallback = callback
	rx.mu.Unlock()

	// Read initial boost setting now that callback is set
	rx.handleBoostSetting()
}

func (rx *IPCRx) SetKersEnabledCallback(callback KersEnabledCallback) {
	rx.mu.Lock()
	rx.kersEnabledCallback = callback
	rx.mu.Unlock()

	rx.handleKersEnabledSetting()
}

func (rx *IPCRx) SetKersPowerCallback(callback KersPowerCallback) {
	rx.mu.Lock()
	rx.kersPowerCallback = callback
	rx.mu.Unlock()

	rx.handleKersPowerSetting()
	rx.handleKersPowerDualSetting()
}

func (rx *IPCRx) SetKersVoltageCallback(callback KersVoltageCallback) {
	rx.mu.Lock()
	rx.kersVoltageCallback = callback
	rx.mu.Unlock()

	rx.handleKersVoltageSetting()
}

func (rx *IPCRx) setupSubscriptions() error {
	// Subscribe to vehicle updates
	rx.vehicleSubscription = rx.redis.Subscribe(rx.ctx, "vehicle")

	// Start vehicle handler
	go rx.handleVehicleSubscription()

	// Subscribe to settings updates
	rx.settingsSubscription = rx.redis.Subscribe(rx.ctx, "settings")

	// Start settings handler
	go rx.handleSettingsSubscription()

	// Setup battery subscriptions
	for i := 0; i < BatteryCount; i++ {
		batteryChannel := fmt.Sprintf("battery:%d", i)
		rx.batterySubscriptions[i] = rx.redis.Subscribe(rx.ctx, batteryChannel)

		// Start battery handler
		go rx.handleBatterySubscription(i)
	}

	return nil
}

func (rx *IPCRx) handleVehicleSubscription() {
	rx.log.Info("Starting vehicle subscription handler")

	for {
		msg, err := rx.vehicleSubscription.Receive(rx.ctx)
		if err != nil {
			if err == context.Canceled {
				return
			}
			// Check for closed client - panic to trigger systemd restart
			if err.Error() == "redis: client is closed" {
				rx.log.Error("Redis connection lost on vehicle subscription - restarting service")
				panic("Redis disconnected")
			}
			rx.log.Error("Vehicle subscription error: %v", err)
			continue
		}

		switch m := msg.(type) {
		case *redis.Message:
			rx.log.Debug("Vehicle message received: channel=%s, payload=%s", m.Channel, m.Payload)

			// Only process state change notifications; ignore seatbox, brake, blinker, etc.
			if m.Payload != "state" {
				continue
			}

			state, err := rx.redis.HGet(rx.ctx, "vehicle", "state").Result()
			if err != nil && err != redis.Nil {
				rx.log.Error("Failed to get vehicle state: %v", err)
				continue
			}

			if err != redis.Nil {
				rx.handleVehicleState(state)
			}

		case *redis.Subscription:
			rx.log.Debug("Vehicle subscription event: %s %s", m.Channel, m.Kind)
		}
	}
}

func (rx *IPCRx) handleSettingsSubscription() {
	rx.log.Info("Starting settings subscription handler")

	for {
		msg, err := rx.settingsSubscription.Receive(rx.ctx)
		if err != nil {
			if err == context.Canceled {
				return
			}
			// Check for closed client - panic to trigger systemd restart
			if err.Error() == "redis: client is closed" {
				rx.log.Error("Redis connection lost on settings subscription - restarting service")
				panic("Redis disconnected")
			}
			rx.log.Error("Settings subscription error: %v", err)
			continue
		}

		switch m := msg.(type) {
		case *redis.Message:
			rx.log.Debug("Settings message received: channel=%s, payload=%s", m.Channel, m.Payload)

			// Payload contains the key that changed
			switch m.Payload {
			case "engine-ecu.boost":
				rx.handleBoostSetting()
			case "engine-ecu.kers":
				rx.handleKersEnabledSetting()
			case "engine-ecu.kers-power":
				rx.handleKersPowerSetting()
			case "engine-ecu.kers-power-dual":
				rx.handleKersPowerDualSetting()
			case "engine-ecu.kers-voltage":
				rx.handleKersVoltageSetting()
			}

		case *redis.Subscription:
			rx.log.Debug("Settings subscription event: %s %s", m.Channel, m.Kind)
		}
	}
}

func (rx *IPCRx) handleBoostSetting() {
	value, err := rx.redis.HGet(rx.ctx, "settings", "engine-ecu.boost").Result()
	if err != nil {
		if err != redis.Nil {
			rx.log.Error("Failed to get boost setting: %v", err)
		}
		return
	}

	enabled := value == "true"
	rx.log.Info("Boost setting changed: %s (enabled=%v)", value, enabled)

	rx.mu.RLock()
	callback := rx.boostCallback
	rx.mu.RUnlock()

	if callback != nil {
		if err := callback(enabled); err != nil {
			rx.log.Error("Failed to set boost: %v", err)
		}
	}
}

func (rx *IPCRx) handleKersEnabledSetting() {
	value, err := rx.redis.HGet(rx.ctx, "settings", "engine-ecu.kers").Result()
	if err != nil {
		if err != redis.Nil {
			rx.log.Error("Failed to get KERS enabled setting: %v", err)
		}
		// Default: KERS enabled
		return
	}

	enabled := value != "disabled"
	rx.log.Info("KERS enabled setting changed: %s (enabled=%v)", value, enabled)

	rx.mu.RLock()
	callback := rx.kersEnabledCallback
	rx.mu.RUnlock()

	if callback != nil {
		callback(enabled)
	}
}

func (rx *IPCRx) handleKersPowerSetting() {
	value, err := rx.redis.HGet(rx.ctx, "settings", "engine-ecu.kers-power").Result()
	if err != nil {
		if err != redis.Nil {
			rx.log.Error("Failed to get KERS power setting: %v", err)
		}
		return
	}

	current, err := strconv.ParseUint(value, 10, 16)
	if err != nil {
		rx.log.Error("Invalid KERS power value '%s': %v", value, err)
		return
	}

	rx.log.Info("KERS power setting changed: %d mA", current)

	rx.mu.Lock()
	rx.kersPowerSingle = uint16(current)
	rx.applyKersPower()
	rx.mu.Unlock()
}

func (rx *IPCRx) handleKersPowerDualSetting() {
	value, err := rx.redis.HGet(rx.ctx, "settings", "engine-ecu.kers-power-dual").Result()
	if err != nil {
		if err != redis.Nil {
			rx.log.Error("Failed to get KERS dual power setting: %v", err)
		}
		// Not set = fall back to single power value
		rx.mu.Lock()
		rx.hasDualPower = false
		rx.applyKersPower()
		rx.mu.Unlock()
		return
	}

	current, err := strconv.ParseUint(value, 10, 16)
	if err != nil {
		rx.log.Error("Invalid KERS dual power value '%s': %v", value, err)
		return
	}

	rx.log.Info("KERS dual power setting changed: %d mA", current)

	rx.mu.Lock()
	rx.kersPowerDual = uint16(current)
	rx.hasDualPower = true
	rx.applyKersPower()
	rx.mu.Unlock()
}

// applyKersPower determines the correct KERS current based on battery state
// and forwards it to the ECU. Must be called with rx.mu held.
func (rx *IPCRx) applyKersPower() {
	bothActive := rx.battery.BothActive()

	var current uint16
	if bothActive && rx.hasDualPower {
		current = rx.kersPowerDual
		rx.log.Info("Dual battery active -> using dual KERS power: %d mA", current)
	} else {
		current = rx.kersPowerSingle
		if rx.hasDualPower {
			rx.log.Info("Single battery active -> using single KERS power: %d mA (dual configured: %d mA)", current, rx.kersPowerDual)
		}
	}

	if current == rx.lastAppliedCurrent {
		return
	}
	rx.lastAppliedCurrent = current

	callback := rx.kersPowerCallback
	if callback != nil && current > 0 {
		if err := callback(current); err != nil {
			rx.log.Error("Failed to set KERS power: %v", err)
		}
	}
}

func (rx *IPCRx) handleKersVoltageSetting() {
	value, err := rx.redis.HGet(rx.ctx, "settings", "engine-ecu.kers-voltage").Result()
	if err != nil {
		if err != redis.Nil {
			rx.log.Error("Failed to get KERS voltage setting: %v", err)
		}
		return
	}

	voltage, err := strconv.ParseUint(value, 10, 16)
	if err != nil {
		rx.log.Error("Invalid KERS voltage value '%s': %v", value, err)
		return
	}

	rx.log.Info("KERS voltage setting changed: %d mV", voltage)

	rx.mu.RLock()
	callback := rx.kersVoltageCallback
	rx.mu.RUnlock()

	if callback != nil {
		if err := callback(uint16(voltage)); err != nil {
			rx.log.Error("Failed to set KERS voltage: %v", err)
		}
	}
}

func (rx *IPCRx) handleBatterySubscription(idx int) {
	rx.log.Info("Starting battery %d subscription handler", idx)

	for {
		msg, err := rx.batterySubscriptions[idx].Receive(rx.ctx)
		if err != nil {
			if err == context.Canceled {
				return
			}
			// Check for closed client - panic to trigger systemd restart
			if err.Error() == "redis: client is closed" {
				rx.log.Error("Redis connection lost on battery %d subscription - restarting service", idx)
				panic("Redis disconnected")
			}
			rx.log.Error("Battery %d subscription error: %v", idx, err)
			continue
		}

		switch m := msg.(type) {
		case *redis.Message:
			rx.log.Debug("Battery %d message received: channel=%s, payload=%s", idx, m.Channel, m.Payload)

			batteryKey := fmt.Sprintf("battery:%d", idx)
			state := BatteryState{}

			// Get current state first
			currentState, err := rx.redis.HGetAll(rx.ctx, batteryKey).Result()
			if err != nil && err != redis.Nil {
				rx.log.Error("Failed to get battery %d current state: %v", idx, err)
				continue
			}

			// Update state based on current values
			if active, ok := currentState["state"]; ok {
				state.Active = (active == "active")
			}
			if tempState, ok := currentState["temperature-state"]; ok {
				switch tempState {
				case "cold":
					state.TemperatureState = BatteryTemperatureStateCold
				case "hot":
					state.TemperatureState = BatteryTemperatureStateHot
				case "ideal":
					state.TemperatureState = BatteryTemperatureStateIdeal
				default:
					state.TemperatureState = BatteryTemperatureStateUnknown
				}
			}

			// Update battery state
			rx.battery.Update(uint(idx), state)

			// Update KERS based on active battery temperature state
			rx.kers.UpdateBattery(rx.battery.GetActiveTemperatureState())

			// Re-evaluate KERS power (single vs dual battery)
			rx.mu.Lock()
			rx.applyKersPower()
			rx.mu.Unlock()

		case *redis.Subscription:
			rx.log.Debug("Battery subscription event: %s %s", m.Channel, m.Kind)
		}
	}
}

func (rx *IPCRx) readInitialStates() {
	// Read vehicle state
	state, err := rx.redis.HGet(rx.ctx, "vehicle", "state").Result()
	if err != nil && err != redis.Nil {
		rx.log.Error("Failed to read initial vehicle state: %v", err)
	} else {
		rx.log.Info("Initial vehicle state: %s", state)
		rx.handleVehicleState(state)
	}

	// Read initial boost setting
	rx.handleBoostSetting()

	// Read battery states
	for i := 0; i < BatteryCount; i++ {
		batteryKey := fmt.Sprintf("battery:%d", i)
		batteryState := BatteryState{}

		state, err := rx.redis.HGet(rx.ctx, batteryKey, "state").Result()
		if err != nil && err != redis.Nil {
			rx.log.Error("Failed to read initial battery %d state: %v", i, err)
		} else {
			rx.log.Info("Initial battery %d state: %s", i, state)
			batteryState.Active = (state == "active")
		}

		tempState, err := rx.redis.HGet(rx.ctx, batteryKey, "temperature-state").Result()
		if err != nil && err != redis.Nil {
			rx.log.Error("Failed to read initial battery %d temperature state: %v", i, err)
		} else {
			rx.log.Info("Initial battery %d temperature state: %s", i, tempState)
			switch tempState {
			case "cold":
				batteryState.TemperatureState = BatteryTemperatureStateCold
			case "hot":
				batteryState.TemperatureState = BatteryTemperatureStateHot
			case "ideal":
				batteryState.TemperatureState = BatteryTemperatureStateIdeal
			default:
				batteryState.TemperatureState = BatteryTemperatureStateUnknown
			}
		}

		// Update battery state
		rx.battery.Update(uint(i), batteryState)
	}

	// Update KERS with initial battery state
	rx.kers.UpdateBattery(rx.battery.GetActiveTemperatureState())

	// Apply initial KERS power based on battery configuration
	rx.mu.Lock()
	rx.applyKersPower()
	rx.mu.Unlock()
}

func (rx *IPCRx) handleVehicleState(state string) {
	rx.mu.Lock()
	if state == rx.lastVehicleState {
		rx.mu.Unlock()
		return
	}
	rx.lastVehicleState = state
	rx.mu.Unlock()

	var vehicleState VehicleState
	if state == "ready-to-drive" {
		vehicleState = VehicleStateEngineReady
		rx.log.Info("Vehicle state changed to: ready-to-drive")
	} else {
		vehicleState = VehicleStateEngineNotReady
		rx.log.Info("Vehicle state changed to: %s", state)
	}

	rx.kers.HandleVehicleStateChange(vehicleState)
}

func (rx *IPCRx) Destroy() {
	rx.mu.Lock()
	defer rx.mu.Unlock()

	if rx.cancel != nil {
		rx.cancel()
	}

	for i := 0; i < BatteryCount; i++ {
		if rx.batterySubscriptions[i] != nil {
			rx.batterySubscriptions[i].Close()
		}
	}

	if rx.vehicleSubscription != nil {
		rx.vehicleSubscription.Close()
	}

	if rx.settingsSubscription != nil {
		rx.settingsSubscription.Close()
	}
}
