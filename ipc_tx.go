package main

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/go-redis/redis/v8"
)

type IPCTx struct {
	log   *log.Logger
	redis *redis.Client
	mu    sync.Mutex
	ctx   context.Context
}

func NewIPCTx(logger *log.Logger, redis *redis.Client) *IPCTx {
	return &IPCTx{
		log:   logger,
		redis: redis,
		ctx:   context.Background(),
	}
}

func (tx *IPCTx) Destroy() {}

func (tx *IPCTx) SendStatus1(data RedisStatus1) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	pipe := tx.redis.Pipeline()

	pipe.HSet(tx.ctx, "engine-ecu", map[string]interface{}{
		"motor:voltage": data.MotorVoltage,
		"motor:current": data.MotorCurrent,
		"rpm":           data.RPM,
		"speed":         data.Speed,
		"raw-speed":     data.RawSpeed,
		"throttle":      map[bool]string{true: "on", false: "off"}[data.ThrottleOn],
	})

	_, err := pipe.Exec(tx.ctx)
	if err != nil {
		return fmt.Errorf("failed to send Status1: %v", err)
	}

	// Publish throttle state changes
	if err := tx.redis.Publish(tx.ctx, "engine-ecu throttle", nil).Err(); err != nil {
		return fmt.Errorf("failed to publish throttle state: %v", err)
	}

	return nil
}

func (tx *IPCTx) SendStatus2(data RedisStatus2) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	if err := tx.redis.HSet(tx.ctx, "engine-ecu",
		"temperature", data.Temperature,
	).Err(); err != nil {
		return fmt.Errorf("failed to send Status2: %v", err)
	}

	return nil
}

func (tx *IPCTx) SendStatus3(data RedisStatus3) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	pipe := tx.redis.Pipeline()

	pipe.HSet(tx.ctx, "engine-ecu",
		"odometer", data.Odometer,
	)

	// Also publish odometer updates
	pipe.Publish(tx.ctx, "engine-ecu odometer", nil)

	_, err := pipe.Exec(tx.ctx)
	if err != nil {
		return fmt.Errorf("failed to send Status3: %v", err)
	}

	return nil
}

func (tx *IPCTx) SendStatus4(data RedisStatus4) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	pipe := tx.redis.Pipeline()

	pipe.HSet(tx.ctx, "engine-ecu",
		"kers", map[bool]string{true: "on", false: "off"}[data.KersOn],
	)

	// Also publish KERS state changes
	pipe.Publish(tx.ctx, "engine-ecu kers", nil)

	_, err := pipe.Exec(tx.ctx)
	if err != nil {
		return fmt.Errorf("failed to send Status4: %v", err)
	}

	return nil
}

func (tx *IPCTx) SendStatus5(data RedisStatus5) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	if err := tx.redis.HSet(tx.ctx, "engine-ecu",
		"fw-version", fmt.Sprintf("%08X", data.FirmwareVersion),
	).Err(); err != nil {
		return fmt.Errorf("failed to send Status5: %v", err)
	}

	return nil
}

func (tx *IPCTx) SendKersReasonOff(reason KersReasonOff) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	pipe := tx.redis.Pipeline()

	reasonStr := "none"
	switch reason {
	case KersReasonOffCold:
		reasonStr = "cold"
	case KersReasonOffHot:
		reasonStr = "hot"
	}

	pipe.HSet(tx.ctx, "engine-ecu",
		"kers-reason-off", reasonStr,
	)

	// Also publish KERS reason off changes
	pipe.Publish(tx.ctx, "engine-ecu kers-reason-off", nil)

	_, err := pipe.Exec(tx.ctx)
	if err != nil {
		return fmt.Errorf("failed to send KERS reason off: %v", err)
	}

	return nil
}
