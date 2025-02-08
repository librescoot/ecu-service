package main

import (
	"log"
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
	log         *log.Logger
	batteryData [BatteryCount]BatteryState
	mu          sync.RWMutex
}

func NewBattery(logger *log.Logger) *Battery {
	return &Battery{
		log: logger,
	}
}

func (b *Battery) Destroy() {}

func (b *Battery) Update(idx uint, data BatteryState) {
    b.mu.Lock()
    defer b.mu.Unlock()

    b.log.Printf("DETAILED: Updating battery %d with state: active=%v, temperature_state=%v", idx, data.Active, data.TemperatureState)

    if idx >= BatteryCount {
        b.log.Printf("Invalid battery index: %d (num batteries: %d)", idx, BatteryCount)
        return
    }

    b.batteryData[idx] = data
}

func (b *Battery) GetActiveTemperatureState() BatteryTemperatureState {
    b.mu.RLock()
    defer b.mu.RUnlock()

    b.log.Printf("DETAILED: GetActiveTemperatureState called")
    b.log.Printf("DETAILED: Battery 0 state: active=%v, temperature_state=%v", b.batteryData[0].Active, b.batteryData[0].TemperatureState)
    b.log.Printf("DETAILED: Battery 1 state: active=%v, temperature_state=%v", b.batteryData[1].Active, b.batteryData[1].TemperatureState)

    // If battery 0 is active, return its temperature state
    if b.batteryData[0].Active && !b.batteryData[1].Active {
        b.log.Printf("bat 0 active")
        return b.batteryData[0].TemperatureState
    }

    // If battery 1 is active, return its temperature state
    if b.batteryData[1].Active && !b.batteryData[0].Active {
        b.log.Printf("bat 1 active")
        return b.batteryData[1].TemperatureState
    }

    // If no batteries are active or both are active, return unknown
    return BatteryTemperatureStateUnknown
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
