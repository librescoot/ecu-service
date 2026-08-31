package main

import (
	"context"
	"sync"
	"time"
)

// kersEngineOnDelay defers the KERS write after entering ready-to-drive to give
// the ECU time to initialize before we send it regen config.
const kersEngineOnDelay = 1500 * time.Millisecond

type KERSReason string

const (
	KERSReasonNone    KERSReason = "none"
	KERSReasonCold    KERSReason = "cold"
	KERSReasonHot     KERSReason = "hot"
	KERSReasonUnknown KERSReason = "unknown"
)

// KERSController decides when regen (KERS) may be enabled. Changes are applied
// only while stopped and ready. Callbacks are always invoked without mu held.
type KERSController struct {
	mu               sync.Mutex
	actionsMu        sync.Mutex
	generation       uint64
	temperatureState TempState
	reason           KERSReason
	vehicleStopped   bool
	readyToDrive     bool
	engineReady      bool
	settingsDisabled bool
	enabled          bool
	engineOnDelay    time.Duration
	engineReadyAt    time.Time
	engineOnTimer    *time.Timer

	onEnable func(bool)
	onReason func(KERSReason)
}

type kersActions struct {
	generation uint64
	callReason bool
	reason     KERSReason
	callEnable bool
	enabled    bool
}

func newKERSController(ctx context.Context, onEnable func(bool), onReason func(KERSReason)) *KERSController {
	return newKERSControllerWithDelay(ctx, kersEngineOnDelay, onEnable, onReason)
}

func newKERSControllerWithDelay(ctx context.Context, delay time.Duration, onEnable func(bool), onReason func(KERSReason)) *KERSController {
	k := &KERSController{
		temperatureState: TempUnknown,
		reason:           KERSReasonUnknown,
		vehicleStopped:   true,
		engineOnDelay:    delay,
		onEnable:         onEnable,
		onReason:         onReason,
	}
	k.engineOnTimer = time.NewTimer(delay)
	k.engineOnTimer.Stop()
	go k.timerLoop(ctx)
	return k
}

func (k *KERSController) timerLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.engineOnTimer.C:
			k.mu.Lock()
			if !k.readyToDrive {
				k.mu.Unlock()
				continue
			}
			// A timer event from an earlier ready period may already have been
			// selected when a new period begins. Honor the new edge's complete
			// delay rather than enabling early from that stale event.
			if remaining := time.Until(k.engineReadyAt); remaining > 0 {
				k.engineOnTimer.Reset(remaining)
				k.mu.Unlock()
				continue
			}
			k.engineReady = true
			k.generation++
			actions := k.updateKersLocked()
			k.mu.Unlock()
			k.runActions(actions)
		}
	}
}

func reasonForTemp(temp TempState) KERSReason {
	switch temp {
	case TempCold:
		return KERSReasonCold
	case TempHot:
		return KERSReasonHot
	case TempIdeal:
		return KERSReasonNone
	default:
		return KERSReasonUnknown
	}
}

// updateKersLocked recomputes state and returns work to run after unlocking.
func (k *KERSController) updateKersLocked() kersActions {
	reason := reasonForTemp(k.temperatureState)
	k.reason = reason
	if !k.vehicleStopped {
		return kersActions{}
	}

	actions := kersActions{generation: k.generation, callReason: true, reason: reason}
	if k.engineReady {
		actions.callEnable = true
		actions.enabled = !k.settingsDisabled && reason == KERSReasonNone
		k.enabled = actions.enabled
	}
	return actions
}

func (k *KERSController) runActions(actions kersActions) {
	if !actions.callReason && !actions.callEnable {
		return
	}
	// State callbacks may originate from independent Redis, timer, and CAN
	// goroutines. Serialize their effects, and discard work computed before a
	// newer state transition, so an old enable can never run after a new inhibit.
	k.actionsMu.Lock()
	defer k.actionsMu.Unlock()
	k.mu.Lock()
	current := actions.generation == k.generation
	k.mu.Unlock()
	if !current {
		return
	}
	if actions.callReason && k.onReason != nil {
		k.onReason(actions.reason)
	}
	if actions.callEnable && k.onEnable != nil {
		k.onEnable(actions.enabled)
	}
}

func (k *KERSController) Reason() KERSReason {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reason
}

// SetReadyToDrive arms the deferred engine-on timer only on the false-to-true
// edge and disables KERS immediately on the true-to-false edge.
func (k *KERSController) SetReadyToDrive(ready bool) {
	k.mu.Lock()
	if ready == k.readyToDrive {
		k.mu.Unlock()
		return
	}
	k.readyToDrive = ready
	k.generation++
	if !ready {
		stopTimer(k.engineOnTimer)
		k.engineReadyAt = time.Time{}
		k.engineReady = false
		callDisable := k.enabled
		k.enabled = false
		actions := kersActions{generation: k.generation, callEnable: callDisable, enabled: false}
		k.mu.Unlock()
		k.runActions(actions)
		return
	}
	stopTimer(k.engineOnTimer)
	k.engineReadyAt = time.Now().Add(k.engineOnDelay)
	k.engineOnTimer.Reset(k.engineOnDelay)
	k.mu.Unlock()
}

func (k *KERSController) SetTempState(temp TempState) {
	k.mu.Lock()
	if temp == k.temperatureState {
		k.mu.Unlock()
		return
	}
	k.temperatureState = temp
	k.generation++
	actions := k.updateKersLocked()
	k.mu.Unlock()
	k.runActions(actions)
}

func (k *KERSController) SetSettingsEnabled(enabled bool) {
	k.mu.Lock()
	disabled := !enabled
	if k.settingsDisabled == disabled {
		k.mu.Unlock()
		return
	}
	k.settingsDisabled = disabled
	k.generation++
	actions := k.updateKersLocked()
	k.mu.Unlock()
	k.runActions(actions)
}

func (k *KERSController) UpdateVehicleStopped(stopped bool) {
	k.mu.Lock()
	if stopped == k.vehicleStopped {
		k.mu.Unlock()
		return
	}
	k.vehicleStopped = stopped
	k.generation++
	var actions kersActions
	if stopped {
		actions = k.updateKersLocked()
	}
	k.mu.Unlock()
	k.runActions(actions)
}

func (k *KERSController) UpdateECUKers(ecuEnabled bool) {
	k.mu.Lock()
	var actions kersActions
	if ecuEnabled && k.reason != KERSReasonNone {
		actions = k.updateKersLocked()
	}
	k.mu.Unlock()
	k.runActions(actions)
}
