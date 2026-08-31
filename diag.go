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
	pendingSince  time.Time
	updateTimer   *time.Timer
	clearTimer    *time.Timer
	onFaultChange func(fault Fault, cfg FaultConfig)
	log           *Logger
	updateDelay   time.Duration
	clearDelay    time.Duration
}

func newDiagnostics(ctx context.Context, log *Logger, onFaultChange func(Fault, FaultConfig)) *Diagnostics {
	return newDiagnosticsWithDurations(ctx, log, faultUpdateDelay, faultClearTimeout, onFaultChange)
}

func newDiagnosticsWithDurations(ctx context.Context, log *Logger, updateDelay, clearDelay time.Duration, onFaultChange func(Fault, FaultConfig)) *Diagnostics {
	d := &Diagnostics{
		log:           log,
		onFaultChange: onFaultChange,
		updateDelay:   updateDelay,
		clearDelay:    clearDelay,
	}
	d.updateTimer = time.NewTimer(updateDelay)
	d.updateTimer.Stop()
	d.clearTimer = time.NewTimer(clearDelay)
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
				// A prior timer event may already have been selected when Update
				// installs a newer transition. Never let that stale event commit
				// the new fault before its own full stability interval.
				if remaining := d.updateDelay - time.Since(d.pendingSince); remaining > 0 {
					d.updateTimer.Reset(remaining)
					d.mu.Unlock()
					continue
				}
				d.currentFault = f
				_, cfg := MapFault(uint32(f))
				d.mu.Unlock()
				d.log.Warn("Fault committed: code=%d (%s)", f, cfg.Description)
				d.onFaultChange(f, cfg)
			} else {
				d.mu.Unlock()
			}

		case <-d.clearTimer.C:
			d.mu.Lock()
			if d.pendingFault == FaultNone && d.currentFault != FaultNone {
				if remaining := d.clearDelay - time.Since(d.pendingSince); remaining > 0 {
					d.clearTimer.Reset(remaining)
					d.mu.Unlock()
					continue
				}
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

	// Debounce is transition-driven: another report of the same pending state
	// must not restart its timer and starve a commit or clear indefinitely.
	if fault == d.pendingFault {
		return
	}
	d.pendingFault = fault
	d.pendingSince = time.Now()

	stopTimer(d.updateTimer)
	stopTimer(d.clearTimer)

	// Returning to the committed state only cancels the opposite transition.
	if fault == d.currentFault {
		return
	}
	if fault == FaultNone {
		d.clearTimer.Reset(d.clearDelay)
	} else {
		d.updateTimer.Reset(d.updateDelay)
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
