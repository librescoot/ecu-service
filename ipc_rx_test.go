package main

import "testing"

func TestKersSettingEnabled(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		// The schema's enum, and what lsc, the BLE settings write and the
		// docs all produce.
		{"enabled", true},
		{"disabled", false},
		// Kept working for scooters whose persisted settings still carry it.
		{"false", false},
		{"true", true},
		// Unset, and anything unrecognised, leaves regen available.
		{"", true},
		{"off", true},
	}

	for _, tt := range tests {
		if got := kersSettingEnabled(tt.val); got != tt.want {
			t.Errorf("kersSettingEnabled(%q) = %v, want %v", tt.val, got, tt.want)
		}
	}
}
