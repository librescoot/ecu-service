package main

import "sync"

type TempState int

const (
	TempUnknown TempState = iota
	TempCold
	TempHot
	TempIdeal
)

func parseTempState(s string) TempState {
	switch s {
	case "cold":
		return TempCold
	case "hot":
		return TempHot
	case "ideal":
		return TempIdeal
	default:
		return TempUnknown
	}
}

type batteryState struct {
	active    bool
	tempState TempState
}

type BatteryTracker struct {
	mu     sync.Mutex
	states [2]batteryState
}

// SetState updates the state for battery at index idx (0 or 1).
func (b *BatteryTracker) SetState(idx int, active bool, temp TempState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.states[idx] = batteryState{active: active, tempState: temp}
}

// DualActive reports whether two or more batteries are active, which selects
// the dual-battery KERS current.
func (b *BatteryTracker) DualActive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, s := range b.states {
		if s.active {
			n++
		}
	}
	return n >= 2
}

// ActiveTempState returns the conservative superset of all active batteries'
// temperature states. TempIdeal is returned only if every active battery is
// ideal; any non-ideal state takes precedence. Returns TempUnknown if no
// battery is active.
func (b *BatteryTracker) ActiveTempState() TempState {
	b.mu.Lock()
	defer b.mu.Unlock()

	result := TempUnknown
	anyActive := false
	for _, s := range b.states {
		if !s.active {
			continue
		}
		if !anyActive {
			anyActive = true
			result = s.tempState
		} else if s.tempState != TempIdeal {
			result = s.tempState
		}
	}
	return result
}
