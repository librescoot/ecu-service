package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brutella/can"
)

const canQueueSize = 64
const canTXTimeout = 100 * time.Millisecond

var errCANQueueFull = errors.New("CAN transmit queue full")
var errCANObsolete = errors.New("CAN command expired or superseded")

type canSocket interface {
	Receive(context.Context) (can.Frame, time.Time, string, error)
	Send(context.Context, can.Frame, func() bool) error
	Close() error
}

type txResult struct{ err error }
type txRequest struct {
	frames  []can.Frame
	expires time.Time
	allowed func() bool
	done    chan txResult
}

type canTransmitter interface{ Submit(txRequest) error }

type receivedFrame struct {
	frame  can.Frame
	at     time.Time
	motion *motionEvidence
}

type canTransport struct {
	socket   canSocket
	rx       chan receivedFrame
	tx       chan txRequest
	closed   atomic.Bool
	submitMu sync.Mutex
	drops    atomic.Uint64
}

func newCANTransport(socket canSocket) *canTransport {
	return &canTransport{socket: socket, rx: make(chan receivedFrame, canQueueSize), tx: make(chan txRequest, canQueueSize)}
}

func (t *canTransport) Submit(r txRequest) error {
	t.submitMu.Lock()
	defer t.submitMu.Unlock()
	if t.closed.Load() {
		return errCANObsolete
	}
	select {
	case t.tx <- r:
		return nil
	default:
		return errCANQueueFull
	}
}

func (t *canTransport) Close() error {
	t.submitMu.Lock()
	t.closed.Store(true)
	t.submitMu.Unlock()
	return t.socket.Close()
}

func (t *canTransport) transmit(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-t.tx:
			err := errCANObsolete
			if ctx.Err() == nil && commandCurrent(r.expires, time.Now()) && r.allowed() {
				sendCtx, cancel := context.WithDeadline(ctx, r.expires)
				err = nil
				for _, frame := range r.frames {
					if !r.allowed() {
						err = errCANObsolete
						break
					}
					if err = t.socket.Send(sendCtx, frame, r.allowed); err != nil {
						break
					}
				}
				cancel()
			}
			r.done <- txResult{err: err}
		}
	}
}

func (t *canTransport) receive(ctx context.Context) error {
	source := newMotionSource()
	var previous time.Time
	lastMotion := time.Now()
	invalidated := false
	for ctx.Err() == nil {
		if !invalidated && !motionTimeFresh(lastMotion, time.Now()) {
			t.enqueue(receivedFrame{motion: source.invalid("silence")}, &source)
			invalidated = true
		}
		readCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		frame, at, reason, err := t.socket.Receive(readCtx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			continue
		}
		if err != nil {
			return err
		}
		now := time.Now()
		if clockJump(previous, now) {
			reason = "clock-change"
		}
		previous = now
		if reason != "" {
			source = newMotionSource()
			t.enqueue(receivedFrame{motion: source.invalid(reason)}, &source)
			invalidated = true
			continue
		}
		event := receivedFrame{frame: frame, at: at}
		if frame.ID == frameStatus1 {
			event.motion = source.observe(frame, at)
			lastMotion, invalidated = at, !event.motion.Valid
		}
		t.enqueue(event, &source)
	}
	return ctx.Err()
}

func (t *canTransport) enqueue(e receivedFrame, source **motionSource) {
	select {
	case t.rx <- e:
		return
	default:
	}
	// Losing any frame can conceal a throttle pulse or a transport fault.
	// Discard the backlog and announce a new receive generation before resuming.
	for {
		select {
		case <-t.rx:
			t.drops.Add(1)
		default:
			goto drained
		}
	}
drained:
	t.drops.Add(1)
	*source = newMotionSource()
	t.rx <- receivedFrame{motion: (*source).invalid("rx-queue-overflow")}
}

// The socket pump and TX worker never execute Redis or decoder callbacks.
// The ordered dispatcher may block on legacy publication without blocking RX.
func (t *canTransport) Run(ctx context.Context, dispatch func(receivedFrame)) error {
	if t.closed.Load() {
		return errCANObsolete
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	stopped := make(chan error, 1)
	wg.Add(2)
	go func() { defer wg.Done(); stopped <- t.receive(ctx); cancel() }()
	go func() { defer wg.Done(); t.transmit(ctx) }()
	defer func() {
		cancel()
		_ = t.Close()
		wg.Wait()
		for {
			select {
			case r := <-t.tx:
				r.done <- txResult{err: errCANObsolete}
			default:
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			select {
			case err := <-stopped:
				return err
			default:
				return ctx.Err()
			}
		case err := <-stopped:
			return err
		case e := <-t.rx:
			if e.motion == nil || e.motion.Valid {
				if !motionTimeFresh(e.at, time.Now()) {
					if e.motion != nil {
						e.motion = invalidMotion(e.motion, "dispatch-expired")
					} else {
						continue
					}
					e.frame, e.at = can.Frame{}, time.Time{}
				}
			}
			dispatch(e)
		}
	}
}

func commandCurrent(deadline, now time.Time) bool {
	return now.Before(deadline) && now.UnixNano() < deadline.UnixNano() && !clockJump(deadline, now)
}

// time.Sub uses the monotonic clock; UnixNano observes wall-clock steps.
func clockJump(previous, now time.Time) bool {
	if previous.IsZero() {
		return false
	}
	delta := time.Duration(now.UnixNano()-previous.UnixNano()) - now.Sub(previous)
	return delta > 5*time.Millisecond || delta < -5*time.Millisecond
}
