package main

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brutella/can"
	"golang.org/x/sys/unix"
)

type testSocket struct {
	rx      chan receivedFrame
	sends   chan can.Frame
	block   chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newTestSocket() *testSocket {
	return &testSocket{rx: make(chan receivedFrame, 256), sends: make(chan can.Frame, 256), entered: make(chan struct{})}
}
func (s *testSocket) Receive(ctx context.Context) (can.Frame, time.Time, string, error) {
	select {
	case e := <-s.rx:
		return e.frame, e.at, "", nil
	case <-ctx.Done():
		return can.Frame{}, time.Time{}, "", ctx.Err()
	}
}
func (s *testSocket) Send(ctx context.Context, f can.Frame, allowed func() bool) error {
	s.once.Do(func() { close(s.entered) })
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !allowed() {
		return errCANObsolete
	}
	select {
	case s.sends <- f:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *testSocket) Close() error { return nil }
func statusMotion() can.Frame      { return can.Frame{ID: frameStatus1, Length: 8} }
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestUnrelatedCANFramesDoNotOverflowECUQueue(t *testing.T) {
	s := newTestSocket()
	tr := newCANTransport(s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tr.receive(ctx) }()
	defer func() { cancel(); <-done }()

	for i := 0; i < 200; i++ {
		s.rx <- receivedFrame{frame: can.Frame{ID: 0x80000946, Length: 8}, at: time.Now()}
	}
	s.rx <- receivedFrame{frame: statusMotion(), at: time.Now()}
	select {
	case got := <-tr.rx:
		if got.frame.ID != frameStatus1 {
			t.Fatalf("unexpected queued event: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("ECU frame not delivered")
	}
	if drops := tr.drops.Load(); drops != 0 {
		t.Fatalf("unrelated traffic caused %d queue drops", drops)
	}
}

func TestBlockedTXDoesNotBlockReceiveOrDecoder(t *testing.T) {
	s := newTestSocket()
	s.block = make(chan struct{})
	tr := newCANTransport(s)
	ecu := newECU(tr, newLogger(LogLevelNone), []uint8{30, 60, 100})
	ecu.SetPowered(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var count atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- tr.Run(ctx, func(e receivedFrame) {
			if ecu.HandleFrameAt(e.frame, e.at) {
				count.Add(1)
			}
		})
	}()
	ecu.RequestStatus()
	select {
	case <-s.entered:
	case <-time.After(time.Second):
		t.Fatal("TX did not start")
	}
	for i := 0; i < 10; i++ {
		s.rx <- receivedFrame{frame: statusMotion(), at: time.Now()}
	}
	waitFor(t, func() bool { return count.Load() == 10 })
	ecu.mu.RLock()
	sent := ecu.gearsSentOnPower || ecu.stateAssertedToECU
	ecu.mu.RUnlock()
	if sent {
		t.Fatal("enqueue marked commands sent")
	}
	// Cancellation must terminate a stalled transmitter and the receive pump.
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown stalled")
	}
}

func TestTXQueueFullAndPowerGeneration(t *testing.T) {
	s := newTestSocket()
	tr := newCANTransport(s)
	ecu := newECU(tr, newLogger(LogLevelNone), nil)
	ecu.SetPowered(true)
	for i := 0; i < canQueueSize; i++ {
		if err := tr.Submit(txRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Submit(txRequest{}); !errors.Is(err, errCANQueueFull) {
		t.Fatalf("queue full: %v", err)
	}
	tr = newCANTransport(s)
	ecu.UpdateBus(tr)
	ecu.SetKersEnabled(true)
	if ecu.stateAssertedToECU {
		t.Fatal("queued assertion marked sent")
	}
	ecu.SetPowered(false)
	ecu.SetPowered(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tr.transmit(ctx); close(done) }()
	waitFor(t, func() bool { return len(tr.tx) == 0 })
	time.Sleep(5 * time.Millisecond)
	if len(s.sends) != 0 {
		t.Fatal("old power generation transmitted")
	}
	cancel()
	<-done
}

func TestTXCompletionOrderingAndPartialFailure(t *testing.T) {
	s := newTestSocket()
	tr := newCANTransport(s)
	ecu := newECU(tr, newLogger(LogLevelNone), []uint8{30, 60, 100})
	ecu.SetPowered(true)
	ecu.HandleFrame(statusMotion())
	if ecu.gearsSentOnPower || ecu.stateAssertedToECU {
		t.Fatal("queued commands claimed sent")
	}
	// Fail the control batch; complete only part of the gear batch.
	control, gears := <-tr.tx, <-tr.tx
	if len(control.frames) != 2 || control.frames[0].ID != frameEBSSet || control.frames[1].ID != frameControl {
		t.Fatal("control ordering")
	}
	if len(gears.frames) != 3 || gears.frames[0].Data[0] != 30 || gears.frames[2].Data[0] != 100 {
		t.Fatal("gear ordering")
	}
	control.done <- txResult{err: errors.New("write failed")}
	gears.done <- txResult{err: errors.New("partial batch")}
	ecu.HandleFrame(statusMotion())
	if ecu.gearsSentOnPower || ecu.stateAssertedToECU {
		t.Fatal("failed batch claimed sent")
	}
	control, gears = <-tr.tx, <-tr.tx
	control.done <- txResult{}
	gears.done <- txResult{}
	ecu.ApplyCommandedState()
	if !ecu.gearsSentOnPower || !ecu.stateAssertedToECU {
		t.Fatal("completed batches not recorded")
	}
	ecu.UpdateBus(newCANTransport(newTestSocket()))
	if ecu.gearsSentOnPower || ecu.stateAssertedToECU {
		t.Fatal("reconnect retained sent flags")
	}
	if control.allowed() {
		t.Fatal("old connection still authorized")
	}
}

func TestBlockedLegacyDispatchDoesNotBlockReceivePump(t *testing.T) {
	s := newTestSocket()
	tr := newCANTransport(s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() { done <- tr.Run(ctx, func(e receivedFrame) { once.Do(func() { close(entered); <-release }) }) }()
	s.rx <- receivedFrame{frame: statusMotion(), at: time.Now()}
	<-entered
	for i := 0; i < canQueueSize+1; i++ {
		s.rx <- receivedFrame{frame: statusMotion(), at: time.Now()}
	}
	waitFor(t, func() bool { return tr.drops.Load() > 0 })
	close(release)
	cancel()
	<-done
}

func TestKernelMetadata(t *testing.T) {
	now := time.Now()
	for _, size := range []int{8, 16} {
		for _, age := range []time.Duration{0, 500 * time.Millisecond, 500*time.Millisecond + 1, -1} {
			at := now.Add(-age)
			data := make([]byte, size)
			if size == 8 {
				binary.NativeEndian.PutUint32(data, uint32(at.Unix()))
				binary.NativeEndian.PutUint32(data[4:], uint32(at.Nanosecond()))
			} else {
				binary.NativeEndian.PutUint64(data, uint64(at.Unix()))
				binary.NativeEndian.PutUint64(data[8:], uint64(at.Nanosecond()))
			}
			got, err := kernelReceiveTime(data, now)
			want := age >= 0 && age <= frameMaxAge
			if (err == nil) != want || (want && !got.Equal(at)) {
				t.Fatalf("size=%d age=%v got=%v err=%v", size, age, got, err)
			}
		}
	}
	data := make([]byte, 16)
	binary.NativeEndian.PutUint64(data, uint64(now.Unix()))
	binary.NativeEndian.PutUint64(data[8:], uint64(now.Nanosecond()))
	stamp := unix.SocketControlMessage{Header: unix.Cmsghdr{Level: unix.SOL_SOCKET, Type: unix.SCM_TIMESTAMPNS}, Data: data}
	drops := unix.SocketControlMessage{Header: unix.Cmsghdr{Level: unix.SOL_SOCKET, Type: unix.SO_RXQ_OVFL}, Data: []byte{1, 0, 0, 0}}
	_, n, reason := receiveMetadata([]unix.SocketControlMessage{stamp, drops}, now, 0)
	if n != 1 || reason != "rx-overflow" {
		t.Fatal("overflow not invalidated")
	}
	if _, _, reason = receiveMetadata([]unix.SocketControlMessage{stamp, drops}, now, 1); reason != "" {
		t.Fatal("unchanged counter invalidated")
	}
	if _, _, reason = receiveMetadata(nil, now, 0); reason == "" {
		t.Fatal("missing timestamp accepted")
	}
	for _, data := range [][]byte{nil, make([]byte, 7), make([]byte, 8), make([]byte, 16)} {
		if _, err := kernelReceiveTime(data, now); err == nil {
			t.Fatal("invalid timestamp accepted")
		}
	}
}

// A stalled dispatcher drops the newest frames and counts them; the ordered
// backlog already queued is kept.
func TestReceiveQueueOverflowDropsNewestAndCounts(t *testing.T) {
	tr := newCANTransport(newTestSocket())
	for i := 0; i < canQueueSize+3; i++ {
		frame := statusMotion()
		frame.Data[0] = byte(i)
		tr.enqueue(receivedFrame{frame: frame, at: time.Now()})
	}
	if got := tr.drops.Load(); got != 3 {
		t.Fatalf("drops=%d, want 3", got)
	}
	for i := 0; i < canQueueSize; i++ {
		if e := <-tr.rx; e.frame.Data[0] != byte(i) {
			t.Fatalf("backlog reordered at %d", i)
		}
	}
}

func TestDispatcherDropsFramesThatWaitedTooLong(t *testing.T) {
	s := newTestSocket()
	tr := newCANTransport(s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan can.Frame, 4)
	done := make(chan error, 1)
	go func() { done <- tr.Run(ctx, func(e receivedFrame) { got <- e.frame }) }()
	stale := statusMotion()
	stale.Data[0] = 1
	fresh := statusMotion()
	fresh.Data[0] = 2
	s.rx <- receivedFrame{frame: stale, at: time.Now().Add(-frameMaxAge - time.Second)}
	s.rx <- receivedFrame{frame: fresh, at: time.Now()}
	select {
	case f := <-got:
		if f.Data[0] != 2 {
			t.Fatalf("stale frame dispatched: %d", f.Data[0])
		}
	case <-time.After(time.Second):
		t.Fatal("fresh frame not dispatched")
	}
	cancel()
	<-done
}
