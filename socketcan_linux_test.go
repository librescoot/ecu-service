package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brutella/can"
)

// Needs a CAN interface. Run in a private namespace:
//
//	ECU_TEST_CAN=vcan0 unshare -rn sh -c 'ip link add vcan0 type vcan && ip link set vcan0 up && go test -run SocketCAN .'
func vcanDevice(t *testing.T) string {
	dev := os.Getenv("ECU_TEST_CAN")
	if dev == "" {
		t.Skip("ECU_TEST_CAN not set")
	}
	return dev
}

func ctxSwitches(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "voluntary_ctxt_switches:") || strings.HasPrefix(line, "nonvoluntary_ctxt_switches:") {
			n, _ := strconv.Atoi(strings.TrimSpace(line[strings.Index(line, ":")+1:]))
			total += n
		}
	}
	return total
}

func TestSocketCANReceiveBlocksWithoutWakeups(t *testing.T) {
	dev := vcanDevice(t)
	rx, err := openCANTransport(dev)
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()
	tx, err := openCANTransport(dev)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()

	done := make(chan error, 1)
	go func() {
		_, _, _, err := rx.socket.Receive(context.Background())
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	before := ctxSwitches(t)
	time.Sleep(2 * time.Second)
	if n := ctxSwitches(t) - before; n > 20 {
		t.Fatalf("idle receive caused %d context switches in 2s", n)
	}

	frame := can.Frame{ID: frameStatus1, Length: 8}
	if err := tx.socket.Send(context.Background(), frame, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("frame not delivered")
	}
}

func TestSocketCANReceiveHonoursDeadlineCancelAndClose(t *testing.T) {
	dev := vcanDevice(t)
	tr, err := openCANTransport(dev)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	start := time.Now()
	_, _, _, err = tr.socket.Receive(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("deadline: err=%v after %v", err, time.Since(start))
	}

	ctx, cancel = context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start = time.Now()
	_, _, _, err = tr.socket.Receive(ctx)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("cancel: err=%v after %v", err, time.Since(start))
	}

	// A cancelled call must not leak its past deadline into the next one.
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, _, _, err = tr.socket.Receive(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("after cancel: %v", err)
	}

	errc := make(chan error, 1)
	go func() { _, _, _, err := tr.socket.Receive(context.Background()); errc <- err }()
	time.Sleep(50 * time.Millisecond)
	_ = tr.Close()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("receive after close returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock receive")
	}
}
