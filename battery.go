package main

import (
	"sync"
)

const BatteryCount = 2

type BatteryTemperatureState int

const (
	BatteryTemperatureStateUnknown BatteryTemperatureState = iota
	BatteryTemperatureStateCold
	BatteryTemperatureStateHot
	BatteryTemperatureStateIdeal
)

type BatteryState struct {
	Active           bool
	TemperatureState BatteryTemperatureState
}

type Battery struct {
	log         *LeveledLogger
	batteryData [BatteryCount]BatteryState
	mu          sync.RWMutex
}

func NewBattery(logger *LeveledLogger) *Battery {
	return &Battery{
		log: logger,
	}
}

func (b *Battery) Destroy() {}

func (b *Battery) Update(idx uint, data BatteryState) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.log.Debug("Updating battery %d with state: active=%v, temperature_state=%v", idx, data.Active, data.TemperatureState)

	if idx >= BatteryCount {
		b.log.Error("Invalid battery index: %d (num batteries: %d)", idx, BatteryCount)
		return
	}

	b.batteryData[idx] = data
}

func (b *Battery) GetActiveTemperatureState() BatteryTemperatureState {
	b.mu.RLock()
	defer b.mu.RUnlock()

	b.log.Debug("GetActiveTemperatureState called")
	b.log.Debug("Battery 0 state: active=%v, temperature_state=%v", b.batteryData[0].Active, b.batteryData[0].TemperatureState)
	b.log.Debug("Battery 1 state: active=%v, temperature_state=%v", b.batteryData[1].Active, b.batteryData[1].TemperatureState)

	b0 := b.batteryData[0]
	b1 := b.batteryData[1]

	if b0.Active && !b1.Active {
		return b0.TemperatureState
	}

	if b1.Active && !b0.Active {
		return b1.TemperatureState
	}

	// Both active: return the most restrictive state (lowest enum value).
	// Enum order: Unknown(0) < Cold(1) < Hot(2) < Ideal(3)
	if b0.Active && b1.Active {
		if b0.TemperatureState < b1.TemperatureState {
			return b0.TemperatureState
		}
		return b1.TemperatureState
	}

	// Neither active
	return BatteryTemperatureStateUnknown
}

func (b *Battery) BothActive() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.batteryData[0].Active && b.batteryData[1].Active
}

func (b *Battery) stringifyTemperatureState(state BatteryTemperatureState) string {
	switch state {
	case BatteryTemperatureStateCold:
		return "cold"
	case BatteryTemperatureStateHot:
		return "hot"
	case BatteryTemperatureStateIdeal:
		return "ideal"
	case BatteryTemperatureStateUnknown:
		fallthrough
	default:
		return "unknown"
	}
}
