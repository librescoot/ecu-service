package main

import (
	"context"
	"sync"
	"time"
)

const KersEngineOnDelayS = time.Second + 500*time.Millisecond

type KersReasonOff int

const (
	KersReasonOffNone KersReasonOff = iota
	KersReasonOffCold
	KersReasonOffHot
)

type VehicleState int

const (
	VehicleStateEngineNotReady VehicleState = iota
	VehicleStateEngineReady
)

type KERS struct {
	log              *LeveledLogger
	ipcTx            *IPCTx
	kersCallback     func(bool) error
	temperatureState BatteryTemperatureState
	kersReasonOff    KersReasonOff
	vehicleStopped   bool
	vehicleState     VehicleState
	engineOnTimer    *time.Timer
	mu               sync.RWMutex
	ctx              context.Context
}

func NewKERS(logger *LeveledLogger, ctx context.Context, ipcTx *IPCTx) *KERS {
	k := &KERS{
		log:              logger,
		ctx:              ctx,
		ipcTx:            ipcTx,
		temperatureState: BatteryTemperatureStateUnknown,
		kersReasonOff:    KersReasonOffNone,
		vehicleStopped:   true,
		vehicleState:     VehicleStateEngineNotReady,
	}

	k.engineOnTimer = time.NewTimer(KersEngineOnDelayS * time.Second)
	k.engineOnTimer.Stop()

	go k.timerLoop()

	return k
}

func (k *KERS) Destroy() {
	if k.engineOnTimer != nil {
		k.engineOnTimer.Stop()
	}
}

func (k *KERS) timerLoop() {
	for {
		select {
		case <-k.ctx.Done():
			return
		case <-k.engineOnTimer.C:
			k.mu.Lock()
			k.log.Printf("Engine ON (timer callback) -> updating KERS")
			k.vehicleState = VehicleStateEngineReady
			k.updateKers()
			k.mu.Unlock()
		}
	}
}

func (k *KERS) SetKersEnabledCallback(callback func(bool) error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	// Store the error-returning function directly
	k.kersCallback = callback
}

func (k *KERS) enableDisableKers(enable bool) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.kersCallback != nil {
		k.log.Printf("Setting ECU EBS to: %v", enable)
		if err := k.kersCallback(enable); err != nil {
			k.log.Printf("Error setting KERS: %v", err)
		}
	}
}

func (k *KERS) updateKers() {
	switch k.temperatureState {
	case BatteryTemperatureStateCold:
		k.kersReasonOff = KersReasonOffCold
	case BatteryTemperatureStateHot:
		k.kersReasonOff = KersReasonOffHot
	case BatteryTemperatureStateIdeal:
		k.kersReasonOff = KersReasonOffNone
	case BatteryTemperatureStateUnknown:
		k.log.Printf("update_kers: battery state 'unknown' -> not updating.")
		return
	}

	k.log.Printf("DETAILED updateKers: temperature=%s, vehicleStopped=%v, vehicleState=%v, kersReasonOff=%s",
		k.stringifyBatteryTemperatureState(),
		k.vehicleStopped,
		k.vehicleState,
		k.stringifyKersReasonOff())

	if k.vehicleStopped {
		k.log.Printf("Updating KERS: kers-reason-off=%s",
			k.stringifyKersReasonOff())

		if err := k.ipcTx.SendKersReasonOff(k.kersReasonOff); err != nil {
			k.log.Printf("Failed to send KERS reason off: %v", err)
		}

		if k.vehicleState == VehicleStateEngineReady {
			k.enableDisableKers(k.kersReasonOff == KersReasonOffNone)
		} else {
			k.log.Printf("ECU not enabled. Not setting KERS (yet).")
		}
	} else {
		k.log.Printf("Vehicle not stopped. Not updating KERS (yet)")
	}
}

func (k *KERS) UpdateBattery(state BatteryTemperatureState) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.log.Printf("DETAILED: UpdateBattery called with state: %v", state)
	k.log.Printf("DETAILED: Current temperature state: %v, Reason off: %v",
		k.temperatureState, k.kersReasonOff)

	if k.temperatureState == state {
		return
	}

	k.temperatureState = state
	k.log.Printf("Battery temperature-state updated: %s",
		k.stringifyBatteryTemperatureState())
	k.updateKers()
}

func (k *KERS) HandleVehicleStateChange(state VehicleState) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.log.Printf("DETAILED: HandleVehicleStateChange BEFORE - current state: %v, new state: %v",
		k.vehicleState, state)

	stateChanged := k.vehicleState != state
	k.log.Printf("Setting engine state to: %s",
		k.stringifyVehicleState())

	k.engineOnTimer.Stop()

	if stateChanged && state == VehicleStateEngineReady {
		k.log.Printf("Ready to drive -> awaiting 'Engine ON' ... (%.1f s)",
			KersEngineOnDelayS.Seconds())
		k.vehicleState = state // EXPLICITLY set the new state
		k.engineOnTimer.Reset(KersEngineOnDelayS)
	} else {
		k.vehicleState = state // Always set the state
	}

	k.log.Printf("DETAILED: HandleVehicleStateChange AFTER - current state: %v", k.vehicleState)

	// Force an update of KERS state
	k.updateKers()
}

func (k *KERS) UpdateVehicleStopped(stopped bool) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.log.Printf("DETAILED: UpdateVehicleStopped called with value: %v", stopped)
	k.log.Printf("DETAILED: Current vehicle state before update: stopped=%v, vehicleState=%v",
		k.vehicleStopped, k.vehicleState)

	stoppedChanged := k.vehicleStopped != stopped
	k.vehicleStopped = stopped

	if stoppedChanged && stopped {
		k.log.Printf("Vehicle stopped -> updating KERS")
		k.updateKers()
	}
}

func (k *KERS) UpdateECUKers(kersActive bool) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.log.Printf("ECU-kers is %s", map[bool]string{true: "enabled", false: "disabled"}[kersActive])

	if kersActive && (k.kersReasonOff != KersReasonOffNone) {
		k.log.Printf("ECU-kers is enabled, despite kers-reason-off=%s -> updating KERS",
			k.stringifyKersReasonOff())
		k.updateKers()
	}
}

func (k *KERS) stringifyKersReasonOff() string {
	switch k.kersReasonOff {
	case KersReasonOffCold:
		return "cold"
	case KersReasonOffHot:
		return "hot"
	case KersReasonOffNone:
		fallthrough
	default:
		return "none"
	}
}

func (k *KERS) stringifyBatteryTemperatureState() string {
	switch k.temperatureState {
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

func (k *KERS) stringifyVehicleState() string {
	if k.vehicleState == VehicleStateEngineReady {
		return "on"
	}
	return "off"
}
