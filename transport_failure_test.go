package main

import (
	"testing"
	"time"
)

func TestTransportCause(t *testing.T) {
	w := &CanLinkWatcher{ifname: "can0", log: newLogger(LogLevelNone)}
	a := &App{canLink: w}
	if a.ecuTransportCause(time.Now()) != "" {
		t.Fatal("unknown link treated as failed")
	}
	for _, state := range []canState{canStateErrorActive, canStateErrorWarning, canStateErrorPassive, canStateBusOff, canStateStopped, canStateSleeping} {
		w.observe(canLinkSample{State: state})
		failed := a.ecuTransportCause(time.Now()) != ""
		want := state == canStateBusOff || state == canStateStopped || state == canStateSleeping
		if failed != want {
			t.Fatalf("state=%s failure=%t", state, failed)
		}
	}
	if a.ecuTransportCause(time.Now().Add(4*canLinkTick)) != "" {
		t.Fatal("expired sample latched failure")
	}
	w.observe(canLinkSample{State: canStateErrorActive})
	a.socketUnavailable.Store(true)
	if a.ecuTransportCause(time.Now()) == "" {
		t.Fatal("missing socket failure")
	}
	a.socketUnavailable.Store(false)
	if a.ecuTransportCause(time.Now()) != "" {
		t.Fatal("socket failure did not clear")
	}
}
