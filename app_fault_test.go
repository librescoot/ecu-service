package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCommLossRequiresFreshFaultReport(t *testing.T) {
	for _, committed := range []bool{false, true} {
		for _, freshBeforeRecovery := range []bool{false, true} {
			for _, code := range []byte{0, 1} {
				t.Run(fmt.Sprintf("committed=%t/freshBeforeRecovery=%t/code=%d", committed, freshBeforeRecovery, code), func(t *testing.T) {
					tx, mr := newTestTx(t)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					a := &App{log: newLogger(LogLevelNone), ecu: newTestECU(), ipcTx: tx}
					a.kers = newKERSControllerWithDelay(ctx, time.Millisecond, func(bool) {}, func(KERSReason) {})
					changes := make(chan Fault, 10)
					a.diag = newDiagnosticsWithDurations(ctx, a.log, 20*time.Millisecond, 20*time.Millisecond, func(f Fault, cfg FaultConfig) {
						if err := tx.ReportFault(f, cfg); err != nil {
							t.Errorf("ReportFault: %v", err)
						}
						changes <- f
					})
					h := (*appHandler)(a)
					status2 := func(code byte) { h.Handle(makeFrame(frameStatus2, []byte{20, 0, 0, 0, 0, code})) }
					waitFault := func() {
						t.Helper()
						select {
						case f := <-changes:
							if f != FaultBatteryOverVoltage {
								t.Fatalf("fault = %d, want E1", f)
							}
						case <-time.After(time.Second):
							t.Fatal("fresh E1 not committed")
						}
					}
					noFault := func() {
						t.Helper()
						select {
						case f := <-changes:
							t.Fatalf("unexpected diagnostic publication: %d", f)
						case <-time.After(50 * time.Millisecond):
						}
					}
					status2(1)
					if committed {
						waitFault()
					}
					a.onCommLostChange(true)
					h.Handle(makeFrame(frameStatus1, make([]byte, 8)))
					noFault()
					if got := mr.HGet(ecuHashKey, "fault:code"); got != "20" {
						t.Fatalf("during loss: %s, want E20", got)
					}
					if present, err := mr.SIsMember(faultSetKey, "20"); err != nil || !present {
						t.Fatal("E20 missing")
					}
					if freshBeforeRecovery {
						status2(code)
					}
					a.onCommLostChange(false)
					if !freshBeforeRecovery {
						h.Handle(makeFrame(frameStatus1, make([]byte, 8)))
						noFault()
						if got := mr.HGet(ecuHashKey, "fault:code"); got != "0" {
							t.Fatalf("stale fault restored: %s", got)
						}
						if mr.Exists(faultSetKey) {
							t.Fatal("stale fault event restored")
						}
						status2(code)
					}
					if code == 1 {
						waitFault()
						if present, err := mr.SIsMember(faultSetKey, "1"); err != nil || !present {
							t.Fatal("fresh E1 missing")
						}
					} else {
						noFault()
						if mr.Exists(faultSetKey) {
							t.Fatal("fault present after healthy Status2")
						}
					}
				})
			}
		}
	}
}
