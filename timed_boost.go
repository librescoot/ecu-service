package main

import (
	"encoding/binary"
	"encoding/json"
	"math/bits"
	"time"

	"github.com/brutella/can"
	"github.com/redis/go-redis/v9"
)

const timedBoostTTL = 500 * time.Millisecond
const timedBoostKey = "engine-ecu:boost"

var timedBoostStates = [...]string{"disabled", "armed", "active", "waiting-release", "cooldown", "inhibited"}
var timedBoostReasons = [...]string{"none", "heartbeat-expired", "supply-unavailable", "motor-inhibited", "no-headroom", "limit-write-failed"}

type TimedBoostStatus struct {
	ObservedAt     time.Time
	State          string
	Reason         string
	Enabled        bool
	Active         bool
	HeartbeatFresh bool
	SupplyFresh    bool
	EligiblePacks  uint8
	RemainingMS    uint32
	CooldownMS     uint32
}

func decodeTimedBoost(frame can.Frame, observed time.Time) TimedBoostStatus {
	if frame.Length != 8 || frame.Data[1] != 0xb1 || observed.IsZero() {
		return TimedBoostStatus{}
	}
	flags := frame.Data[2]
	state := flags & 7
	packs, reason := frame.Data[3]&3, frame.Data[3]>>2
	if flags&0x80 != 0 || int(state) >= len(timedBoostStates) || int(reason) >= len(timedBoostReasons) {
		return TimedBoostStatus{}
	}
	enabled, active := flags&8 != 0, flags&16 != 0
	heartbeat, supply := flags&32 != 0, flags&64 != 0
	remaining := uint32(binary.BigEndian.Uint16(frame.Data[4:6])) * 100
	cooldown := uint32(binary.BigEndian.Uint16(frame.Data[6:8])) * 100
	pair := frame.Data[0] & 0x0c
	if (pair != 4 && pair != 8) || (pair == 4) != enabled ||
		(active != (state == 2)) || (active && (!enabled || remaining == 0 || cooldown != 0)) ||
		(enabled && (!heartbeat || !supply || packs == 0)) ||
		remaining > 20000 || cooldown > 3600000 || (state == 4 && cooldown == 0) ||
		(state == 1 && (!enabled || remaining == 0 || cooldown != 0)) ||
		(state == 3 && (remaining != 0 || cooldown != 0)) || (enabled && (state == 0 || state == 5)) ||
		(active && reason != 0) || (state == 5 && reason == 0) {
		return TimedBoostStatus{}
	}
	return TimedBoostStatus{
		ObservedAt: observed, State: timedBoostStates[state], Reason: timedBoostReasons[reason], Enabled: enabled, Active: active,
		HeartbeatFresh: heartbeat, SupplyFresh: supply, EligiblePacks: uint8(bits.OnesCount8(packs)),
		RemainingMS: remaining, CooldownMS: cooldown,
	}
}

func (b *ECU) updateTimedBoostLocked(frame can.Frame) {
	if !b.timedBoostIdentity {
		b.timedBoost = TimedBoostStatus{}
		return
	}
	b.timedBoost = decodeTimedBoost(frame, b.lastFrameTime)
}

func (b *ECU) TimedBoost() TimedBoostStatus {
	b.mu.RLock()
	defer b.mu.RUnlock()
	age := time.Since(b.timedBoost.ObservedAt)
	if b.powerCmd == powerOff || b.timedBoost.ObservedAt.IsZero() || age < 0 || age >= timedBoostTTL {
		return TimedBoostStatus{}
	}
	return b.timedBoost
}

func (tx *IPCTx) sendTimedBoost(status TimedBoostStatus) error {
	now := time.Now()
	deadline := status.ObservedAt.Add(timedBoostTTL)
	valid := !status.ObservedAt.IsZero() && !status.ObservedAt.After(now) && deadline.After(now)
	var payload []byte
	var err error
	if valid {
		payload, err = json.Marshal(struct {
			Version        int       `json:"version"`
			Valid          bool      `json:"valid"`
			ObservedAt     time.Time `json:"observed_at"`
			State          string    `json:"state"`
			Reason         string    `json:"reason"`
			Enabled        bool      `json:"enabled"`
			Active         bool      `json:"active"`
			HeartbeatFresh bool      `json:"heartbeat_fresh"`
			SupplyFresh    bool      `json:"supply_fresh"`
			EligiblePacks  uint8     `json:"eligible_packs"`
			RemainingMS    uint32    `json:"remaining_ms"`
			CooldownMS     uint32    `json:"cooldown_ms"`
		}{Version: 1, Valid: true, ObservedAt: status.ObservedAt.UTC(), State: status.State, Reason: status.Reason,
			Enabled: status.Enabled, Active: status.Active, HeartbeatFresh: status.HeartbeatFresh,
			SupplyFresh: status.SupplyFresh, EligiblePacks: status.EligiblePacks, RemainingMS: status.RemainingMS, CooldownMS: status.CooldownMS})
	} else {
		payload = []byte(`{"version":1,"valid":false}`)
		deadline = now.Add(timedBoostTTL)
	}
	if err != nil {
		return err
	}
	_, err = tx.raw.TxPipelined(tx.ctx, func(pipe redis.Pipeliner) error {
		pipe.Do(tx.ctx, "SET", timedBoostKey, payload, "PXAT", deadline.UnixMilli())
		pipe.Publish(tx.ctx, timedBoostKey, payload)
		return nil
	})
	return err
}
