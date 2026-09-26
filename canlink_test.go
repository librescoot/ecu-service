package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeCanLink replays scripted reads behind canLinkSource; the last entry
// repeats once the script runs out, so a latched state reads as a latched
// state does on a real link.
type fakeCanLink struct {
	reads  []fakeCanLinkRead
	n      int
	closed bool
}

type fakeCanLinkRead struct {
	sample canLinkSample
	err    error
}

func (f *fakeCanLink) Read() (canLinkSample, error) {
	r := f.reads[min(f.n, len(f.reads)-1)]
	f.n++
	return r.sample, r.err
}

func (f *fakeCanLink) Close() { f.closed = true }

// newTestCanLinkWatcher wires a watcher to scripted reads and a capturing
// logger at the given level.
func newTestCanLinkWatcher(level LogLevel, reads ...fakeCanLinkRead) (*CanLinkWatcher, *fakeCanLink, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	src := &fakeCanLink{reads: reads}
	w := newCanLinkWatcher(src, "can0", &Logger{l: log.New(buf, "", 0), level: level})
	return w, src, buf
}

func readState(s canState) fakeCanLinkRead { return fakeCanLinkRead{sample: canLinkSample{State: s}} }

func readErr(err error) fakeCanLinkRead { return fakeCanLinkRead{err: err} }

// TestCanLinkWatcher_BusOffWarnsOnceWithPriorState pins the primary
// requirement: entering BUS-OFF is a WARN naming the prior state, and a latched
// BUS-OFF does not repeat it every second.
func TestCanLinkWatcher_BusOffWarnsOnceWithPriorState(t *testing.T) {
	w, _, buf := newTestCanLinkWatcher(LogLevelInfo,
		readState(canStateErrorActive),
		readState(canStateBusOff),
		readState(canStateBusOff),
		readState(canStateBusOff),
	)
	for i := 0; i < 4; i++ {
		w.check()
	}

	if n := strings.Count(buf.String(), "[WARN] CAN link can0: ERROR-ACTIVE -> BUS-OFF"); n != 1 {
		t.Fatalf("bus-off edge logged %d times at WARN, want 1; log:\n%s", n, buf.String())
	}
	if w.prev.State != canStateBusOff {
		t.Errorf("prev state = %s, want BUS-OFF", w.prev.State)
	}
}

// TestCanLinkWatcher_RecoveryLogsInfo covers the other half of the episode:
// the kernel restarts the link after restart-ms, and that edge is what brackets
// the E20 window in a field log.
func TestCanLinkWatcher_RecoveryLogsInfo(t *testing.T) {
	w, _, buf := newTestCanLinkWatcher(LogLevelInfo,
		readState(canStateBusOff),
		readState(canStateErrorActive),
		readState(canStateErrorActive),
	)
	w.check() // first sample: started during bus-off
	w.check()
	w.check()

	if n := strings.Count(buf.String(), "[WARN] CAN link can0: found BUS-OFF at first sample"); n != 1 {
		t.Errorf("bus-off at first sample logged %d times at WARN, want 1", n)
	}
	if n := strings.Count(buf.String(), "[INFO] CAN link can0: BUS-OFF -> ERROR-ACTIVE"); n != 1 {
		t.Errorf("recovery logged %d times, want 1; log:\n%s", n, buf.String())
	}
}

// TestCanLinkWatcher_OtherStateChangeLogsInfoOnce covers non-bus-off
// transitions (the example from the field: ERROR-PASSIVE recovering to
// ERROR-ACTIVE): edge-triggered INFO, not one line per tick.
func TestCanLinkWatcher_OtherStateChangeLogsInfoOnce(t *testing.T) {
	w, _, buf := newTestCanLinkWatcher(LogLevelInfo,
		readState(canStateErrorPassive),
		readState(canStateErrorActive),
		readState(canStateErrorActive),
		readState(canStateErrorActive),
	)
	for i := 0; i < 4; i++ {
		w.check()
	}

	if n := strings.Count(buf.String(), "[INFO] CAN link can0: ERROR-PASSIVE -> ERROR-ACTIVE"); n != 1 {
		t.Fatalf("state change logged %d times at INFO, want 1; log:\n%s", n, buf.String())
	}
}

