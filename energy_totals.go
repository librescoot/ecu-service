package main

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// loadEnergyTotals adds the retained baseline to any energy integrated while waiting.
func (a *App) loadEnergyTotals(ctx context.Context) error {
	fields := []string{"energy:consumed", "energy:recovered"}
	values, err := a.ipcTx.raw.HMGet(ctx, ecuHashKey, fields...).Result()
	if err != nil {
		return fmt.Errorf("read energy totals: %w", err)
	}
	var totals [2]uint64
	for i, value := range values {
		if value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("invalid %s value", fields[i])
		}
		totals[i], err = strconv.ParseUint(text, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid %s value %q: %w", fields[i], text, err)
		}
	}
	a.ecu.restoreEnergyTotals(totals[0], totals[1])
	return nil
}

const energyRestoreInterval = 5 * time.Second
const energyRestoreTimeout = 2 * time.Second

func (a *App) restoreEnergyTotals(ctx context.Context) {
	for {
		attempt, cancel := context.WithTimeout(ctx, energyRestoreTimeout)
		err := a.loadEnergyTotals(attempt)
		cancel()
		if err == nil {
			a.log.Info("Energy totals restored from Redis")
			return
		}
		if ctx.Err() != nil {
			return
		}
		a.log.Warn("Energy totals unavailable; retrying in %s: %v", energyRestoreInterval, err)
		timer := time.NewTimer(energyRestoreInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
