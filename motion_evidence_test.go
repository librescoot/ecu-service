package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestMotionWireFixtures(t *testing.T) {
	at, _ := time.Parse(time.RFC3339Nano, "2026-10-17T10:00:00.123456789Z")
	source := &motionSource{source: "cd0ed924a45d4af18b65395fd632bd11", sequence: 41}
	cases := map[string]*motionEvidence{"motion-valid.json": source.observe(statusMotion(), at), "motion-invalid.json": source.invalid("rx-overflow")}
	for name, e := range cases {
		expected, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		actual, _ := json.Marshal(e)
		var a, b any
		if json.Unmarshal(actual, &a) != nil || json.Unmarshal(expected, &b) != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("wire contract mismatch: %s\n%s", name, actual)
		}
	}
}

func TestMotionRedisHasRealIODeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
			_, _ = io.Copy(io.Discard, conn)
		}
	}()
	client := redis.NewClient(motionRedisOptions(&redis.Options{Addr: listener.Addr().String()}))
	defer client.Close()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), motionPublishTimeout)
	defer cancel()
	err = client.Publish(ctx, motionEvidenceChannel, "test").Err()
	select {
	case conn := <-accepted:
		_ = conn.Close()
	default:
	}
	if err == nil {
		t.Fatal("nonresponsive Redis reported success")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("network deadline ineffective: %s", elapsed)
	}
}

func TestPowerOffInvalidatesBlockedSend(t *testing.T) {
	socket := newTestSocket()
	socket.block = make(chan struct{})
	tr := newCANTransport(socket)
	ecu := newECU(tr, newLogger(LogLevelNone), nil)
	ecu.SetPowered(true)
	ecu.RequestStatus()
	pending := ecu.pendingTX[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { tr.transmit(ctx); close(done) }()
	<-socket.entered
	ecu.SetPowered(false)
	close(socket.block)
	select {
	case result := <-pending.done:
		if result.err == nil {
			t.Fatal("power-off command sent")
		}
	case <-time.After(time.Second):
		t.Fatal("TX blocked")
	}
	if len(socket.sends) != 0 {
		t.Fatal("write occurred after power-off")
	}
	cancel()
	<-done
}

func TestStatusRequestQueueFullIsExplicit(t *testing.T) {
	tr := newCANTransport(newTestSocket())
	ecu := newECU(tr, newLogger(LogLevelNone), []uint8{50})
	ecu.SetPowered(true)
	for i := 0; i < canQueueSize; i++ {
		if err := tr.Submit(txRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ecu.SetGear(1); err != errCANQueueFull {
		t.Fatalf("queue overflow hidden: %v", err)
	}
	if ecu.gearsSentOnPower {
		t.Fatal("full queue marked startup complete")
	}
}

func TestSilenceInvalidatesDespiteUnrelatedTraffic(t *testing.T) {
	socket := newTestSocket()
	tr := newCANTransport(socket)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := make(chan *motionEvidence, 10)
	done := make(chan error, 1)
	go func() {
		done <- tr.Run(ctx, func(e receivedFrame) {
			if e.motion != nil {
				received <- e.motion
			}
		})
	}()
	socket.rx <- receivedFrame{frame: statusMotion(), at: time.Now()}
	first := <-received
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ticker.C:
			frame := statusMotion()
			frame.ID = frameStatus2
			socket.rx <- receivedFrame{frame: frame, at: time.Now()}
		case e := <-received:
			if e.Valid || e.Reason != "silence" || e.Sequence != first.Sequence+1 {
				t.Fatalf("wrong invalidation: %+v", e)
			}
			cancel()
			<-done
			return
		case <-timer.C:
			t.Fatal("unrelated frames renewed motion")
		}
	}
}
