package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	ipc "github.com/librescoot/redis-ipc"
	"github.com/redis/go-redis/v9"
)

// newTestTx wires an IPCTx against an in-process Redis.
func newTestTx(t *testing.T) (*IPCTx, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	host, port, err := net.SplitHostPort(mr.Addr())
	if err != nil {
		t.Fatalf("split miniredis addr: %v", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse miniredis port: %v", err)
	}

	client, err := ipc.New(ipc.WithAddress(host), ipc.WithPort(p))
	if err != nil {
		t.Fatalf("ipc.New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return newIPCTx(context.Background(), client, newLogger(LogLevelNone)), mr
}

// The five hash-change notifications used to go out on channels literally named
// "engine-ecu <field>", which nothing subscribes to. Channel is the hash name,
// message is the field name.
func TestPublishNotificationsUseHashChannelAndFieldMessage(t *testing.T) {
	tx, mr := newTestTx(t)

	sub := redis.NewClient(&redis.Options{Addr: mr.Addr()}).Subscribe(context.Background(), ecuChannel)
	t.Cleanup(func() { sub.Close() })
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"throttle", tx.PublishThrottle, "throttle"},
		{"odometer", tx.PublishOdometer, "odometer"},
		{"kers", tx.PublishKERS, "kers"},
		{"kers-reason-off", tx.PublishKERSReasonOff, "kers-reason-off"},
		{"regen-available", tx.PublishRegen, "regen-available"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); err != nil {
				t.Fatalf("publish: %v", err)
			}
			msg := receive(t, sub)
			if msg.Channel != ecuChannel {
				t.Errorf("channel = %q, want %q", msg.Channel, ecuChannel)
			}
			if msg.Payload != c.want {
				t.Errorf("payload = %q, want %q", msg.Payload, c.want)
			}
		})
	}
}

func TestSendStatusDoesNotPublishInvalidOdometer(t *testing.T) {
	tx, mr := newTestTx(t)

	if err := tx.SendStatus(Status{}); err != nil {
		t.Fatalf("send unrelated status: %v", err)
	}
	keys, err := mr.HKeys(ecuHashKey)
	if err != nil {
		t.Fatalf("read status hash: %v", err)
	}
	for _, key := range keys {
		if key == "odometer" {
			t.Fatal("first status published a fake zero odometer")
		}
	}

	if err := tx.SendStatus(Status{Odometer: 0, OdometerValid: true}); err != nil {
		t.Fatalf("send valid zero: %v", err)
	}
	got := mr.HGet(ecuHashKey, "odometer")
	if got != "0" {
		t.Errorf("odometer = %q, want 0", got)
	}
}

func TestMissingCacheClearsStaleRedisOdometer(t *testing.T) {
	tx, mr := newTestTx(t)
	mr.HSet(ecuHashKey, "odometer", "123")
	a := &App{
		opts:  Options{OdometerFile: filepath.Join(t.TempDir(), "missing.json")},
		log:   newLogger(LogLevelNone),
		ipcTx: tx,
	}

	if err := a.loadCachedOdometer(); err != nil {
		t.Fatal(err)
	}

	if got := mr.HGet(ecuHashKey, "odometer"); got != "" {
		t.Fatalf("missing durable cache left stale Redis odometer %q", got)
	}
	if a.odometerValid {
		t.Fatal("missing durable cache established odometer validity")
	}
}

func TestCorruptCacheClearsStaleRedisOdometer(t *testing.T) {
	tx, mr := newTestTx(t)
	mr.HSet(ecuHashKey, "odometer", "123")
	path := filepath.Join(t.TempDir(), "odometer.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &App{
		opts:  Options{OdometerFile: path},
		log:   newLogger(LogLevelNone),
		ipcTx: tx,
	}

	if err := a.loadCachedOdometer(); err != nil {
		t.Fatal(err)
	}

	if got := mr.HGet(ecuHashKey, "odometer"); got != "" {
		t.Fatalf("corrupt durable cache left stale Redis odometer %q", got)
	}
}

func TestLoadCachedOdometerPublishesZero(t *testing.T) {
	tx, mr := newTestTx(t)
	path := filepath.Join(t.TempDir(), "odometer.json")
	if err := saveOdometer(path, 0); err != nil {
		t.Fatal(err)
	}
	a := &App{
		opts:  Options{OdometerFile: path},
		log:   newLogger(LogLevelNone),
		ipcTx: tx,
	}

	if err := a.loadCachedOdometer(); err != nil {
		t.Fatal(err)
	}

	if !a.odometerValid || a.odometer != 0 {
		t.Fatalf("cached odometer = (%d, %v), want (0, true)", a.odometer, a.odometerValid)
	}
	got := mr.HGet(ecuHashKey, "odometer")
	if got != "0" {
		t.Errorf("cached odometer = %q, want 0", got)
	}
	if a.lastLiveOdometerOK {
		t.Error("cache publication was treated as a live Status3")
	}
}

