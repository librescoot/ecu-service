package main

// Regen-availability model. All constants below are empirically derived and
// expressed in the ECU's internal FOC current-command counts unless noted.
const (
	// regenCeilingCounts is the absolute regen-current command ceiling.
	regenCeilingCounts = 5719
	// regenCountToMA converts a current-command count to milliamps.
	regenCountToMA = 10
	// regenFloorCounts is the current-command floor; at or below it the
	// configured current limit yields zero braking authority.
	regenFloorCounts = 100
	// regenVLoopCountsPerV is the voltage-loop backoff slope: command counts
	// cancelled per volt that pack voltage exceeds the accepted voltage cap.
	regenVLoopCountsPerV = 1875
	// regenVLoopBandMV is the over-voltage band, above the accepted cap, over
	// which the voltage loop ramps regen from full authority down to zero.
	regenVLoopBandMV = 3050
)

// RegenState is the derived view of whether regen is available, why not, and
// how much braking current the ECU is expected to allow right now.
type RegenState struct {
	Available  bool
	Reason     string // "none" when available; otherwise cold/hot/off/full
	ExpectedMA int    // expected regen current envelope, in mA
}

// computeRegen derives the regen envelope from the accepted EBS caps, the live
// pack voltage, the KERS arm state and its reason. vMaxMV/iMaxMA are the
// accepted caps echoed by the ECU (0 until the first EBS Status frame).
func computeRegen(enabled bool, armReason KERSReason, vPackMV, vMaxMV, iMaxMA int) RegenState {
	// Temperature gating disarms KERS outright.
	switch armReason {
	case KERSReasonCold:
		return RegenState{Available: false, Reason: "cold"}
	case KERSReasonHot:
		return RegenState{Available: false, Reason: "hot"}
	}
	// Not armed (user-disabled or not yet ready to drive).
	if !enabled {
		return RegenState{Available: false, Reason: "off"}
	}
	// No accepted caps yet (no EBS Status frame seen) — assume available
	// rather than falsely flag a limit we can't assess.
	if vMaxMV <= 0 || iMaxMA <= 0 {
		return RegenState{Available: true, Reason: "none"}
	}

	authority := iMaxMA/regenCountToMA - regenFloorCounts
	if authority < 0 {
		authority = 0
	}
	if authority > regenCeilingCounts {
		authority = regenCeilingCounts
	}

	envelope := authority
	switch {
	case vPackMV <= vMaxMV:
		// Current-limited region: full configured authority.
	case vPackMV < vMaxMV+regenVLoopBandMV:
		// Voltage loop backs current off as the pack rises past the cap.
		envelope = authority - regenVLoopCountsPerV*(vPackMV-vMaxMV)/1000
		if envelope < 0 {
			envelope = 0
		}
	default:
		// At or beyond the cap plus the band: regen fully cancelled.
		envelope = 0
	}

	if envelope == 0 {
		// Voltage-limited to zero — the pack is at its cap.
		return RegenState{Available: false, Reason: "full"}
	}
	return RegenState{Available: true, Reason: "none", ExpectedMA: envelope * regenCountToMA}
}
