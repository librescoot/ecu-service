package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/brutella/can"
	"golang.org/x/sys/unix"
)

type timestampSocket struct {
	mu    sync.RWMutex
	fd    int
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
	ok := false
	defer func() {
		if !ok {
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
	if err := unix.Bind(fd, &unix.SockaddrCAN{Ifindex: iface.Index}); err != nil {
		return nil, err
	}
	ok = true
	return newCANTransport(&timestampSocket{fd: fd}), nil
}

func (s *timestampSocket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd < 0 {
		return nil
	}
	err := unix.Close(s.fd)
	s.fd = -1
	return err
}

func (s *timestampSocket) Receive(ctx context.Context) (can.Frame, time.Time, string, error) {
	var data [16]byte
	var control [128]byte
	for ctx.Err() == nil {
		s.mu.RLock()
		fd := s.fd
		if fd < 0 {
			s.mu.RUnlock()
			return can.Frame{}, time.Time{}, "", errors.New("CAN socket closed")
		}
		n, oobn, flags, _, err := unix.Recvmsg(fd, data[:], control[:], unix.MSG_DONTWAIT)
		s.mu.RUnlock()
		if err == unix.EAGAIN || err == unix.EINTR {
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			_, err = unix.Poll(poll, 20)
			if err != nil && err != unix.EINTR {
				return can.Frame{}, time.Time{}, "", err
			}
			if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				return can.Frame{}, time.Time{}, "", fmt.Errorf("CAN socket unavailable: poll=%#x", poll[0].Revents)
			}
			continue
		}
		if err != nil {
			return can.Frame{}, time.Time{}, "", err
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
	return can.Frame{}, time.Time{}, "", ctx.Err()
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
	if age < 0 || age > motionEvidenceMaxAge {
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
