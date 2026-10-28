package main

import "time"

const driveStartProbeInterval = time.Second

// Other status frames can keep the ECU alive without reporting throttle or
// wheel movement. A pending start therefore requests a full status snapshot
// independently of the communication-loss watchdog's silence detection.
func (w *CommLostWatcher) requestDriveStartStatus(state, status string, powered bool, now time.Time) {
	if state != "parked" || !powered || w.failed || w.powerOnEdge.IsZero() || now.Sub(w.powerOnEdge) < commLostPowerOnGrace {
		return
	}
	switch status {
	case "waiting-ecu", "release-throttle", "stop-vehicle":
	default:
		return
	}
	if (!w.driveStartProbeAt.IsZero() && now.Sub(w.driveStartProbeAt) < driveStartProbeInterval) ||
		(!w.probeAt.IsZero() && now.Sub(w.probeAt) < driveStartProbeInterval) {
		return
	}
	w.driveStartProbeAt = now
	w.ecu.RequestStatus()
}
