package main

import (
	"time"

	"github.com/brutella/can"
)

type pendingTX struct {
	generation, revision uint64
	purpose              string
	heard                bool
	done                 chan txResult
}

// Completions are consumed under the state lock; the TX worker never needs it
// to report a result and never holds it while writing to the socket.
func (b *ECU) drainTXLocked() {
	pending := b.pendingTX[:0]
	for _, p := range b.pendingTX {
		select {
		case result := <-p.done:
			if p.generation != b.txGeneration || (p.purpose == "control" && p.revision != b.controlRevision) {
				continue
			}
			if result.err != nil {
				b.log.Debug("CAN %s not sent: %v", p.purpose, result.err)
				continue
			}
			switch p.purpose {
			case "control":
				b.stateAssertedToECU = true
				// This records a completed assertion to a reachable ECU. Actual KERS/
				// boost values remain the separately decoded controller status fields.
				b.stateAckedByECU = p.heard
			case "startup gears":
				b.gearsSentOnPower = true
			}
			b.log.Debug("CAN %s batch sent", p.purpose)
		default:
			pending = append(pending, p)
		}
	}
	b.pendingTX = pending
}

func (b *ECU) pendingLocked(purpose string) bool {
	for _, p := range b.pendingTX {
		if p.purpose == purpose && p.generation == b.txGeneration && (purpose != "control" || p.revision == b.controlRevision) {
			return true
		}
	}
	return false
}

func (b *ECU) submitLocked(frames []can.Frame, purpose string, heard bool) error {
	b.drainTXLocked()
	if b.bus == nil || !b.mayTransmitLocked() {
		return nil
	}
	if len(b.pendingTX) >= canQueueSize {
		return errCANQueueFull
	}
	p := &pendingTX{generation: b.txGeneration, revision: b.controlRevision, purpose: purpose, heard: heard, done: make(chan txResult, 1)}
	r := txRequest{frames: frames, expires: time.Now().Add(canTXTimeout), done: p.done}
	r.allowed = func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return commandCurrent(r.expires, time.Now()) && b.txGeneration == p.generation && b.mayTransmitLocked() && (purpose != "control" || b.controlRevision == p.revision)
	}
	if err := b.bus.Submit(r); err != nil {
		return err
	}
	b.pendingTX = append(b.pendingTX, p)
	b.drainTXLocked()
	return nil
}

func (b *ECU) motionState(at time.Time) (bool, uint64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.powerCmd != powerOff && (b.powerCmd != powerOn || !at.Before(b.powerOnAt)), b.txGeneration
}

func (b *ECU) changedControlLocked() {
	b.controlRevision++
	b.stateAssertedToECU, b.stateAckedByECU = false, false
}
