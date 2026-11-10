package core

import "testing"

// EDEN-1852 slice 2: main marks the bridge's stop once; from then on every agent's end is the bridge's own.
func TestFlosendo1852_BridgeStoppingFlag(t *testing.T) {
	flosendoBridgeStopping.Store(false)
	t.Cleanup(func() { flosendoBridgeStopping.Store(false) })
	if FlosendoBridgeStopping() {
		t.Fatal("the bridge reads as stopping before anything marked it")
	}
	FlosendoMarkBridgeStopping()
	if !FlosendoBridgeStopping() {
		t.Fatal("the mark did not hold")
	}
}
