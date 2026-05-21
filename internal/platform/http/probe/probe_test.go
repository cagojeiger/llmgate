package probe

import "testing"

func TestState_Idempotent(t *testing.T) {
	probe := NewState()
	if probe.IsShuttingDown() {
		t.Fatal("zero state must report not shutting down")
	}
	probe.MarkShuttingDown()
	probe.MarkShuttingDown() // idempotent: no panic, no flip back
	if !probe.IsShuttingDown() {
		t.Fatal("after MarkShuttingDown must report shutting down")
	}
}

func TestState_NilSafe(t *testing.T) {
	var probe *State
	probe.MarkShuttingDown()
	if probe.IsShuttingDown() {
		t.Fatal("nil pointer must read as not shutting down")
	}
}
