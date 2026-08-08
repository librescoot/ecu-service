package main

import "testing"

func TestComputeRegen(t *testing.T) {
	const (
		vMax = 56000 // accepted regen voltage cap (mV)
		iMax = 10000 // accepted regen current cap (mA) -> 900 counts authority
		rpm  = 200   // wheel RPM, above the engage deadband
	)

	tests := []struct {
		name          string
		enabled       bool
		armReason     KERSReason
		wheelRPM      int
		vPackMV       int
		vMaxMV        int
		iMaxMA        int
		wantAvailable bool
		wantReason    string
	}{
		{"cold disarms", true, KERSReasonCold, rpm, 50000, vMax, iMax, false, "cold"},
		{"hot disarms", true, KERSReasonHot, rpm, 50000, vMax, iMax, false, "hot"},
		{"disabled", false, KERSReasonNone, rpm, 50000, vMax, iMax, false, "off"},
		{"standstill", true, KERSReasonNone, 0, 50000, vMax, iMax, false, "standstill"},
		{"just below engage speed", true, KERSReasonNone, regenEngageMinWheelRPM - 1, 50000, vMax, iMax, false, "standstill"},
		{"at engage speed", true, KERSReasonNone, regenEngageMinWheelRPM, 50000, vMax, iMax, true, "none"},
		{"no caps -> assume available", true, KERSReasonNone, rpm, 50000, 0, 0, true, "none"},
		{"current-limited below cap", true, KERSReasonNone, rpm, 50000, vMax, iMax, true, "none"},
		{"voltage-limited to zero at cap+band", true, KERSReasonNone, rpm, vMax + regenVLoopBandMV, vMax, iMax, false, "full"},
		// Authority 3900 counts (iMax 40000 mA); 1 V over-cap costs 1875 counts,
		// leaving a positive envelope -> still available.
		{"voltage backing off inside band", true, KERSReasonNone, rpm, vMax + 1000, vMax, 40000, true, "none"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeRegen(tc.enabled, tc.armReason, tc.wheelRPM, tc.vPackMV, tc.vMaxMV, tc.iMaxMA)
			if got.Available != tc.wantAvailable {
				t.Errorf("Available = %v, want %v", got.Available, tc.wantAvailable)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// The standstill gate must take precedence over the voltage loop: even with the
// pack well below the cap (full voltage headroom), a stopped vehicle reports
// unavailable, matching the ECU dropping out of regen near standstill.
func TestComputeRegenStandstillBeatsVoltageHeadroom(t *testing.T) {
	got := computeRegen(true, KERSReasonNone, 0, 40000, 56000, 10000)
	if got.Available || got.Reason != "standstill" {
		t.Errorf("got {Available:%v Reason:%q}, want {false standstill}", got.Available, got.Reason)
	}
}
