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
	// commLostPowerOnGrace suppresses E20 right after the ECU is powered, giving
	// it time to boot and send its first frame.
	commLostPowerOnGrace = 2 * time.Second
)

// CommLostWatcher raises fault E20 when the ECU should be alive and powered but
// hasn't sent a CAN frame within commLostRaiseAfter. It is gated on vehicle
// engine-power && main-power (so it stays quiet during standby or when 48V is
// down) and on a power-on grace window. It does not gate on speed: once up, a
// powered ECU broadcasts Status frames unprompted whether or not it is moving
// (measured on-vehicle), so frame staleness on its own is a sound signal and a
// parked-but-powered ECU never raises the fault. The 0x4EF poll only covers the
// gap before the first broadcast. Gating on speed hid a dead bus at standstill.
type CommLostWatcher struct {
	ipc      *ipc.Client
	ecu      *ECU
	log      *Logger
	onChange func(raise bool)

	published      bool
	prevEcuPowered bool
	powerOnEdge    time.Time
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
		w.log.Info("E20 cleared")
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
	return stale && ecuPowered && !inGrace
}
