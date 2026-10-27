package main

import (
	"fmt"
	"strconv"
)

// loadEnergyTotals runs before CAN reception starts; failed reads or invalid
// values must not be replaced with a new from-zero total.
func (a *App) loadEnergyTotals() error {
	fields := []string{"energy:consumed", "energy:recovered"}
	values, err := a.ipcTx.raw.HMGet(a.ipcTx.ctx, ecuHashKey, fields...).Result()
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
	a.ecu.energyConsumed, a.ecu.energyRecovered = totals[0], totals[1]
	return nil
}
