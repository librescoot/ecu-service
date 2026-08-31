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

	// Fail-safe precedence is independent of slot order: hot outranks cold,
	// which outranks an unknown sensor/state, which outranks ideal.
	precedence := func(temp TempState) int {
		switch temp {
		case TempHot:
			return 4
		case TempCold:
			return 3
		case TempUnknown:
			return 2
		case TempIdeal:
			return 1
		default:
			return 2
		}
	}

	result := TempUnknown
	best := 0
	for _, s := range b.states {
		if s.active && precedence(s.tempState) > best {
			result = s.tempState
			best = precedence(s.tempState)
		}
	}
	return result
}
