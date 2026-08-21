package main

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/brutella/can"
	ipc "github.com/librescoot/redis-ipc"
)

// fakeHash is an in-memory vehicleHashReader so CommLostWatcher.check() can be
// driven deterministically without Redis.
type fakeHash struct {
	fields map[string]string
}

func (f *fakeHash) HGetAll(key string) (map[string]string, error) {
	return f.fields, nil
}

// fakeBusRWC records frames published onto the bus (outgoing control frames such as
// the 0x4EF status request) so a unit test can observe RequestStatus() and, via
// can.NewBus, act as the socket behind ECU.bus.
type fakeBusRWC struct {
	mu   sync.Mutex
	sent []can.Frame
}

func (f *fakeBusRWC) WriteFrame(frame can.Frame) error {
	f.mu.Lock()
	f.sent = append(f.sent, frame)
	f.mu.Unlock()
	return nil
}

func (f *fakeBusRWC) sentIDs() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]uint32, len(f.sent))
	for i, fr := range f.sent {
		ids[i] = fr.ID
	}
	return ids
}

func (f *fakeBusRWC) ReadFrame(*can.Frame) error { return io.EOF }
func (f *fakeBusRWC) Read([]byte) (int, error)   { return 0, io.EOF }
func (f *fakeBusRWC) Write([]byte) (int, error)  { return 0, io.EOF }
func (f *fakeBusRWC) Close() error               { return nil }

func containsID(ids []uint32, want uint32) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestStandstillSilenceDoesNotRaiseE20 locks in the corrected understanding: the
// ECU pushes no data while the scooter is at standstill (normal), so a
// parked-but-powered ECU that is silent must NOT raise E20 (a false alarm). The
// 0x4EF poll must still fire at standstill so the feed keeps eliciting data, but the
// E20 fault is speed-gated. The driving-stall recovery (E20 raise + republish)
// is covered separately by the driving tests.
func TestStandstillSilenceDoesNotRaiseE20(t *testing.T) {
	hash := &fakeHash{fields: map[string]string{
		"state":        "ready-to-drive",
		"engine-power": "on",
		"main-power":   "on",
	}}
	bus := &fakeBusRWC{}
	ecu := newTestECU()
	ecu.bus = can.NewBus(bus)

	// Last real frame: scooter at standstill (speed 0). The ECU then goes
	// silent — it pushes nothing while stood.
	data := make([]byte, 8)
	ecu.HandleFrame(makeFrame(frameStatus1, data))
	if ecu.Speed() != 0 {
		t.Fatalf("precondition: standstill speed must be 0, got %d", ecu.Speed())
	}
	ecu.mu.Lock()
	ecu.lastFrameTime = time.Now().Add(-5 * time.Second) // silence >= raise threshold
	ecu.mu.Unlock()

	var raised bool
	w := newCommLostWatcher(hash, ecu, newLogger(LogLevelNone), func(raise bool) {
		raised = raise
	})
	// Pre-seed so the power-on grace window has already lapsed.
	w.prevEcuPowered = true
	w.powerOnEdge = time.Now().Add(-5 * time.Second)

	// Silence period at standstill while the watcher keeps polling.
	for range 5 {
		w.check()
	}

	// The 0x4EF poll is still issued at standstill so it keeps eliciting data.
	if !containsID(bus.sentIDs(), frameStatusReq) {
		t.Fatalf("RequestStatus (0x4EF) was never issued during the standstill silence")
	}

	// At standstill Speed()==0, so a silent-but-powered ECU is normal: E20 must NOT
	// be raised (no false alarm), and the watcher must not latch published state.
	if raised || w.published {
		t.Fatalf("standstill silence spuriously raised E20: Speed()==0 while parked-but-powered must not flag comm loss (raised=%v published=%v)",
			raised, w.published)
	}
}

// TestE20ClearResetsTrackingRepublishes locks in the second half of the fix: when E20
// clears after a comm stall, SendStatus change-tracking must be reset so the live values
// that were frozen behind the synthetic fault republish once frames resume — even when those
// values are unchanged from the pre-stall reading (publish-on-change alone would suppress them).
func TestE20ClearResetsTrackingRepublishes(t *testing.T) {
	m := miniredis.RunT(t)
	defer m.Close()

	host, portStr, err := net.SplitHostPort(m.Addr())
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", m.Addr(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}
	client, err := ipc.New(ipc.WithAddress(host), ipc.WithPort(port))
	if err != nil {
		t.Fatalf("ipc.New: %v", err)
	}
	defer client.Close()

	ctx := context.Background()
	log := newLogger(LogLevelNone)
	a := &App{
		ipcTx: newIPCTx(ctx, client, log),
		ecu:   newTestECU(),
		log:   log,
	}

	// Establish a SendStatus baseline (hasLast=true, all live fields written).
	base := Status{Speed: 20, Voltage: 11100, Current: 53, Power: 980}
	if err := a.ipcTx.SendStatus(base); err != nil {
		t.Fatalf("SendStatus baseline: %v", err)
	}
	if got := m.HGet(ecuHashKey, "speed"); got != "20" {
		t.Fatalf("baseline speed = %q, want 20", got)
	}

	// Comm stall: raise E20 then clear it — onCommLostChange(false) resets tracking.
	a.onCommLostChange(true)
	a.onCommLostChange(false)
	if got := m.HGet(ecuHashKey, "fault:code"); got != "0" {
		t.Fatalf("fault:code after E20 clear = %q, want 0 (ECU's real fault restored)", got)
	}

	// Drop a live field, then re-send the exact pre-stall values. If change-tracking
	// had been left intact this SendStatus would be a no-op and the field would stay gone.
	// ResetTracking must make it republish (the frozen feed returns to a live state).
	m.HDel(ecuHashKey, "speed")
	if err := a.ipcTx.SendStatus(base); err != nil {
		t.Fatalf("SendStatus resume: %v", err)
	}
	if got := m.HGet(ecuHashKey, "speed"); got != "20" {
		t.Fatalf("live speed value did not republish after E20 clear (got %q)", got)
	}
}

