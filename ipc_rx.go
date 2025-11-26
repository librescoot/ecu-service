package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-redis/redis/v8"
)

const IpcRxBatteryNameSize = 16

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

func (rx *IPCRx) setupSubscriptions() error {
	// Subscribe to vehicle updates
	rx.vehicleSubscription = rx.redis.Subscribe(rx.ctx, "vehicle")

	// Start vehicle handler
	go rx.handleVehicleSubscription()

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

			// Check if state was updated
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
}

func (rx *IPCRx) handleVehicleState(state string) {
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
}
