package main

import (
	"context"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

const (
	commLostTick          = 500 * time.Millisecond
	commLostRaiseAfter    = 3 * time.Second
	commLostProbeAfter    = time.Second
	commLostProbeWait     = 1500 * time.Millisecond
	commLostProbeInterval = 3 * time.Second
	commLostRecoveryTime  = time.Second
	// ecuColdStartWorst is the longest measured delay between engine_power going
	// on and the controller's first CAN frame. It is not one number: it varies by
	// controller by a factor of four, measured with `lsc engine on`, stationary,
	// engine brake engaged.
	//
	//	replacement logic board   5.0s to 5.5s   (four cycles)
	//	stock controller          1.1s to 1.4s   (three cycles, v1.2.1)
	//
	// Both are consistent across repeated cycles, so these are boot times and not
	// warm-up effects. The grace window has to cover the slowest controller we
	// know about, so this is the replacement board's figure. Measure again before
	// assuming a new controller fits inside it.
	ecuColdStartWorst = 5500 * time.Millisecond
	// commLostPowerOnGrace suppresses E20 right after the ECU is powered, giving
	// it time to boot and send its first frame. It has to clear
	// ecuColdStartWorst with margin: at 2s, every single power-on on the slower
	// controller raised E20 for one to two and a half seconds, which the cluster
	// renders as dashes for speed. That was invisible while the watchdog still
	// gated on non-zero speed, because speed is 0 for the whole of the boot.
	//
	// A stock controller boots inside the old 2s on its own, so this window is
	// generous there. That costs nothing: the only case that waits it out is an
	// ECU that has sent no frame at all.
	//
	// All liveness checks wait through this grace; status requests then receive
	// their own bounded response windows.
	commLostPowerOnGrace = 8 * time.Second
)

// CommLostWatcher distinguishes silence from failure with two bounded status
// requests, independent of cached speed. Monitoring requires both power rails
// and completed boot grace. Transport failures are attributed separately.
// Recovery requires sustained traffic or a separate probe response.
type CommLostWatcher struct {
	ipc      *ipc.Client
	ecu      *ECU
	log      *Logger
	onChange func(raise bool)

	published         bool
	prevEcuPowered    bool
	powerOnEdge       time.Time
	probeAt           time.Time
	probeFrame        time.Time
	probeAttempts     int
	failed            bool
	recoverySince     time.Time
	recoveryProbeAt   time.Time
	recoveryLastFrame time.Time
	transportCause    func(time.Time) string
	failureCause      string
	// silentAtRest edges the at-rest log line. The check runs at 2Hz, and an ECU
	// that has gone quiet stays quiet, so logging the condition rather than the
	// transition would fill the journal for as long as it lasts.
	silentAtRest bool
}

func newCommLostWatcher(client *ipc.Client, ecu *ECU, log *Logger, onChange func(bool)) *CommLostWatcher {
	return &CommLostWatcher{ipc: client, ecu: ecu, log: log, onChange: onChange}
}

func (w *CommLostWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(commLostTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.check()
		}
	}
}

func (w *CommLostWatcher) check() {
	fields, err := w.ipc.HGetAll("vehicle")
	if err != nil {
		w.log.Debug("comm-lost watchdog: read vehicle hash: %v", err)
		return
	}
	state := fields["state"]
	// Refresh the state here as well as in the watcher: this tick owns the
	// power-up assertion, so it must not send parked KERS while waiting for a
	// Redis subscription update.
	w.ecu.SetParked(state == "parked")
	// ECU is expected to talk iff vehicle-service commanded engine-power ON and
	// the battery supplies the 48V rail (main-power ON). Both must hold.
	ecuPowered := fields["engine-power"] == "on" && fields["main-power"] == "on"
	// Refresh the transmit gate alongside the power-field subscription.
	// Unacknowledged transmissions to an unpowered ECU can cause bus-off.
	w.ecu.SetPowered(ecuPowered)
	// Re-assert the commanded KERS and boost state until the controller answers.
	// The Control frame is answered with 0x7E4 and no-ops once acknowledged.
	w.ecu.ApplyCommandedState()

	shouldRaise := w.evaluate(ecuPowered)

	switch {
	case shouldRaise && !w.published:
		w.published = true
		w.log.Warn("ECU communication lost in state=%s: %s, publishing E20", state, w.failureCause)
		w.onChange(true)
	case !shouldRaise && w.published:
		w.published = false
		// Which of the two it is matters when reading a log after the fact. A
		// clear on the power-off edge is not the ECU recovering, it is the fault
		// becoming unreportable, and reading those as recovery understates how
		// long an episode really lasted.
		if !ecuPowered {
			w.log.Info("E20 cleared (ECU power removed, not recovery)")
		} else {
			w.log.Info("E20 cleared (frame received after %.1fs)", w.ecu.TimeSinceLastFrame().Seconds())
		}
		w.onChange(false)
	}
}

