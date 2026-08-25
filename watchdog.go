package main

import (
	"context"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

const (
	commLostTick = 500 * time.Millisecond
	// commLostPollAfter: prod the ECU with 0x4EF only once we haven't heard from
	// it this long. Once up, a powered ECU broadcasts Status frames unprompted
	// on its own regardless of speed (measured on-vehicle: continuous Status1 /
	// Status3 traffic with the scooter stationary, no further polling needed
	// once the first frame arrived); this poll only covers the gap between
	// power-on and that first frame.
	commLostPollAfter = 700 * time.Millisecond
	// commLostRaiseAfter is deliberately longer than pollAfter + worst-case ECU
	// reply latency (the 0x7E0-0x7E8 burst is spread over ~1s), so a fresh poll's
	// own response has time to land before we'd flag comm lost.
	commLostRaiseAfter = 3 * time.Second
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
	// Only the no-frame-yet case waits this long. Staleness is measured from the
	// more recent of {last frame, power-on edge}, so the first frame to arrive
	// ends the window on its own and a genuinely dead ECU is still reported,
	// just at power-on + this rather than power-on + commLostRaiseAfter.
	commLostPowerOnGrace = 8 * time.Second
)

// CommLostWatcher raises fault E20 when the ECU should be alive and powered but
// hasn't sent a CAN frame within commLostRaiseAfter. It is gated on vehicle
// engine-power && main-power (so it stays quiet during standby or when 48V is
// down), on a power-on grace window wide enough for the ECU to boot, and on the
// last known speed being non-zero.
//
// That speed gate is deliberate and was reinstated after field evidence. A
// powered controller that goes quiet while the vehicle is stopped turns out to
// be common across the fleet rather than exceptional: controllers vary in how
// much they report at rest, and raising E20 for it dashes the cluster at a
// standstill for a condition the rider can do nothing about and which clears
// itself the moment they set off. The at-rest case is logged instead, at info,
// so it can be counted in the field without being shown to riders.
//
// The cost is accepted knowingly: a bus that dies while the vehicle is parked
// and powered will not raise E20 until the vehicle moves. Frame staleness while
// moving is unambiguous and still raises immediately.
//
// Measured stationary on two controllers, both healthy. The rate differs by
// 20x, the gap this check depends on does not:
//
//	replacement logic board   ~200 frames/s, occasional multi-second dropouts
//	stock controller          10 frames/s, largest gap 0.26s over 83s
type CommLostWatcher struct {
	ipc      *ipc.Client
	ecu      *ECU
	log      *Logger
	onChange func(raise bool)

	published      bool
	prevEcuPowered bool
	powerOnEdge    time.Time
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
	// ECU is expected to talk iff vehicle-service commanded engine-power ON and
	// the battery supplies the 48V rail (main-power ON). Both must hold.
	ecuPowered := fields["engine-power"] == "on" && fields["main-power"] == "on"
	// This is the only place that reads the power fields, so it also tells the
	// ECU whether it may transmit at all. Without that, the CAN reconnect loop
	// and the KERS setters would keep talking to an unpowered ECU, and the
	// unacknowledged frames eventually latch the controller bus-off.
	w.ecu.SetPowered(ecuPowered)

	shouldRaise := w.evaluate(ecuPowered)

	switch {
	case shouldRaise && !w.published:
		w.published = true
		w.log.Warn("ECU communication lost (>%v) in state=%s, publishing E20", commLostRaiseAfter, state)
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
// raised, polling the ECU (0x4EF) if we're overdue and tracking the power-on
// grace edge along the way. Split out from check() so the decision itself can
// be tested without a live IPC connection.
func (w *CommLostWatcher) evaluate(ecuPowered bool) bool {
	now := time.Now()
	if ecuPowered && !w.prevEcuPowered {
		w.powerOnEdge = now
	}
	w.prevEcuPowered = ecuPowered
	inGrace := !w.powerOnEdge.IsZero() && now.Sub(w.powerOnEdge) < commLostPowerOnGrace

	if ecuPowered && w.ecu.TimeSinceLastFrame() > commLostPollAfter {
		w.ecu.RequestStatus()
	}

	// Measure staleness from the more recent of {last frame, power-on edge}, so a
	// frame timestamp carried over from a previous power cycle doesn't trip the
	// check the instant the grace window expires.
	frameAge := w.ecu.TimeSinceLastFrame()
	if !w.powerOnEdge.IsZero() {
		if since := now.Sub(w.powerOnEdge); since < frameAge {
			frameAge = since
		}
	}
	stale := frameAge > commLostRaiseAfter
	silent := stale && ecuPowered && !inGrace

	// Speed is the ECU's own last reported value, so it is whatever was true when
	// it stopped talking: non-zero means it went quiet mid-ride.
	moving := w.ecu.Speed() != 0
	w.noteSilentAtRest(silent && !moving, frameAge)

	return silent && moving
}

// noteSilentAtRest logs the suppressed case on its edges. This is the only
// record that a powered controller went quiet while stopped, so it is info
// rather than debug: the whole point is to be able to count it in the field
// from ordinary log packages.
func (w *CommLostWatcher) noteSilentAtRest(silent bool, frameAge time.Duration) {
	switch {
	case silent && !w.silentAtRest:
		w.silentAtRest = true
		w.log.Info("ECU silent at rest: powered, no frame for %.1fs, speed 0. E20 suppressed while stopped", frameAge.Seconds())
	case !silent && w.silentAtRest:
		w.silentAtRest = false
		w.log.Info("ECU no longer silent at rest")
	}
}