func TestReportFaultRaiseThenClear(t *testing.T) {
	tx, mr := newTestTx(t)

	cfg := faultConfigs[FaultOverTemperature]
	if err := tx.ReportFault(FaultOverTemperature, cfg); err != nil {
		t.Fatalf("raise: %v", err)
	}
	if got := faultSet(t, mr); len(got) != 1 || got[0] != "11" {
		t.Fatalf("fault set after raise = %v, want [11]", got)
	}

	if err := tx.ReportFault(FaultNone, FaultConfig{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := faultSet(t, mr); len(got) != 0 {
		t.Fatalf("fault set after clear = %v, want empty", got)
	}

	// The clear has to name the fault it clears. A bare code 0 is dropped by
	// consumers that key on the absolute code, so it never enters fault history.
	codes := streamCodes(t, mr)
	want := []string{"11", "-11"}
	if len(codes) != len(want) {
		t.Fatalf("stream codes = %v, want %v", codes, want)
	}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("stream codes = %v, want %v", codes, want)
		}
	}
}

// An A -> B transition has to retire A, or the set carries both codes until
// something clears it wholesale.
func TestReportFaultTransitionRetiresPrevious(t *testing.T) {
	tx, mr := newTestTx(t)

	if err := tx.ReportFault(FaultOverTemperature, faultConfigs[FaultOverTemperature]); err != nil {
		t.Fatalf("raise A: %v", err)
	}
	if err := tx.ReportFault(FaultMotorStalled, faultConfigs[FaultMotorStalled]); err != nil {
		t.Fatalf("raise B: %v", err)
	}

	if got := faultSet(t, mr); len(got) != 1 || got[0] != "4" {
		t.Fatalf("fault set = %v, want [4]", got)
	}

	codes := streamCodes(t, mr)
	want := []string{"11", "-11", "4"}
	if len(codes) != len(want) {
		t.Fatalf("stream codes = %v, want %v", codes, want)
	}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("stream codes = %v, want %v", codes, want)
		}
	}
}

// An unrecognised code is reported under its own raw number, which can be large
// enough to wrap if the clear negates it as an int32.
func TestReportFaultClearOfLargeUnknownCodeDoesNotWrap(t *testing.T) {
	tx, mr := newTestTx(t)

	const raw = 0x80000001
	fault, cfg := MapFault(raw)
	if err := tx.ReportFault(fault, cfg); err != nil {
		t.Fatalf("raise: %v", err)
	}
	if err := tx.ReportFault(FaultNone, FaultConfig{}); err != nil {
		t.Fatalf("clear: %v", err)
	}

	codes := streamCodes(t, mr)
	if len(codes) != 2 {
		t.Fatalf("stream codes = %v, want 2 entries", codes)
	}
	if codes[1] != "-2147483649" {
		t.Errorf("clear code = %q, want %q", codes[1], "-2147483649")
	}
}

// A clear with nothing raised sweeps the set without emitting a clear event.
func TestReportFaultClearWithoutRaiseEmitsNoEvent(t *testing.T) {
	tx, mr := newTestTx(t)

	mr.SAdd(faultSetKey, "7")
	if err := tx.ReportFault(FaultNone, FaultConfig{}); err != nil {
		t.Fatalf("clear: %v", err)
	}

	if got := faultSet(t, mr); len(got) != 0 {
		t.Errorf("fault set = %v, want empty (stale entries swept)", got)
	}
	if codes := streamCodes(t, mr); len(codes) != 0 {
		t.Errorf("stream codes = %v, want none", codes)
	}
}

func TestSendStatusFailureDoesNotSuppressRetry(t *testing.T) {
	tx, mr := newTestTx(t)
	initial := Status{Odometer: 100, OdometerValid: true}
	if err := tx.SendStatus(initial); err != nil {
		t.Fatal(err)
	}

	mr.Close()
	changed := Status{Odometer: 200, OdometerValid: true}
	if err := tx.SendStatus(changed); err == nil {
		t.Fatal("SendStatus succeeded after Redis stopped")
	}
	if tx.last.Odometer != initial.Odometer {
		t.Fatalf("failed write advanced last odometer to %d", tx.last.Odometer)
	}
}

func TestPublishOdometerReportsRedisFailure(t *testing.T) {
	tx, mr := newTestTx(t)
	mr.Close()
	if err := tx.PublishOdometer(); err == nil {
		t.Fatal("synchronous odometer notification hid Redis failure")
	}
}

func receive(t *testing.T, sub *redis.PubSub) *redis.Message {
	t.Helper()
	select {
	case msg := <-sub.Channel():
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification")
		return nil
	}
}

func faultSet(t *testing.T, mr *miniredis.Miniredis) []string {
	t.Helper()
	members, err := mr.Members(faultSetKey)
	if err != nil {
		if err == miniredis.ErrKeyNotFound {
			return nil
		}
		t.Fatalf("read fault set: %v", err)
	}
	return members
}

func streamCodes(t *testing.T, mr *miniredis.Miniredis) []string {
	t.Helper()
	entries, err := mr.Stream(faultStreamKey)
	if err != nil {
		if err == miniredis.ErrKeyNotFound {
			return nil
		}
		t.Fatalf("read fault stream: %v", err)
	}
	codes := make([]string, 0, len(entries))
	for _, e := range entries {
		for i := 0; i+1 < len(e.Values); i += 2 {
			if e.Values[i] == "code" {
				codes = append(codes, e.Values[i+1])
			}
		}
	}
	return codes
}
