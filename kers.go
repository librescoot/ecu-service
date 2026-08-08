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
	KERSReasonNone KERSReason = "none"
	KERSReasonCold KERSReason = "cold"
	KERSReasonHot  KERSReason = "hot"
)

// KERSController decides when regen (KERS) may be enabled. It mirrors the
// production v1 gating: changes are only applied while the vehicle is stopped
// and the engine is ready, the engine-on write is deferred ~1.5s for the ECU to
// initialize, a user setting can force KERS off, and an ECU that re-enables KERS
// despite a non-none reason is reconciled back off. As an explicit safety
// belt (a v2 addition over v1), KERS is also disabled immediately when the
// vehicle leaves ready-to-drive.
type KERSController struct {
	mu               sync.Mutex
	temperatureState TempState
	reason           KERSReason
	vehicleStopped   bool
	engineReady      bool
	settingsDisabled bool
	enabled          bool
	engineOnTimer    *time.Timer

	onEnable func(bool)       // send the ECU command + publish kers
	onReason func(KERSReason) // store + publish kers-reason-off
}

func newKERSController(ctx context.Context, onEnable func(bool), onReason func(KERSReason)) *KERSController {
	k := &KERSController{
		temperatureState: TempUnknown,
		reason:           KERSReasonNone,
		vehicleStopped:   true,
		onEnable:         onEnable,
		onReason:         onReason,
	}
	k.engineOnTimer = time.NewTimer(kersEngineOnDelay)
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
			k.engineReady = true
			k.updateKers()
			k.mu.Unlock()
		}
	}
}

// updateKers recomputes the KERS reason from temperature and, while the vehicle
// is stopped, publishes the reason and (when engine-ready) applies the enable
// decision. Gating both on "stopped" means a temperature or settings change
// mid-ride applies at the next stop rather than altering regen feel while
// moving. Must be called with mu held; releases and re-acquires mu around the
// callbacks.
func (k *KERSController) updateKers() {
	var reason KERSReason
	switch k.temperatureState {
	case TempCold:
		reason = KERSReasonCold
	case TempHot:
		reason = KERSReasonHot
	case TempIdeal:
		reason = KERSReasonNone
	case TempUnknown:
		return // wait for a known temperature state
	}
	k.reason = reason

	if !k.vehicleStopped {
		return
	}

	// Re-assert unconditionally (matching the OEM and v1): every update while
	// stopped+ready re-sends the enable/disable command, so the reconcile path
	// (UpdateECUKers) can force the ECU back off when it has drifted on. Gating
	// this on a local change flag would let a spurious ECU re-enable persist.
	callEnable := k.engineReady
	var newEnabled bool
	if callEnable {
		newEnabled = !k.settingsDisabled && reason == KERSReasonNone
		k.enabled = newEnabled
	}

	onEnable := k.onEnable
	onReason := k.onReason
	k.mu.Unlock()
	onReason(reason)
	if callEnable {
		onEnable(newEnabled)
	}
	k.mu.Lock()
}

// SetReadyToDrive arms the deferred engine-on timer when entering ready-to-drive
// and disables KERS immediately when leaving it.
func (k *KERSController) SetReadyToDrive(ready bool) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if !ready {
		k.engineOnTimer.Stop()
		k.engineReady = false
		if k.enabled {
			k.enabled = false
			k.mu.Unlock()
			k.onEnable(false)
			k.mu.Lock()
		}
		return
	}

	if k.engineReady {
		return
	}
	k.engineOnTimer.Stop()
	k.engineOnTimer.Reset(kersEngineOnDelay)
}

// SetTempState updates the active-battery temperature state and re-evaluates.
func (k *KERSController) SetTempState(temp TempState) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if temp == k.temperatureState {
		return
	}
	k.temperatureState = temp
	k.updateKers()
}

// SetSettingsEnabled toggles the user KERS enable setting and re-evaluates.
func (k *KERSController) SetSettingsEnabled(enabled bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	disabled := !enabled
	if k.settingsDisabled == disabled {
		return
	}
	k.settingsDisabled = disabled
	k.updateKers()
}

// UpdateVehicleStopped tracks whether the vehicle is stopped (speed == 0); KERS
// is only (re-)evaluated when coming to a stop.
func (k *KERSController) UpdateVehicleStopped(stopped bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if stopped == k.vehicleStopped {
		return
	}
	k.vehicleStopped = stopped
	if stopped {
		k.updateKers()
	}
}

// UpdateECUKers reconciles the ECU back off when it reports KERS enabled despite
// a non-none reason-off (e.g. the ECU re-enabled regen on its own).
func (k *KERSController) UpdateECUKers(ecuEnabled bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if ecuEnabled && k.reason != KERSReasonNone {
		k.updateKers()
	}
}
