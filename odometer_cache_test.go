package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOdometerMissing(t *testing.T) {
	got, valid, err := loadOdometer(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("load missing cache: %v", err)
	}
	if valid || got != 0 {
		t.Fatalf("missing cache = (%d, %v), want (0, false)", got, valid)
	}
}

func TestLoadOdometerRejectsInvalidFiles(t *testing.T) {
	cases := map[string]string{
		"empty":           "",
		"malformed":       "{",
		"missing version": `{"odometer":1}`,
		"missing value":   `{"version":1}`,
		"wrong version":   `{"version":2,"odometer":1}`,
		"unknown field":   `{"version":1,"odometer":1,"extra":true}`,
		"negative":        `{"version":1,"odometer":-1}`,
		"overflow":        `{"version":1,"odometer":4294967296}`,
		"trailing value":  `{"version":1,"odometer":1} {}`,
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "odometer.json")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, valid, err := loadOdometer(path); err == nil || valid {
				t.Fatalf("load invalid cache = (valid %v, err %v), want error", valid, err)
			}
		})
	}
}

func TestSaveLoadOdometerAtomicAndZero(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "odometer.json")

	for _, want := range []uint32{0, 123456, 42} {
		if err := saveOdometer(path, want); err != nil {
			t.Fatalf("save %d: %v", want, err)
		}
		got, valid, err := loadOdometer(path)
		if err != nil {
			t.Fatalf("load %d: %v", want, err)
		}
		if !valid || got != want {
			t.Fatalf("load = (%d, %v), want (%d, true)", got, valid, want)
		}
		matches, err := filepath.Glob(filepath.Join(dir, ".odometer-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("temporary files remain after atomic save: %v", matches)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0640 {
		t.Errorf("cache permissions = %04o, want 0640", got)
	}
}
