package main

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	ipc "github.com/librescoot/redis-ipc"
	"github.com/redis/go-redis/v9"
)

// newTestTx wires an IPCTx against an in-process Redis.
func newTestTx(t *testing.T) (*IPCTx, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	host, port, err := net.SplitHostPort(mr.Addr())
	if err != nil {
		t.Fatalf("split miniredis addr: %v", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse miniredis port: %v", err)
	}

	client, err := ipc.New(ipc.WithAddress(host), ipc.WithPort(p))
	if err != nil {
		t.Fatalf("ipc.New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return newIPCTx(context.Background(), client, newLogger(LogLevelNone)), mr
}

// The five hash-change notifications used to go out on channels literally named
// "engine-ecu <field>", which nothing subscribes to. Channel is the hash name,
// message is the field name.
func TestPublishNotificationsUseHashChannelAndFieldMessage(t *testing.T) {
	tx, mr := newTestTx(t)

	sub := redis.NewClient(&redis.Options{Addr: mr.Addr()}).Subscribe(context.Background(), ecuChannel)
	t.Cleanup(func() { sub.Close() })
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"throttle", tx.PublishThrottle, "throttle"},
		{"odometer", tx.PublishOdometer, "odometer"},
		{"kers", tx.PublishKERS, "kers"},
		{"kers-reason-off", tx.PublishKERSReasonOff, "kers-reason-off"},
		{"regen-available", tx.PublishRegen, "regen-available"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); err != nil {
				t.Fatalf("publish: %v", err)
			}
			msg := receive(t, sub)
			if msg.Channel != ecuChannel {
				t.Errorf("channel = %q, want %q", msg.Channel, ecuChannel)
			}
			if msg.Payload != c.want {
				t.Errorf("payload = %q, want %q", msg.Payload, c.want)
			}
		})
	}
}

func receive(t *testing.T, sub *redis.PubSub) *redis.Message {
	t.Helper()
	select {
	case msg := <-sub.Channel():
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification")
		return nil
	}
}
