package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestAcceptStatus3OdometerPersistsChangedValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "odometer.json")
	ecu := newTestECU()
	a := &App{
		opts: Options{OdometerFile: path},
		log:  newLogger(LogLevelNone),
		ecu:  ecu,
	}

	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, 1000)
	ecu.HandleFrame(makeFrame(frameStatus3, data))
	if !a.acceptStatus3Odometer() {
		t.Fatal("first live Status3 did not request notification")
	}
	want := ecu.Odometer()
	got, valid, err := loadOdometer(path)
	if err != nil {
		t.Fatal(err)
	}
	if !valid || got != want {
		t.Fatalf("persisted odometer = (%d, %v), want (%d, true)", got, valid, want)
	}
	// onFrame records this only after both the Redis hash update and
	// notification succeed.
	a.lastLiveOdometer = want
	a.lastLiveOdometerOK = true

	if a.acceptStatus3Odometer() {
		t.Error("unchanged repeated Status3 requested another notification")
	}

	binary.BigEndian.PutUint32(data, 2000)
	ecu.HandleFrame(makeFrame(frameStatus3, data))
	if !a.acceptStatus3Odometer() {
		t.Fatal("changed live Status3 did not request notification")
	}
	got, valid, err = loadOdometer(path)
	if err != nil {
		t.Fatal(err)
	}
	if !valid || got != ecu.Odometer() {
		t.Fatalf("updated cache = (%d, %v), want (%d, true)", got, valid, ecu.Odometer())
	}
}

func TestAcceptStatus3OdometerRetriesUnacknowledgedPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "odometer.json")
	ecu := newTestECU()
	a := &App{
		opts: Options{OdometerFile: path},
		log:  newLogger(LogLevelNone),
		ecu:  ecu,
	}

	ecu.HandleFrame(makeFrame(frameStatus3, make([]byte, 4)))
	if !a.acceptStatus3Odometer() {
		t.Fatal("first valid zero did not request publication")
	}
	// onFrame only acknowledges a publication after both the Redis hash write
	// and synchronous notification succeed. Without that acknowledgement, an
	// identical Status3 must request the whole operation again.
	if !a.acceptStatus3Odometer() {
		t.Fatal("identical Status3 did not retry unacknowledged publication")
	}
}

func TestAcceptStatus3OdometerRetriesFailedPersistence(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	ecu := newTestECU()
	a := &App{
		opts: Options{OdometerFile: filepath.Join(blocked, "odometer.json")},
		log:  newLogger(LogLevelNone),
		ecu:  ecu,
	}

	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, 1000)
	ecu.HandleFrame(makeFrame(frameStatus3, data))
	a.acceptStatus3Odometer()
	if a.persistedOdometerValid {
		t.Fatal("failed save was recorded as durable")
	}

	a.opts.OdometerFile = filepath.Join(dir, "state", "odometer.json")
	a.acceptStatus3Odometer()
	if !a.persistedOdometerValid || a.persistedOdometer != ecu.Odometer() {
		t.Fatal("unchanged repeated Status3 did not retry persistence")
	}
}

func TestCachedOdometerDoesNotEstablishLiveStatus3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "odometer.json")
	if err := saveOdometer(path, 123); err != nil {
		t.Fatal(err)
	}
	a := &App{
		opts: Options{OdometerFile: path},
		log:  newLogger(LogLevelNone),
		ecu:  newTestECU(),
	}
	odometer, valid, err := loadOdometer(path)
	if err != nil || !valid {
		t.Fatalf("load cache = (%d, %v, %v)", odometer, valid, err)
	}
	a.odometer, a.odometerValid = odometer, valid

	if a.acceptStatus3Odometer() {
		t.Error("cache without live Status3 requested notification")
	}
	if a.lastLiveOdometerOK {
		t.Error("cache was treated as proof of a live Status3")
	}
}
