package main

import (
	"context"
	"sync"
	"time"
)

const (
	faultUpdateDelay  = 500 * time.Millisecond
	faultClearTimeout = 5 * time.Second
)

// Diagnostics tracks the active fault from the ECU and applies hysteresis:
//   - New fault: reported after 500ms of stability (debounce transients).
//   - Fault clear: reported only after 5s of continuous FaultNone (prevents flapping).
type Diagnostics struct {
	mu            sync.Mutex
	currentFault  Fault // committed to Redis
	pendingFault  Fault // currently being reported by ECU
	updateTimer   *time.Timer
	clearTimer    *time.Timer
	onFaultChange func(fault Fault, cfg FaultConfig)
	log           *Logger
}

func newDiagnostics(ctx context.Context, log *Logger, onFaultChange func(Fault, FaultConfig)) *Diagnostics {
	d := &Diagnostics{
		log:           log,
		onFaultChange: onFaultChange,
	}
	d.updateTimer = time.NewTimer(faultUpdateDelay)
	d.updateTimer.Stop()
	d.clearTimer = time.NewTimer(faultClearTimeout)
	d.clearTimer.Stop()

	go d.timerLoop(ctx)
	return d
}

func (d *Diagnostics) timerLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return

		case <-d.updateTimer.C:
			d.mu.Lock()
			f := d.pendingFault
			if f != FaultNone && f != d.currentFault {
				d.currentFault = f
				cfg := faultConfigs[f]
				d.mu.Unlock()
				d.log.Warn("Fault committed: code=%d (%s)", f, cfg.Description)
				d.onFaultChange(f, cfg)
			} else {
				d.mu.Unlock()
			}

		case <-d.clearTimer.C:
			d.mu.Lock()
			if d.pendingFault == FaultNone && d.currentFault != FaultNone {
				d.currentFault = FaultNone
				d.mu.Unlock()
				d.log.Info("Fault cleared")
				d.onFaultChange(FaultNone, FaultConfig{})
			} else {
				d.mu.Unlock()
			}
		}
	}
}

// Update is called on every Status2 frame with the raw fault code from the ECU.
func (d *Diagnostics) Update(code uint32) {
	fault, _ := MapFault(code)

	d.mu.Lock()
	defer d.mu.Unlock()

	if fault == d.currentFault {
		// Fault state unchanged. If the fault is active and a clear countdown
		// is running (briefly got FaultNone earlier), cancel it.
		if fault != FaultNone {
			if !d.clearTimer.Stop() {
				select {
				case <-d.clearTimer.C:
				default:
				}
			}
			d.clearTimer.Reset(faultClearTimeout)
		}
		return
	}

	// Fault differs from what we've committed.
	d.pendingFault = fault

	if fault == FaultNone {
		// Fault disappeared — wait 5s before clearing.
		if !d.updateTimer.Stop() {
			select {
			case <-d.updateTimer.C:
			default:
			}
		}
		if !d.clearTimer.Stop() {
			select {
			case <-d.clearTimer.C:
			default:
			}
		}
		d.clearTimer.Reset(faultClearTimeout)
	} else {
		// New fault — commit after 500ms of stability.
		if !d.clearTimer.Stop() {
			select {
			case <-d.clearTimer.C:
			default:
			}
		}
		if !d.updateTimer.Stop() {
			select {
			case <-d.updateTimer.C:
			default:
			}
		}
		d.updateTimer.Reset(faultUpdateDelay)
	}
}
