package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// canLinkTick is the sampling period. It must stay below the watchdog's
// commLostRaiseAfter: a bus-off episode long enough to starve frames into E20
// holds the BUS-OFF state for roughly that long, and a slower grid could step
// over the episode entirely. One netlink round-trip per second is negligible
// even on the i.MX6.
const canLinkTick = time.Second

// canState is the kernel's CAN controller state (enum can_state), spelled the
// way iproute2 prints it so log lines read the same as `ip -d link show can0`.
type canState string

const (
	canStateErrorActive  canState = "ERROR-ACTIVE"
	canStateErrorWarning canState = "ERROR-WARNING"
	canStateErrorPassive canState = "ERROR-PASSIVE"
	canStateBusOff       canState = "BUS-OFF"
	canStateStopped      canState = "STOPPED"
	canStateSleeping     canState = "SLEEPING"
)

func canStateName(v uint32) canState {
	switch v {
	case 0:
		return canStateErrorActive
	case 1:
		return canStateErrorWarning
	case 2:
		return canStateErrorPassive
	case 3:
		return canStateBusOff
	case 4:
		return canStateStopped
	case 5:
		return canStateSleeping
	default:
		return canState(fmt.Sprintf("STATE-%d", v))
	}
}

// canDeviceStats mirrors struct can_device_stats, reported as
// IFLA_INFO_XSTATS on a CAN link. The counters are cumulative since the
// netdev was registered.
type canDeviceStats struct {
	BusError        uint32
	ErrorWarning    uint32
	ErrorPassive    uint32
	BusOff          uint32
	ArbitrationLost uint32
	Restarts        uint32
}

// canBerrCounter mirrors struct can_berr_counter: the controller's TX/RX error
// counters at the time of sampling (TEC/REC).
type canBerrCounter struct {
	Tx uint16
	Rx uint16
}

// canLinkSample is one reading of a CAN link's state and counters. Stats and
// Berr are nil when the kernel did not report them (older kernel, or a driver
// without the callbacks), so a transition line never claims zeros that were
// never measured. Rx/Tx fields come from the generic rtnl stats64 attribute.
type canLinkSample struct {
	State     canState
	Stats     *canDeviceStats
	Berr      *canBerrCounter
	RxErrors  uint64
	TxErrors  uint64
	RxDropped uint64
	TxDropped uint64
}

func (s canLinkSample) counters() string {
	parts := make([]string, 0, 12)
	if s.Stats != nil {
		parts = append(parts,
			fmt.Sprintf("bus-error=%d", s.Stats.BusError),
			fmt.Sprintf("err-warn=%d", s.Stats.ErrorWarning),
			fmt.Sprintf("err-passive=%d", s.Stats.ErrorPassive),
			fmt.Sprintf("bus-off=%d", s.Stats.BusOff),
			fmt.Sprintf("arbit-lost=%d", s.Stats.ArbitrationLost),
			fmt.Sprintf("restarts=%d", s.Stats.Restarts),
		)
	}
	parts = append(parts,
		fmt.Sprintf("rx-err=%d", s.RxErrors),
		fmt.Sprintf("tx-err=%d", s.TxErrors),
		fmt.Sprintf("rx-drop=%d", s.RxDropped),
		fmt.Sprintf("tx-drop=%d", s.TxDropped),
	)
	if s.Berr != nil {
		parts = append(parts,
			fmt.Sprintf("berr-tx=%d", s.Berr.Tx),
			fmt.Sprintf("berr-rx=%d", s.Berr.Rx),
		)
	}
	return strings.Join(parts, " ")
}

// canLinkReader supplies CAN link samples. The production implementation reads
// rtnetlink (canlink_netlink.go); tests substitute a fake.
type canLinkReader interface {
	Read() (canLinkSample, error)
	Close()
}

// CanLinkWatcher samples a CAN link's state and error counters and logs state
// transitions. Its purpose is attribution: an ECU that stops talking and a
// bus-off latch on the link both starve frames and raise E20, so a field log
// needs the link's state history to tell the two apart.
type CanLinkWatcher struct {
	src    canLinkReader
	ifname string
	log    *Logger

	havePrev bool
	prev     canLinkSample
}

func newCanLinkWatcher(src canLinkReader, ifname string, log *Logger) *CanLinkWatcher {
	return &CanLinkWatcher{src: src, ifname: ifname, log: log}
}

func (w *CanLinkWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(canLinkTick)
	defer ticker.Stop()
	defer w.src.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.check()
		}
	}
}

func (w *CanLinkWatcher) check() {
	s, err := w.src.Read()
	if err != nil {
		w.log.Debug("can link watcher: %s: %v", w.ifname, err)
		return
	}
	w.observe(s)
}

// observe edges the log lines: a WARN on entering BUS-OFF with the prior
// state, an INFO on every other transition, and the full counter summary only
// at debug so a stable link writes nothing to the journal at default level.
// The check runs at 1Hz and a latched state persists, so logging conditions
// rather than transitions would repeat the same line every second.
func (w *CanLinkWatcher) observe(s canLinkSample) {
	if !w.havePrev {
		w.havePrev = true
		w.prev = s
		if s.State == canStateBusOff {
			w.log.Warn("CAN link %s: found %s at first sample, %s", w.ifname, s.State, s.counters())
		} else {
			w.log.Info("CAN link %s: %s, %s", w.ifname, s.State, s.counters())
		}
		return
	}
	if s.State != w.prev.State {
		if s.State == canStateBusOff {
			w.log.Warn("CAN link %s: %s -> BUS-OFF, %s", w.ifname, w.prev.State, s.counters())
		} else {
			w.log.Info("CAN link %s: %s -> %s, %s", w.ifname, w.prev.State, s.State, s.counters())
		}
	}
	w.log.Debug("CAN link %s: %s, %s", w.ifname, s.State, s.counters())
	w.prev = s
}
