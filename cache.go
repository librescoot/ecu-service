package main

import (
	"encoding/json"
	"fmt"
	"os"
)

const (
	cacheDir  = "/data/cache"
	cacheFile = "/data/cache/engine-ecu.json"
)

type ecuCache struct {
	Odometer uint32 `json:"odometer"`
}

func loadOdometerCache(log *LeveledLogger) uint32 {
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("Failed to read odometer cache: %v", err)
		}
		return 0
	}

	var cache ecuCache
	if err := json.Unmarshal(data, &cache); err != nil {
		log.Warn("Failed to parse odometer cache: %v", err)
		return 0
	}

	return cache.Odometer
}

func saveOdometerCache(log *LeveledLogger, odometer uint32) error {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	data, err := json.Marshal(ecuCache{Odometer: odometer})
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}

	tmpFile := cacheFile + ".tmp"
	f, err := os.OpenFile(tmpFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmpFile)
		return fmt.Errorf("write cache: %w", err)
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpFile)
		return fmt.Errorf("sync cache: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpFile, cacheFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("rename cache file: %w", err)
	}

	return nil
}
