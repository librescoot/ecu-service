package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/brutella/can"
	"golang.org/x/sys/unix"
)

// timestampSocket reads through the Go runtime's network poller, so a blocked
// Receive costs no wake-ups. The os.File owns fd; Send writes to the same
// descriptor under mu, which Close takes before releasing it.
type timestampSocket struct {
	mu    sync.RWMutex
	fd    int
	file  *os.File
	rc    syscall.RawConn
	drops uint32
}

func openCANTransport(device string) (*canTransport, error) {
	iface, err := net.InterfaceByName(device)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_CAN, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.CAN_RAW)
	if err != nil {
		return nil, err
	}
	var file *os.File
	ok := false
	defer func() {
		if ok {
			return
		}
		if file != nil {
			_ = file.Close()
		} else {
			_ = unix.Close(fd)
		}
	}()
	for _, opt := range []struct{ level, name, value int }{
		{unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1},
		{unix.SOL_SOCKET, unix.SO_RXQ_OVFL, 1},
		{unix.SOL_CAN_RAW, unix.CAN_RAW_ERR_FILTER, unix.CAN_ERR_MASK},
	} {
		if err := unix.SetsockoptInt(fd, opt.level, opt.name, opt.value); err != nil {
			return nil, err
		}
	}
	// Only the controller's standard frames (0x7E0-0x7EF) are of interest. The
	// kernel drops everything else, so other devices on the bus cost no wake-ups.
	filter := []unix.CanFilter{{Id: frameStatus1, Mask: 0x7F0 | unix.CAN_EFF_FLAG | unix.CAN_RTR_FLAG}}
	if err := unix.SetsockoptCanRawFilter(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_FILTER, filter); err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrCAN{Ifindex: iface.Index}); err != nil {
		return nil, err
	}
	file = os.NewFile(uintptr(fd), device)
	rc, err := file.SyscallConn()
	if err != nil {
		return nil, err
	}
	// Deadlines require the descriptor to be registered with the poller.
	if err := file.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	ok = true
	return newCANTransport(&timestampSocket{fd: fd, file: file, rc: rc}), nil
}

func (s *timestampSocket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd < 0 {
		return nil
	}
	err := s.file.Close()
	s.fd = -1
	return err
}

func (s *timestampSocket) Receive(ctx context.Context) (can.Frame, time.Time, string, error) {
	if err := ctx.Err(); err != nil {
		return can.Frame{}, time.Time{}, "", err
	}
	deadline, _ := ctx.Deadline()
	if err := s.file.SetReadDeadline(deadline); err != nil {
		return can.Frame{}, time.Time{}, "", errors.New("CAN socket closed")
	}
	// Cancellation interrupts the poller wait by moving the deadline into the
	// past. Wait for a callback that already started so it cannot touch the
	// deadline of the next call.
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = s.file.SetReadDeadline(time.Unix(1, 0))
		close(fired)
	})
	defer func() {
		if !stop() {
			<-fired
		}
	}()
	return s.receiveOne(ctx)
}

func (s *timestampSocket) receiveOne(ctx context.Context) (can.Frame, time.Time, string, error) {
	var data [16]byte
	var control [128]byte
	var n, oobn, flags int
	var recvErr error
	err := s.rc.Read(func(fd uintptr) bool {
		for {
			n, oobn, flags, _, recvErr = unix.Recvmsg(int(fd), data[:], control[:], unix.MSG_DONTWAIT)
			if recvErr == unix.EINTR {
				continue
			}
			return recvErr != unix.EAGAIN
		}
	})
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return can.Frame{}, time.Time{}, "", ctx.Err()
		case errors.Is(err, os.ErrDeadlineExceeded):
			return can.Frame{}, time.Time{}, "", context.DeadlineExceeded
		case errors.Is(err, os.ErrClosed):
			return can.Frame{}, time.Time{}, "", errors.New("CAN socket closed")
		}
		return can.Frame{}, time.Time{}, "", err
	}
	if recvErr != nil {
		return can.Frame{}, time.Time{}, "", recvErr
	}
	var frame can.Frame
	if n != len(data) || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || can.Unmarshal(data[:], &frame) != nil {
		return frame, time.Time{}, "malformed-frame", nil
	}
	if frame.ID&unix.CAN_ERR_FLAG != 0 {
		return frame, time.Time{}, "transport-error", nil
	}
	messages, err := unix.ParseSocketControlMessage(control[:oobn])
	if err != nil {
		return frame, time.Time{}, "missing-timestamp", nil
	}
	at, drops, reason := receiveMetadata(messages, time.Now(), s.drops)
	s.drops = drops
	return frame, at, reason, nil
}

func (s *timestampSocket) Send(ctx context.Context, frame can.Frame, allowed func() bool) error {
	data, err := can.Marshal(frame)
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		s.mu.RLock()
		fd := s.fd
		if fd < 0 {
			s.mu.RUnlock()
			return errCANObsolete
		}
		// Never hold the ECU state mutex during a socket operation.
		if !allowed() {
			s.mu.RUnlock()
			return errCANObsolete
		}
		n, err := unix.Write(fd, data)
		s.mu.RUnlock()
		if err == nil {
			if n != len(data) {
				return fmt.Errorf("short CAN write: %d", n)
			}
			return nil
		}
		if err != unix.EAGAIN && err != unix.ENOBUFS && err != unix.EINTR {
			return err
		}
		timer := time.NewTimer(2 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

// SO_TIMESTAMPNS carries native longs: 8-byte ARMv7 and 16-byte amd64 timespecs.
func kernelReceiveTime(data []byte, now time.Time) (time.Time, error) {
	var sec, nsec int64
	switch len(data) {
	case 8:
		sec, nsec = int64(int32(binary.NativeEndian.Uint32(data[:4]))), int64(int32(binary.NativeEndian.Uint32(data[4:])))
	case 16:
		sec, nsec = int64(binary.NativeEndian.Uint64(data[:8])), int64(binary.NativeEndian.Uint64(data[8:]))
	default:
		return time.Time{}, errors.New("invalid timestamp size")
	}
	if sec <= 0 || nsec < 0 || nsec >= int64(time.Second) {
		return time.Time{}, errors.New("invalid timestamp")
	}
	age := now.Sub(time.Unix(sec, nsec))
	if age < 0 || age > frameMaxAge {
		return time.Time{}, errors.New("timestamp outside freshness window")
	}
	return now.Add(-age), nil
}

func receiveMetadata(messages []unix.SocketControlMessage, now time.Time, drops uint32) (time.Time, uint32, string) {
	var at time.Time
	reason := ""
	for _, msg := range messages {
		if msg.Header.Level != unix.SOL_SOCKET {
			continue
		}
		switch msg.Header.Type {
		case unix.SCM_TIMESTAMPNS:
			var err error
			at, err = kernelReceiveTime(msg.Data, now)
			if err != nil {
				reason = "invalid-timestamp"
			}
		case unix.SO_RXQ_OVFL:
			if len(msg.Data) != 4 {
				reason = "rx-overflow"
				continue
			}
			next := binary.NativeEndian.Uint32(msg.Data)
			if next != drops {
				reason = "rx-overflow"
				drops = next
			}
		}
	}
	if at.IsZero() {
		reason = "missing-timestamp"
	}
	return at, drops, reason
}