// TestCanLinkWatcher_QuietLinkStaysQuietAtInfo is the anti-spam requirement:
// at the default info level a stable link writes exactly one startup line, and
// the periodic full summary lands only at debug.
func TestCanLinkWatcher_QuietLinkStaysQuietAtInfo(t *testing.T) {
	script := make([]fakeCanLinkRead, 11)
	for i := range script {
		script[i] = readState(canStateErrorActive)
	}

	w, _, infoBuf := newTestCanLinkWatcher(LogLevelInfo, script...)
	for i := 0; i < 11; i++ {
		w.check()
	}
	if lines := strings.Count(strings.TrimRight(infoBuf.String(), "\n"), "\n") + 1; lines != 1 {
		t.Errorf("info-level log has %d lines after 11 quiet ticks, want 1:\n%s", lines, infoBuf.String())
	}

	w, _, debugBuf := newTestCanLinkWatcher(LogLevelDebug, script...)
	for i := 0; i < 11; i++ {
		w.check()
	}
	// First sample takes the startup branch; every later tick gets one debug
	// summary.
	if n := strings.Count(debugBuf.String(), "[DEBUG] CAN link can0:"); n != 10 {
		t.Errorf("debug summaries = %d, want 10; log:\n%s", n, debugBuf.String())
	}
}

// TestCanLinkWatcher_TransitionCarriesCounters requires the counters on the
// transition line: without them the line cannot show how far into trouble the
// link was when it latched.
func TestCanLinkWatcher_TransitionCarriesCounters(t *testing.T) {
	w, _, buf := newTestCanLinkWatcher(LogLevelInfo,
		fakeCanLinkRead{sample: canLinkSample{
			State: canStateErrorPassive,
			Stats: &canDeviceStats{BusError: 5, ErrorPassive: 2},
			Berr:  &canBerrCounter{Tx: 40, Rx: 12},
		}},
		fakeCanLinkRead{sample: canLinkSample{
			State:     canStateBusOff,
			Stats:     &canDeviceStats{BusError: 40, ErrorWarning: 3, ErrorPassive: 4, BusOff: 1, Restarts: 2},
			Berr:      &canBerrCounter{Tx: 255, Rx: 7},
			RxErrors:  1,
			TxErrors:  120,
			RxDropped: 0,
			TxDropped: 3,
		}},
	)
	w.check()
	w.check()

	line := buf.String()
	for _, want := range []string{
		"ERROR-PASSIVE -> BUS-OFF",
		"bus-error=40", "err-warn=3", "err-passive=4", "bus-off=1", "restarts=2",
		"rx-err=1", "tx-err=120", "rx-drop=0", "tx-drop=3",
		"berr-tx=255", "berr-rx=7",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("transition line missing %q:\n%s", want, line)
		}
	}
}

// TestCanLinkWatcher_ReadErrorKeepsEdges checks a failed netlink read is
// debug-only and does not reset the baseline: the bus-off edge after the
// failures still warns with the correct prior state.
func TestCanLinkWatcher_ReadErrorKeepsEdges(t *testing.T) {
	w, _, buf := newTestCanLinkWatcher(LogLevelInfo,
		readState(canStateErrorActive),
		readErr(unix.ETIMEDOUT),
		readErr(unix.ETIMEDOUT),
		readState(canStateBusOff),
	)
	for i := 0; i < 4; i++ {
		w.check()
	}

	if n := strings.Count(buf.String(), "[WARN] CAN link can0: ERROR-ACTIVE -> BUS-OFF"); n != 1 {
		t.Fatalf("bus-off edge after read errors logged %d times, want 1; log:\n%s", n, buf.String())
	}
	if strings.Contains(buf.String(), "[DEBUG]") {
		t.Errorf("error text leaked above debug level:\n%s", buf.String())
	}
}

// TestCanLinkWatcher_ClosesSource pins the goroutine cleanup contract: Run
// owns the reader and closes it on cancellation.
func TestCanLinkWatcher_ClosesSource(t *testing.T) {
	w, src, _ := newTestCanLinkWatcher(LogLevelNone, readState(canStateErrorActive))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	<-done
	if !src.closed {
		t.Error("Run returned without closing the link source")
	}
}

// rtAttr builds one rtattr record the way the kernel lays them out.
func rtAttr(atype uint16, payload []byte) []byte {
	al := (len(payload) + unix.SizeofRtAttr + 3) &^ 3
	b := make([]byte, al)
	binary.NativeEndian.PutUint16(b[0:2], uint16(unix.SizeofRtAttr+len(payload)))
	binary.NativeEndian.PutUint16(b[2:4], atype)
	copy(b[unix.SizeofRtAttr:], payload)
	return b
}

