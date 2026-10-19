package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brutella/can"
	"github.com/redis/go-redis/v9"
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

func TestReceiveQueueOverflowResetsSourceBeforeFreshData(t *testing.T) {
	tr := newCANTransport(newTestSocket())
	source := newMotionSource()
	old := source.source
	for i := 0; i <= canQueueSize; i++ {
		tr.enqueue(receivedFrame{motion: source.observe(statusMotion(), time.Now())}, &source)
	}
	e := <-tr.rx
	if e.motion.Valid || e.motion.Reason != "rx-queue-overflow" || source.source == old || tr.drops.Load() != canQueueSize+1 {
		t.Fatal("overflow hidden")
	}
	tr.enqueue(receivedFrame{motion: source.observe(statusMotion(), time.Now())}, &source)
	next := <-tr.rx
	if next.motion.Source != e.motion.Source || next.motion.Sequence != e.motion.Sequence+1 {
		t.Fatal("resumption not ordered")
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
			want := age >= 0 && age <= motionEvidenceMaxAge
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

func TestMotionContractAndPublishOverflow(t *testing.T) {
	source := newMotionSource()
	f := statusMotion()
	a := source.observe(f, time.Now())
	b := source.observe(f, time.Now())
	if a.Sequence+1 != b.Sequence || a.Source != b.Source || *a.RPM != 0 || *a.RawSpeed != 0 || *a.ThrottleOn {
		t.Fatal("unchanged evidence missing")
	}
	f.Data[4], f.Data[5], f.Data[6], f.Data[7] = 1, 2, 3, 1
	e := source.observe(f, time.Now())
	if *e.RPM != 258 || *e.RawSpeed != 3 || !*e.ThrottleOn {
		t.Fatal("wrong decoded values")
	}
	for _, frame := range []can.Frame{{ID: frameStatus1, Length: 7}, {ID: frameStatus1 | unix.CAN_RTR_FLAG, Length: 8}, {ID: frameStatus1 | unix.CAN_EFF_FLAG, Length: 8}} {
		if source.observe(frame, time.Now()).Valid {
			t.Fatal("non-motion frame accepted")
		}
	}
	invalid := source.invalid("transport-error")
	data, _ := json.Marshal(invalid)
	var fields map[string]any
	_ = json.Unmarshal(data, &fields)
	if _, ok := fields["rpm"]; ok {
		t.Fatal("invalidation fabricated zero motion")
	}
	feed := newMotionFeed()
	for i := 0; i <= canQueueSize; i++ {
		feed.offer(source.observe(statusMotion(), time.Now()))
	}
	if e := <-feed.queue; e.Valid || e.Reason != "publish-queue-overflow" {
		t.Fatal("publication loss hidden")
	}
}

func TestMotionDropsExpiredQueuedData(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feed := newMotionFeed()
	source := newMotionSource()
	feed.offer(source.observe(statusMotion(), time.Now().Add(-time.Second)))
	done := make(chan struct{})
	go func() {
		feed.run(ctx, func(ctx context.Context, payload []byte) error {
			var e motionEvidence
			if err := json.Unmarshal(payload, &e); err != nil {
				t.Error(err)
			}
			if e.Valid {
				t.Error("stale frame replayed")
			}
			cancel()
			return nil
		})
		close(done)
	}()
	<-done
}

// Cross-repository integration fixture: kernel-timestamped datagrams containing
// CAN frames, the real receive adapter/decoder/publisher, and isolated Redis.
// No CAN interface or vehicle hardware is opened.
func TestMotionIPCProducerFixture(t *testing.T) {
	path := os.Getenv("MOTION_TEST_REDIS_SOCKET")
	if path == "" {
		t.Skip("cross-repository integration fixture")
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pair[1])
	if err := unix.SetsockoptInt(pair[0], unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1); err != nil {
		t.Fatal(err)
	}
	tr := newCANTransport(&timestampSocket{fd: pair[0]})
	client := redis.NewClient(motionRedisOptions(&redis.Options{Network: "unix", Addr: path}))
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	feed := newMotionFeed()
	ecu := newECU(nil, newLogger(LogLevelNone), nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		feed.run(ctx, func(ctx context.Context, payload []byte) error {
			err := client.Publish(ctx, motionEvidenceChannel, payload).Err()
			if err != nil {
				t.Logf("motion publication failed: %v; payload=%s", err, payload)
			}
			return err
		})
	}()
	go func() {
		defer wg.Done()
		_ = tr.Run(ctx, func(e receivedFrame) {
			if e.motion != nil {
				if e.motion.Valid && !ecu.HandleFrameAt(e.frame, e.at) {
					e.motion = invalidMotion(e.motion, "decoder-rejected")
				}
				if !e.motion.Valid {
					t.Logf("motion invalidated: %+v", *e.motion)
				}
				feed.offer(e.motion)
			}
		})
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	data, _ := can.Marshal(statusMotion())
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-ticker.C:
			if _, err := unix.Write(pair[1], data); err != nil {
				t.Error(err)
				cancel()
			}
		}
	}
	wg.Wait()
}
