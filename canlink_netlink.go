package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// The kernel's CAN state and counters are read straight from rtnetlink rather
// than by execing `ip`: one small RTM_GETLINK round-trip per second is cheaper
// than a fork every second on the i.MX6, the netlink UAPI is stable and
// machine-readable, and `ip` output differs between busybox and iproute2
// builds while adding a PATH/binary dependency to the image.

const (
	// canLinkRecvTimeout bounds Recvfrom so a netlink reply that never arrives
	// surfaces as an error instead of parking the watcher goroutine forever.
	canLinkRecvTimeout = 2 * time.Second
	// canLinkBufLen comfortably exceeds one RTM_NEWLINK message for a device
	// without qdiscs/vlans attached (can0's is ~2KB).
	canLinkBufLen = 8192
)

// rtNetlinkCanLink samples a CAN link via RTM_GETLINK. One goroutine owns the
// socket and seq counter.
type rtNetlinkCanLink struct {
	ifname  string
	ifindex int
	fd      int
	seq     uint32
}

func newRTNetlinkCanLink(ifname string) (*rtNetlinkCanLink, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	tv := unix.NsecToTimeval(canLinkRecvTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("netlink recv timeout: %w", err)
	}
	ifindex, err := interfaceIndex(ifname)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &rtNetlinkCanLink{ifname: ifname, ifindex: ifindex, fd: fd}, nil
}

func interfaceIndex(name string) (int, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("ioctl socket: %w", err)
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return 0, err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFINDEX, ifr); err != nil {
		return 0, fmt.Errorf("SIOCGIFINDEX %s: %w", name, err)
	}
	return int(ifr.Uint32()), nil
}

func (l *rtNetlinkCanLink) Read() (canLinkSample, error) {
	l.seq++
	// NlMsghdr followed by Ifinfomsg, native-endian like all netlink fields.
	req := make([]byte, unix.SizeofNlMsghdr+unix.SizeofIfInfomsg)
	e := binary.NativeEndian
	e.PutUint32(req[0:4], uint32(len(req)))
	e.PutUint16(req[4:6], unix.RTM_GETLINK)
	e.PutUint16(req[6:8], unix.NLM_F_REQUEST)
	e.PutUint32(req[8:12], l.seq)
	// req[12:20] stays zero: nlmsg pid, ifinfomsg family AF_UNSPEC, pad, type.
	e.PutUint32(req[20:24], uint32(l.ifindex))

	if err := unix.Sendto(l.fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return canLinkSample{}, fmt.Errorf("RTM_GETLINK send: %w", err)
	}
	buf := make([]byte, canLinkBufLen)
	n, _, err := unix.Recvfrom(l.fd, buf, 0)
	if err != nil {
		return canLinkSample{}, fmt.Errorf("RTM_GETLINK recv: %w", err)
	}
	return parseCANLinkReply(buf[:n], l.seq, l.ifname)
}

func (l *rtNetlinkCanLink) Close() {
	unix.Close(l.fd)
}

// parseCANLinkReply decodes one netlink reply message (header + payload).
func parseCANLinkReply(b []byte, seq uint32, ifname string) (canLinkSample, error) {
	if len(b) < unix.SizeofNlMsghdr {
		return canLinkSample{}, fmt.Errorf("netlink reply too short (%d bytes)", len(b))
	}
	e := binary.NativeEndian
	msgLen := int(e.Uint32(b[0:4]))
	if msgLen < unix.SizeofNlMsghdr || msgLen > len(b) {
		return canLinkSample{}, fmt.Errorf("netlink reply has bad length %d (buffer %d)", msgLen, len(b))
	}
	if got := e.Uint32(b[8:12]); got != seq {
		return canLinkSample{}, fmt.Errorf("netlink reply seq %d, want %d", got, seq)
	}
	payload := b[unix.SizeofNlMsghdr:msgLen]
	switch e.Uint16(b[4:6]) {
	case unix.NLMSG_ERROR:
		if len(payload) < 4 {
			return canLinkSample{}, fmt.Errorf("short NLMSG_ERROR reply")
		}
		if errno := int(int32(e.Uint32(payload[0:4]))); errno != 0 {
			return canLinkSample{}, fmt.Errorf("RTM_GETLINK %s: %w", ifname, unix.Errno(-errno))
		}
		return canLinkSample{}, fmt.Errorf("RTM_GETLINK %s: empty reply", ifname)
	case unix.RTM_NEWLINK:
		return parseCANLinkSample(payload, ifname)
	default:
		return canLinkSample{}, fmt.Errorf("unexpected netlink message type %d", e.Uint16(b[4:6]))
	}
}

