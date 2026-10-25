package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/brutella/can"
	"github.com/redis/go-redis/v9"
)

func activeTimedBoostFrame() can.Frame {
	return makeFrame(frameStatus4, []byte{0x55, 0xb1, 2 | 8 | 16 | 32 | 64, 3, 0, 100, 0, 0})
}

func TestTimedBoostDecodeAndValidation(t *testing.T) {
	now := time.Now()
	frame := activeTimedBoostFrame()
	status := decodeTimedBoost(frame, now)
	if status.State != "active" || !status.Enabled || !status.Active || status.EligiblePacks != 2 || status.RemainingMS != 10000 || status.Reason != "none" {
		t.Fatalf("unexpected status: %+v", status)
	}
	cases := []struct {
		index int
		value byte
	}{
		{0, 0x59}, {1, 0}, {2, 0xff}, {2, 1 | 8 | 16 | 32 | 64}, {2, 2 | 8 | 16},
		{3, 0}, {3, 0xfc}, {4, 1}, {6, 1},
	}
	for _, test := range cases {
		bad := frame
		bad.Data[test.index] = test.value
		if got := decodeTimedBoost(bad, now); !got.ObservedAt.IsZero() {
			t.Errorf("accepted malformed byte%d=%02x: %+v", test.index, test.value, got)
		}
	}
	short := frame
	short.Length = 1
	if !decodeTimedBoost(short, now).ObservedAt.IsZero() || !decodeTimedBoost(frame, time.Time{}).ObservedAt.IsZero() {
		t.Fatal("accepted short or undated status")
	}
	inhibited := makeFrame(frameStatus4, []byte{0x59, 0xb1, 5 | 64, 3 | (1 << 2), 0, 0, 0, 0})
	if got := decodeTimedBoost(inhibited, now); got.State != "inhibited" || got.Reason != "heartbeat-expired" || got.Active || got.Enabled {
		t.Fatalf("inhibited status: %+v", got)
	}
}

func TestTimedBoostRequiresCurrentPowerGenerationIdentity(t *testing.T) {
	ecu := newTestECU()
	ecu.SetPowered(true)
	ecu.HandleFrame(activeTimedBoostFrame())
	if !ecu.TimedBoost().ObservedAt.IsZero() {
		t.Fatal("accepted boost extension before identity")
	}
	identity := makeFrame(frameStatus5, []byte{0, 0, 0, 0, 3, 0x45, 0x10, 2})
	ecu.HandleFrame(identity)
	ecu.HandleFrame(activeTimedBoostFrame())
	if !ecu.TimedBoost().Active {
		t.Fatal("valid controller boost was not exposed")
	}
	ecu.SetPowered(false)
	if !ecu.TimedBoost().ObservedAt.IsZero() {
		t.Fatal("powered-off boost remained valid")
	}
	ecu.SetPowered(true)
	ecu.HandleFrame(activeTimedBoostFrame())
	if !ecu.TimedBoost().ObservedAt.IsZero() {
		t.Fatal("identity survived a power generation")
	}
	ecu.HandleFrame(identity)
	ecu.HandleFrame(activeTimedBoostFrame())
	ecu.HandleFrame(makeFrame(frameStatus4, []byte{0x04}))
	if !ecu.TimedBoost().ObservedAt.IsZero() || !ecu.BoostEnabled() {
		t.Fatal("legacy paired-bit status compatibility changed")
	}
}

func TestTimedBoostFreshnessAndLegacyIdentity(t *testing.T) {
	ecu := newTestECU()
	ecu.timedBoost = decodeTimedBoost(activeTimedBoostFrame(), time.Now().Add(-timedBoostTTL))
	if !ecu.TimedBoost().ObservedAt.IsZero() {
		t.Fatal("stale boost exposed")
	}
	ecu.timedBoost.ObservedAt = time.Now().Add(time.Second)
	if !ecu.TimedBoost().ObservedAt.IsZero() {
		t.Fatal("future boost exposed")
	}
	for _, version := range [][2]byte{{0x10, 1}, {0x74, 2}} {
		ecu.HandleFrame(makeFrame(frameStatus5, []byte{0, 0, 0, 0, 3, 0x45, version[0], version[1]}))
		ecu.HandleFrame(activeTimedBoostFrame())
		if !ecu.TimedBoost().ObservedAt.IsZero() {
			t.Fatalf("accepted non-capable identity %x", version)
		}
	}
}

func TestTimedBoostRedisExpiryAndNoTimestampRenewal(t *testing.T) {
	tx, mr := newTestTx(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })
	subscriber := client.Subscribe(context.Background(), timedBoostKey)
	t.Cleanup(func() { subscriber.Close() })
	if _, err := subscriber.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := Status{TimedBoost: decodeTimedBoost(activeTimedBoostFrame(), time.Now().Add(-50*time.Millisecond))}
	if err := tx.SendStatus(status); err != nil {
		t.Fatal(err)
	}
	message := receive(t, subscriber)
	stored, err := mr.Get(timedBoostKey)
	if err != nil || stored != message.Payload {
		t.Fatal("notification was not preceded by its stored status")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stored), &payload); err != nil || payload["valid"] != true || payload["active"] != true {
		t.Fatalf("payload=%s err=%v", stored, err)
	}
	initialTTL := mr.TTL(timedBoostKey)
	if initialTTL <= 0 || initialTTL > 450*time.Millisecond {
		t.Fatalf("TTL is not based on observation age: %v", initialTTL)
	}
	mr.FastForward(200 * time.Millisecond)
	if err := tx.SendStatus(status); err != nil {
		t.Fatal(err)
	}
	if got := mr.TTL(timedBoostKey); got > initialTTL-200*time.Millisecond {
		t.Fatalf("unchanged observation renewed TTL: %v", got)
	}
	mr.FastForward(time.Second)
	if mr.Exists(timedBoostKey) {
		t.Fatal("boost evidence survived its source deadline")
	}
	mr.SetTime(time.Now())
	if err := tx.SendStatus(Status{}); err != nil {
		t.Fatal(err)
	}
	stored, _ = mr.Get(timedBoostKey)
	if stored != `{"version":1,"valid":false}` {
		t.Fatalf("invalid evidence retained boost fields: %s", stored)
	}
}

func TestTimedBoostPublicationFailureCanRetry(t *testing.T) {
	tx, mr := newTestTx(t)
	if err := tx.SendStatus(Status{}); err != nil {
		t.Fatal(err)
	}
	status := Status{TimedBoost: decodeTimedBoost(activeTimedBoostFrame(), time.Now())}
	mr.SetError("ERR unavailable")
	if err := tx.SendStatus(status); err == nil {
		t.Fatal("expected publication failure")
	}
	mr.SetError("")
	if err := tx.SendStatus(status); err != nil {
		t.Fatal(err)
	}
	stored, _ := mr.Get(timedBoostKey)
	var payload map[string]any
	if err := json.Unmarshal([]byte(stored), &payload); err != nil || payload["valid"] != true {
		t.Fatalf("same status did not retry: %s", stored)
	}
}
