package main

import (
	"context"
	"testing"
	"time"
)

func TestPowerOffInvalidatesBlockedSend(t *testing.T) {
	socket := newTestSocket()
	socket.block = make(chan struct{})
	tr := newCANTransport(socket)
	ecu := newECU(tr, newLogger(LogLevelNone), nil)
	ecu.SetPowered(true)
	ecu.RequestStatus()
	pending := ecu.pendingTX[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { tr.transmit(ctx); close(done) }()
	<-socket.entered
	ecu.SetPowered(false)
	close(socket.block)
	select {
	case result := <-pending.done:
		if result.err == nil {
			t.Fatal("power-off command sent")
		}
	case <-time.After(time.Second):
		t.Fatal("TX blocked")
	}
	if len(socket.sends) != 0 {
		t.Fatal("write occurred after power-off")
	}
	cancel()
	<-done
}

func TestStatusRequestQueueFullIsExplicit(t *testing.T) {
	tr := newCANTransport(newTestSocket())
	ecu := newECU(tr, newLogger(LogLevelNone), []uint8{50})
	ecu.SetPowered(true)
	for i := 0; i < canQueueSize; i++ {
		if err := tr.Submit(txRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ecu.SetGear(1); err != errCANQueueFull {
		t.Fatalf("queue overflow hidden: %v", err)
	}
	if ecu.gearsSentOnPower {
		t.Fatal("full queue marked startup complete")
	}
}