// parseCANLinkSample decodes ifinfomsg plus the nested attributes a CAN link
// carries: IFLA_LINKINFO > {KIND, DATA{IFLA_CAN_*}, XSTATS} and IFLA_STATS64.
func parseCANLinkSample(payload []byte, ifname string) (canLinkSample, error) {
	if len(payload) < unix.SizeofIfInfomsg {
		return canLinkSample{}, fmt.Errorf("short ifinfomsg (%d bytes)", len(payload))
	}
	e := binary.NativeEndian
	var (
		s        canLinkSample
		hasState bool
		kind     string
	)
	walkRtAttrs(payload[unix.SizeofIfInfomsg:], func(atype uint16, v []byte) bool {
		switch atype {
		case unix.IFLA_LINKINFO:
			walkRtAttrs(v, func(atype uint16, v []byte) bool {
				switch atype {
				case unix.IFLA_INFO_KIND:
					kind = string(bytes.TrimRight(v, "\x00"))
				case unix.IFLA_INFO_DATA:
					walkRtAttrs(v, func(atype uint16, v []byte) bool {
						if atype == unix.IFLA_CAN_STATE {
							if u, ok := fixedUint(v); ok {
								s.State = canStateName(u)
								hasState = true
							}
						}
						if atype == unix.IFLA_CAN_BERR_COUNTER && len(v) >= 4 {
							s.Berr = &canBerrCounter{
								Tx: e.Uint16(v[0:2]),
								Rx: e.Uint16(v[2:4]),
							}
						}
						return true
					})
				case unix.IFLA_INFO_XSTATS:
					// struct can_device_stats: six u32 in uapi order.
					if len(v) >= 24 {
						s.Stats = &canDeviceStats{
							BusError:        e.Uint32(v[0:4]),
							ErrorWarning:    e.Uint32(v[4:8]),
							ErrorPassive:    e.Uint32(v[8:12]),
							BusOff:          e.Uint32(v[12:16]),
							ArbitrationLost: e.Uint32(v[16:20]),
							Restarts:        e.Uint32(v[20:24]),
						}
					}
				}
				return true
			})
		case unix.IFLA_STATS64:
			// struct rtnl_link_stats64; the fields wanted here sit at
			// indices rx_errors=4, tx_errors=5, rx_dropped=7, tx_dropped=8.
			if len(v) >= 9*8 {
				s.RxErrors = e.Uint64(v[4*8 : 5*8])
				s.TxErrors = e.Uint64(v[5*8 : 6*8])
				s.RxDropped = e.Uint64(v[7*8 : 8*8])
				s.TxDropped = e.Uint64(v[8*8 : 9*8])
			}
		}
		return true
	})
	if kind != "" && kind != "can" {
		return canLinkSample{}, fmt.Errorf("%s is a %q link, not CAN", ifname, kind)
	}
	if !hasState {
		return canLinkSample{}, fmt.Errorf("no CAN state attribute on %s", ifname)
	}
	return s, nil
}

// walkRtAttrs iterates rtattr records (also the layout of nested rtnl
// attributes), calling fn with each attribute's masked type and payload.
func walkRtAttrs(b []byte, fn func(atype uint16, payload []byte) bool) {
	const typeMask = 0x3fff // strip NLA_F_NESTED / NLA_F_NET_BYTEORDER
	for len(b) >= unix.SizeofRtAttr {
		al := int(binary.NativeEndian.Uint16(b[0:2]))
		if al < unix.SizeofRtAttr || al > len(b) {
			return
		}
		if !fn(binary.NativeEndian.Uint16(b[2:4])&typeMask, b[unix.SizeofRtAttr:al]) {
			return
		}
		b = b[(al+3)&^3:] // RTA_ALIGN
	}
}

// fixedUint reads a scalar attribute payload of 1 or 4 bytes: IFLA_CAN_STATE
// is a u8 today, and a u32 payload must not be silently truncated if that
// ever changes.
func fixedUint(v []byte) (uint32, bool) {
	switch len(v) {
	case 1:
		return uint32(v[0]), true
	case 4:
		return binary.NativeEndian.Uint32(v), true
	default:
		return 0, false
	}
}
