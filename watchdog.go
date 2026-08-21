package main

import (
	"context"
	"time"
)

const (
	commLostTick = 500 * time.Millisecond
	// commLostPollAfter: prod the ECU with 0x4EF only once we haven't heard from
	// it this long. While driving the ECU pushes Status frames at ~5 Hz on its
	// own; idle in ready-to-drive drops below the raise threshold, so poll there.
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
// down) and on a power-on grace window. It is deliberately NOT gated on wheel
// speed: at standstill the ECU sends nothing, so a powered-but-silent ECU must
// still raise/clear E20 or the feed would never recover.
// vehicleHashReader is the minimal read surface CommLostWatcher needs from the
// vehicle Redis hash. *ipc.Client satisfies it; tests inject an in-memory fake.
type vehicleHashReader interface {
	HGetAll(key string) (map[string]string, error)
}

type CommLostWatcher struct {
	ipc      vehicleHashReader
	ecu      *ECU
	log      *Logger
	onChange func(raise bool)

	published      bool
	prevEcuPowered bool
	powerOnEdge    time.Time
}

func newCommLostWatcher(client vehicleHashReader, ecu *ECU, log *Logger, onChange func(bool)) *CommLostWatcher {
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
	// Comm-loss is only flagged while moving. The ECU pushes no frames at standstill
	// (normal), so a parked-but-powered ECU being silent is expected and must NOT raise
	// E20 (a false alarm). While driving, an expected-alive, powered ECU that has been
	// silent for >= commLostRaiseAfter is truly comm-lost and must flag so the live
	// feed recovers. The ecuPowered && !inGrace gates prevent spurious E20 on genuine
	// shutdown or during the power-on grace window.
	moving := w.ecu.Speed() != 0
	shouldRaise := stale && ecuPowered && !inGrace && moving

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