// evaluate applies the power/staleness state to decide whether E20 should be
// raised and tracks the power-on grace edge along the way. Split out from
// check() so the decision itself can be tested without a live IPC connection.
func (w *CommLostWatcher) evaluate(ecuPowered bool) bool {
	return w.evaluateAt(ecuPowered, time.Now())
}

func (w *CommLostWatcher) evaluateAt(ecuPowered bool, now time.Time) bool {
	if ecuPowered != w.prevEcuPowered {
		w.resetEpisode()
	}
	if ecuPowered && !w.prevEcuPowered {
		w.powerOnEdge = now
	}
	w.prevEcuPowered = ecuPowered
	inGrace := !w.powerOnEdge.IsZero() && now.Sub(w.powerOnEdge) < commLostPowerOnGrace

	// Measure staleness from the more recent of {last frame, power-on edge}, so a
	// frame timestamp carried over from a previous power cycle doesn't trip the
	// check the instant the grace window expires.
	lastFrame := w.ecu.LastFrameTime()
	frameAge := now.Sub(lastFrame)
	if !w.failed && lastFrame.After(w.probeFrame) {
		w.probeAttempts = 0
	}
	if !w.powerOnEdge.IsZero() {
		if since := now.Sub(w.powerOnEdge); since < frameAge {
			frameAge = since
		}
	}
	stale := frameAge > commLostRaiseAfter
	silent := stale && ecuPowered && !inGrace

	// Cached speed classifies the diagnostic log only, not liveness.
	moving := w.ecu.Speed() != 0
	w.noteSilentAtRest(silent && !moving, frameAge)

	if !ecuPowered || inGrace {
		return false
	}
	cause := ""
	if w.transportCause != nil {
		cause = w.transportCause(now)
	}
	if cause != "" {
		w.failed = true
		w.recoverySince = time.Time{}
		w.setFailureCause(cause)
		return true
	}
	if w.failed {
		// Confirm recovery with sustained traffic or a reply to a separate probe.
		if !w.recoveryProbeAt.IsZero() && lastFrame.After(w.recoveryProbeAt) {
			w.resetEpisode()
			return false
		}
		if frameAge <= commLostRecoveryTime {
			if w.recoverySince.IsZero() || lastFrame.Sub(w.recoveryLastFrame) > commLostRecoveryTime {
				w.recoverySince = lastFrame
			} else if lastFrame.Sub(w.recoverySince) >= commLostRecoveryTime {
				w.resetEpisode()
				return false
			}
		}
		w.recoveryLastFrame = lastFrame
		if !w.recoverySince.IsZero() {
			if now.Sub(w.recoverySince) > commLostProbeInterval+commLostProbeWait {
				w.recoverySince, w.recoveryProbeAt = time.Time{}, time.Time{}
			} else if w.recoveryProbeAt.IsZero() && now.Sub(w.probeAt) >= commLostProbeInterval {
				w.probeAt, w.recoveryProbeAt = now, now
				w.log.Info("ECU traffic resumed, requesting recovery confirmation")
				w.ecu.RequestStatus()
			}
		}
		return true
	}
	// At most two attempts per silence, including failed sends. The cooldown
	// survives replies and power edges; sending never establishes liveness.
	if frameAge >= commLostProbeAfter && w.probeAttempts < 2 &&
		(w.probeAt.IsZero() || now.Sub(w.probeAt) >= commLostProbeInterval) {
		w.probeAt = now
		w.probeFrame = lastFrame
		w.probeAttempts++
		w.log.Info("ECU silent for %.1fs, requesting status (attempt %d/2)", frameAge.Seconds(), w.probeAttempts)
		w.ecu.RequestStatus()
	}
	if silent && w.probeAttempts == 2 && now.Sub(w.probeAt) >= commLostProbeWait {
		w.failed = true
		w.setFailureCause("no ECU response after two status requests")
	}
	return w.failed
}

func (w *CommLostWatcher) resetEpisode() {
	w.probeAttempts = 0
	w.failed = false
	w.recoverySince, w.recoveryProbeAt = time.Time{}, time.Time{}
	w.failureCause = ""
}

func (w *CommLostWatcher) setFailureCause(cause string) {
	if cause != w.failureCause {
		w.log.Warn("ECU communication unavailable: %s", cause)
		w.failureCause = cause
	}
}

// noteSilentAtRest records stationary silence independently of probe results.
func (w *CommLostWatcher) noteSilentAtRest(silent bool, frameAge time.Duration) {
	switch {
	case silent && !w.silentAtRest:
		w.silentAtRest = true
		w.log.Info("ECU silent at rest: powered, no frame for %.1fs, speed 0; checking responsiveness", frameAge.Seconds())
	case !silent && w.silentAtRest:
		w.silentAtRest = false
		w.log.Info("ECU no longer silent at rest")
	}
}
