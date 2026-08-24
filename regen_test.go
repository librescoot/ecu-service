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
		policyAllows  bool
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
			got := computeRegen(tc.policyAllows, tc.armReason, tc.wheelRPM, tc.vPackMV, tc.vMaxMV, tc.iMaxMA)
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

func TestAppRegenStateUsesCommandedPolicy(t *testing.T) {
	const (
		wheelRPM = 200
		vPackMV  = 50000
		vMaxMV   = 56000
		iMaxMA   = 10000
	)

	ecu := &ECU{kersActive: true}
	app := &App{ecu: ecu, lastKersReason: KERSReasonNone}
	status := Status{
		KersActive:           false, // stale Status4 snapshot
		RPM:                  wheelRPM,
		Voltage:              vPackMV,
		AcceptedRegenVoltage: vMaxMV,
		AcceptedRegenCurrent: iMaxMA,
	}

	got := app.regenState(status)
	if !got.Available || got.Reason != "none" {
		t.Fatalf("commanded-on policy got {Available:%v Reason:%q}, want {true none}", got.Available, got.Reason)
	}

	ecu.kersActive = false
	status.KersActive = true // stale snapshot in the other direction
	got = app.regenState(status)
	if got.Available || got.Reason != "off" {
		t.Fatalf("commanded-off policy got {Available:%v Reason:%q}, want {false off}", got.Available, got.Reason)
	}
}

func TestApplyObservedRegen(t *testing.T) {
	tests := []struct {
		name          string
		predicted     RegenState
		currentMA     int
		wantAvailable bool
		wantReason    string
	}{
		{
			name:          "negative current overrides unavailable prediction",
			predicted:     RegenState{Available: false, Reason: "off"},
			currentMA:     -regenObservedCurrentThresholdMA,
			wantAvailable: true,
			wantReason:    "none",
		},
		{
			name:          "sensor noise does not override prediction",
			predicted:     RegenState{Available: false, Reason: "full"},
			currentMA:     -regenObservedCurrentThresholdMA + 1,
			wantAvailable: false,
			wantReason:    "full",
		},
		{
			name:          "positive prediction is unchanged",
			predicted:     RegenState{Available: true, Reason: "none", ExpectedMA: 9000},
			currentMA:     0,
			wantAvailable: true,
			wantReason:    "none",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyObservedRegen(tc.predicted, tc.currentMA)
			if got.Available != tc.wantAvailable {
				t.Errorf("Available = %v, want %v", got.Available, tc.wantAvailable)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.ExpectedMA != tc.predicted.ExpectedMA {
				t.Errorf("ExpectedMA = %d, want prediction preserved at %d", got.ExpectedMA, tc.predicted.ExpectedMA)
			}
		})
	}
}