// TestDrivingStallFeedUnfreezesAfterRecovery is the end-to-end driving variant of the
// recovery invariant (Lane 2). It wires a real CommLostWatcher to App.onCommLostChange
// over miniredis and drives a MOVING ECU at ~5 Hz. When the socket dies mid-drive and the
// reconnect loop churns (UpdateBus on each cycle), the Lane-1 fix keeps staleness visible so
// E20 raises; when a frame finally arrives the E20 clears, ResetTracking drops the SendStatus
// baseline, and the next SendStatus republishes the live value into the consumer-facing Redis hash.
// This proves the consumer-visible feed unfreezes after a driving stall, not just at standstill.
func TestDrivingStallFeedUnfreezesAfterRecovery(t *testing.T) {
	m := miniredis.RunT(t)
	defer m.Close()

	host, portStr, err := net.SplitHostPort(m.Addr())
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", m.Addr(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}
	client, err := ipc.New(ipc.WithAddress(host), ipc.WithPort(port))
	if err != nil {
		t.Fatalf("ipc.New: %v", err)
	}
	defer client.Close()

	ctx := context.Background()
	log := newLogger(LogLevelNone)
	a := &App{
		ipcTx: newIPCTx(ctx, client, log),
		ecu:   newTestECU(),
		log:   log,
	}
	a.ecu.bus = can.NewBus(&fakeBusRWC{})

	hash := &fakeHash{fields: map[string]string{
		"state":        "ready-to-drive",
		"engine-power": "on",
		"main-power":   "on",
	}}
	w := newCommLostWatcher(hash, a.ecu, log, a.onCommLostChange)
	w.prevEcuPowered = true
	w.powerOnEdge = time.Now().Add(-commLostPowerOnGrace - time.Second) // grace lapsed (excludes H5)

	// Phase 1: MOVING at ~5 Hz — frames land, Speed()>0, live feed published.
	live := Status{Speed: 40, Voltage: 11100, Power: 980}
	data := make([]byte, 8)
	data[6] = 60 // raw speed -> calibrated >0
	for range 5 {
		a.ecu.HandleFrame(makeFrame(frameStatus1, data))
		if err := a.ipcTx.SendStatus(live); err != nil {
			t.Fatalf("SendStatus: %v", err)
		}
		time.Sleep(commLostTick)
	}
	if a.ecu.Speed() == 0 {
		t.Fatalf("precondition: driving speed must be >0, got %d", a.ecu.Speed())
	}
	if got := m.HGet(ecuHashKey, "speed"); got != "40" {
		t.Fatalf("baseline speed = %q, want 40", got)
	}

	// Phase 2: socket dies mid-drive. The reconnect loop rebuilds the bus each cycle;
	// with the Lane-1 fix UpdateBus does NOT rewrite the freshness clock, so the no-frame
	// interval stays visible and E20 raises. The feed is now frozen behind the fault.
	deadline := time.Now().Add(commLostRaiseAfter + 2*commLostTick)
	for time.Now().Before(deadline) {
		a.ecu.UpdateBus(can.NewBus(&fakeBusRWC{}))
		w.check()
		time.Sleep(commLostTick)
	}
	if !w.published {
		t.Fatalf("driving stall did not raise E20 after >%v silent; feed can never unfreeze", commLostRaiseAfter)
	}
	if got := m.HGet(ecuHashKey, "fault:code"); got != "20" {
		t.Fatalf("fault:code during stall = %q, want 20 (E20 raised mid-drive)", got)
	}

	// The live entry froze behind the fault; drop it so the republish-on-recovery is observable.
	m.HDel(ecuHashKey, "speed")

	// Phase 3: a fresh frame finally lands (ECU answers the 0x4EF poll). Staleness
	// clears -> E20 clears -> ResetTracking -> next SendStatus republishes the live value.
	a.ecu.HandleFrame(makeFrame(frameStatus1, data))
	w.check()
	if w.published {
		t.Fatalf("E20 not cleared after the ECU resumed sending frames")
	}
	if err := a.ipcTx.SendStatus(live); err != nil {
		t.Fatalf("SendStatus resume: %v", err)
	}
	if got := m.HGet(ecuHashKey, "speed"); got != "40" {
		t.Fatalf("consumer feed did NOT unfreeze after driving stall: speed = %q, want 40 (republish-on-recovery)", got)
	}
}
