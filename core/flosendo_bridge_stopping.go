package core

import "sync/atomic"

// EDEN-1852 slice 2: the bridge's process has started to stop (a SIGTERM or SIGINT reached it, or /restart). systemd
// stops the bridge with KillMode=control-group, so the same SIGTERM reaches every agent process with the bridge, and an
// agent can end before the engine closes it. The agent's exit line reads this to say that end was the bridge's own.
var flosendoBridgeStopping atomic.Bool

// FlosendoMarkBridgeStopping is called once, as soon as the bridge's process starts to stop. It is never cleared: the
// process is on its way out.
func FlosendoMarkBridgeStopping() { flosendoBridgeStopping.Store(true) }

// FlosendoBridgeStopping reports whether the bridge's process has started to stop.
func FlosendoBridgeStopping() bool { return flosendoBridgeStopping.Load() }