// newCANLinkReply frames attrs as a full RTM_NEWLINK reply for seq.
func newCANLinkReply(seq uint32, attrs ...[]byte) []byte {
	payload := make([]byte, unix.SizeofIfInfomsg)
	for _, a := range attrs {
		payload = append(payload, a...)
	}
	msg := make([]byte, unix.SizeofNlMsghdr+len(payload))
	e := binary.NativeEndian
	e.PutUint32(msg[0:4], uint32(len(msg)))
	e.PutUint16(msg[4:6], unix.RTM_NEWLINK)
	e.PutUint32(msg[8:12], seq)
	copy(msg[unix.SizeofNlMsghdr:], payload)
	return msg
}

func u32s(vals ...uint32) []byte {
	b := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.NativeEndian.PutUint32(b[i*4:], v)
	}
	return b
}

func u64s(vals ...uint64) []byte {
	b := make([]byte, 8*len(vals))
	for i, v := range vals {
		binary.NativeEndian.PutUint64(b[i*8:], v)
	}
	return b
}

// TestParseCANLinkReply_DecodesCANLink drives the parser with a hand-built
// RTM_NEWLINK reply shaped like flexcan's: can state as u8, xstats, berr
// counters, and stats64.
func TestParseCANLinkReply_DecodesCANLink(t *testing.T) {
	const seq = 7
	berr := make([]byte, 4)
	binary.NativeEndian.PutUint16(berr[0:2], 0xff) // txerr
	binary.NativeEndian.PutUint16(berr[2:4], 0x7)  // rxerr

	infoData := rtAttr(unix.IFLA_CAN_STATE, []byte{3}) // BUS-OFF
	infoData = append(infoData, rtAttr(unix.IFLA_CAN_BERR_COUNTER, berr)...)

	linkinfo := rtAttr(unix.IFLA_INFO_KIND, []byte("can\x00"))
	linkinfo = append(linkinfo, rtAttr(unix.IFLA_INFO_DATA, infoData)...)
	// struct can_device_stats: bus_error, error_warning, error_passive,
	// bus_off, arbitration_lost, restarts. XSTATS is a sibling of DATA.
	linkinfo = append(linkinfo, rtAttr(unix.IFLA_INFO_XSTATS, u32s(40, 3, 4, 1, 2, 5))...)

	// stats64: rx_errors at index 4, tx_errors at 5, rx_dropped at 7, tx_dropped at 8.
	s64 := make([]uint64, 9)
	s64[4], s64[5], s64[7], s64[8] = 1, 120, 0, 3

	msg := newCANLinkReply(seq,
		rtAttr(unix.IFLA_LINKINFO, linkinfo),
		rtAttr(unix.IFLA_STATS64, u64s(s64...)),
	)

	s, err := parseCANLinkReply(msg, seq, "can0")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.State != canStateBusOff {
		t.Errorf("state = %q, want BUS-OFF", s.State)
	}
	if s.Stats == nil || *s.Stats != (canDeviceStats{BusError: 40, ErrorWarning: 3, ErrorPassive: 4, BusOff: 1, ArbitrationLost: 2, Restarts: 5}) {
		t.Errorf("stats = %+v, want {40 3 4 1 2 5}", s.Stats)
	}
	if s.Berr == nil || s.Berr.Tx != 0xff || s.Berr.Rx != 0x7 {
		t.Errorf("berr = %+v, want tx=255 rx=7", s.Berr)
	}
	if s.RxErrors != 1 || s.TxErrors != 120 || s.RxDropped != 0 || s.TxDropped != 3 {
		t.Errorf("stats64 = rx-err %d tx-err %d rx-drop %d tx-drop %d, want 1/120/0/3",
			s.RxErrors, s.TxErrors, s.RxDropped, s.TxDropped)
	}
}

// TestParseCANLinkReply_RejectsNonCANLink guards the kind check: a renamed or
// unexpected interface must produce an error, not a sample with a bogus state.
func TestParseCANLinkReply_RejectsNonCANLink(t *testing.T) {
	const seq = 1
	linkinfo := rtAttr(unix.IFLA_INFO_KIND, []byte("vxcan\x00"))
	linkinfo = append(linkinfo, rtAttr(unix.IFLA_INFO_DATA,
		rtAttr(unix.IFLA_CAN_STATE, []byte{0}))...)
	msg := newCANLinkReply(seq, rtAttr(unix.IFLA_LINKINFO, linkinfo))

	if _, err := parseCANLinkReply(msg, seq, "can0"); err == nil {
		t.Fatal("a vxcan link was accepted as CAN")
	}
}
