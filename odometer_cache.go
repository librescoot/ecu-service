package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	defaultOdometerFile = "/data/ecu-service/odometer.json"
	odometerFileVersion = 1
)

type odometerFile struct {
	Version  *int    `json:"version"`
	Odometer *uint32 `json:"odometer"`
}

type odometerFileOutput struct {
	Version  int    `json:"version"`
	Odometer uint32 `json:"odometer"`
}

func loadOdometer(path string) (uint32, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var cached odometerFile
	if err := dec.Decode(&cached); err != nil {
		return 0, false, fmt.Errorf("decode: %w", err)
	}
	if cached.Version == nil || cached.Odometer == nil {
		return 0, false, errors.New("missing required field")
	}
	if *cached.Version != odometerFileVersion {
		return 0, false, fmt.Errorf("unsupported version %d", *cached.Version)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return 0, false, errors.New("multiple JSON values")
		}
		return 0, false, fmt.Errorf("trailing data: %w", err)
	}
	return *cached.Odometer, true, nil
}

func saveOdometer(path string, odometer uint32) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	f, err := os.CreateTemp(dir, ".odometer-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	failed := true
	defer func() {
		if failed {
			f.Close()
		}
	}()
	if err := f.Chmod(0640); err != nil {
		return fmt.Errorf("set permissions: %w", err)
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(odometerFileOutput{Version: odometerFileVersion, Odometer: odometer}); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	failed = false
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename temporary file: %w", err)
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("sync directory: %w", err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("close directory: %w", err)
	}
	return nil
}
