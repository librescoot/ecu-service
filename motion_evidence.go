package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/brutella/can"
	"github.com/redis/go-redis/v9"
)

const motionEvidenceChannel = "engine-ecu:motion"
const motionEvidenceMaxAge = 500 * time.Millisecond
const motionPublishTimeout = 100 * time.Millisecond

func motionTimeFresh(at, now time.Time) bool {
	age, wallAge := now.Sub(at), time.Duration(now.UnixNano()-at.UnixNano())
	return !at.IsZero() && age >= 0 && age <= motionEvidenceMaxAge && wallAge >= 0 && wallAge <= motionEvidenceMaxAge && !clockJump(at, now)
}

type motionEvidence struct {
	Version    int        `json:"version"`
	Source     string     `json:"source"`
	Sequence   uint64     `json:"sequence"`
	Valid      bool       `json:"valid"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	RPM        *uint16    `json:"rpm,omitempty"`
	RawSpeed   *uint8     `json:"raw_speed,omitempty"`
	ThrottleOn *bool      `json:"throttle_on,omitempty"`
	Reason     string     `json:"reason,omitempty"`
}

func (e motionEvidence) MarshalJSON() ([]byte, error) {
	type wire motionEvidence
	value := wire(e)
	if e.ObservedAt != nil {
		at := e.ObservedAt.UTC()
		value.ObservedAt = &at
	}
	return json.Marshal(value)
}

type motionSource struct {
	source   string
	sequence uint64
}

func newMotionSource() *motionSource {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return &motionSource{source: hex.EncodeToString(id[:])}
}
func (s *motionSource) envelope() *motionEvidence {
	s.sequence++
	return &motionEvidence{Version: 1, Source: s.source, Sequence: s.sequence}
}
func (s *motionSource) invalid(reason string) *motionEvidence {
	e := s.envelope()
	e.Reason = reason
	return e
}
func decodeMotionValues(frame can.Frame) (rpm uint16, speed uint8, throttle bool) {
	return binary.BigEndian.Uint16(frame.Data[4:6]), frame.Data[6], frame.Data[7]&1 != 0
}
func (s *motionSource) observe(frame can.Frame, at time.Time) *motionEvidence {
	if frame.ID != frameStatus1 || frame.Length != 8 {
		return s.invalid("malformed-status1")
	}
	e := s.envelope()
	rpm, speed, throttle := decodeMotionValues(frame)
	e.Valid, e.ObservedAt, e.RPM, e.RawSpeed, e.ThrottleOn = true, &at, &rpm, &speed, &throttle
	return e
}
func invalidMotion(e *motionEvidence, reason string) *motionEvidence {
	return &motionEvidence{Version: 1, Source: e.Source, Sequence: e.Sequence, Reason: reason}
}

type motionFeed struct {
	queue   chan *motionEvidence
	allowed func(time.Time) bool
}

func newMotionFeed() *motionFeed { return &motionFeed{queue: make(chan *motionEvidence, canQueueSize)} }
func (f *motionFeed) offer(e *motionEvidence) {
	select {
	case f.queue <- e:
		return
	default:
	}
	for {
		select {
		case <-f.queue:
		default:
			goto drained
		}
	}
drained:
	// Preserve this observation's sequence as an invalidation, never coalesce
	// away a throttle pulse while retaining the preceding stopped window.
	f.queue <- invalidMotion(e, "publish-queue-overflow")
}

func motionRedisOptions(base *redis.Options) *redis.Options {
	opts := *base
	opts.ReadTimeout, opts.WriteTimeout, opts.DialTimeout = motionPublishTimeout, motionPublishTimeout, motionPublishTimeout
	opts.ContextTimeoutEnabled = true
	opts.MaxRetries = -1
	opts.PoolSize, opts.MinIdleConns, opts.MaxActiveConns = 1, 0, 1
	opts.PoolTimeout = motionPublishTimeout
	return &opts
}

func (f *motionFeed) run(ctx context.Context, send func(context.Context, []byte) error) {
	publish := func(e *motionEvidence) {
		payload, err := json.Marshal(e)
		if err != nil {
			return
		}
		callCtx, cancel := context.WithTimeout(ctx, motionPublishTimeout)
		_ = send(callCtx, payload) // Never retry a stale observation; the next sequence exposes loss.
		cancel()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-f.queue:
			if e.Valid && (e.ObservedAt == nil || !motionTimeFresh(*e.ObservedAt, time.Now())) {
				e = invalidMotion(e, "publication-expired")
			}
			if e.Valid && f.allowed != nil && !f.allowed(*e.ObservedAt) {
				e = invalidMotion(e, "powered-off")
			}
			publish(e)
		}
	}
}
