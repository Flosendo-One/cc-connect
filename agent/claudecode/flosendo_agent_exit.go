package claudecode

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// EDEN-1852 slice 2: every agent process exit logs one INFO line, "flosendo: agent exit", with the session key, the
// pid, the exit status or the signal, whether the bridge asked for it, how long the process ran, and whether a turn was
// open (the engine's, or one the CLI started itself). On 6 Nov the bridge logged none of three deaths: finishReadLoop
// logged an exit only when stderr had text, and the engine's "agent process exited" comes only from its own turn loop,
// which an idle session, or a turn the CLI started itself (A's), does not have. The dev-box guard (EDEN-1852 slice 1,
// scripts/ops/dev-box-guard in Flosendo-One/eden) reads this line first: pid=, requested=true|false, signal= or status=.
//
// Requested means the bridge ended it: Close() began before the end was seen (a /new, a cron or timer run's end, an
// idle kill, a stop; a Close() that begins later, such as a message meeting the dead process, did not cause it), or the
// bridge is stopping: the session's context was cancelled (Engine.Stop cancels the engine's context, which kills each
// process, before it closes the sessions) or the bridge's process is stopping (main marks it at the signal). systemd
// stops the bridge with KillMode=control-group, so the same SIGTERM reaches every agent with the bridge, and the CLI
// (2.1.286, measured) traps SIGTERM, SIGINT and SIGHUP and exits 0 within about 40 ms: an end that is not yet
// requested, by any exit status or by one of those three signals, waits up to flosendoStopGrace for the bridge's stop.
// A SIGKILL or a crash signal is logged at once.
//
// Declared limits: an end within flosendoStopGrace before a bridge stop reads as requested; a Close() that begins in
// the moment between the process's end and the bridge seeing it (Wait's return) reads as requested.

type flosendoExit struct {
	key       string // the CC_SESSION_KEY the process was started with ("" when none)
	started   time.Time
	requested atomic.Bool // set as Close() begins
}

// flosendoStopGrace is how long an end that may be a bridge stop's waits to learn that the bridge stops.
var flosendoStopGrace = 2 * time.Second

// flosendoBridgeStopping is core's flag; a test replaces it.
var flosendoBridgeStopping = core.FlosendoBridgeStopping

// flosendoSessionKey is the CC_SESSION_KEY in env, the process's own environment (the last one wins, as for the CLI).
func flosendoSessionKey(env []string) string {
	key := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CC_SESSION_KEY="); ok {
			key = v
		}
	}
	return key
}

// flosendoStopped reports whether the bridge is stopping: the session's context (the engine's) or the process.
func (cs *claudeSession) flosendoStopped() bool {
	return cs.ctx.Err() != nil || flosendoBridgeStopping()
}

// flosendoTurnAtExit is "engine" or "wakeup" when a turn was open as the process ended, else "". A turn the CLI has
// opened (its system init) with no event routed yet is open too, on the side its first event would take; so is our
// message once the CLI has taken it in (command_lifecycle "started"), or, from a CLI that echoes no lifecycle, once it
// is written and unanswered: the engine's turn waits for it (QA round 1, finding 4).
func (cs *claudeSession) flosendoTurnAtExit() string {
	cs.turnMu.Lock()
	defer cs.turnMu.Unlock()
	ch := cs.turnChannel
	if ch == nil {
		switch {
		case cs.turns.cliTurnOpen:
			ch = cs.pickChannelLocked()
		case cs.turns.lifecycle && cs.turns.anyTaken(), !cs.turns.lifecycle && cs.inTurn.Load() > 0:
			return "engine"
		default:
			return ""
		}
	}
	if cs.orphanEvents != nil && ch == cs.orphanEvents {
		return "wakeup"
	}
	return "engine"
}

// flosendoLogExit writes the exit line. finishReadLoop calls it once, after the process is reaped and before anything
// it emits can end the open turn.
func (cs *claudeSession) flosendoLogExit() {
	if cs.cmd == nil || cs.cmd.Process == nil {
		return
	}
	requested := cs.flosendoExit.requested.Load() || cs.flosendoStopped() // Close() counts only if it began before now
	ran := time.Since(cs.flosendoExit.started).Round(time.Second)         // its life, not the wait below (QA finding 3)
	attrs := []any{"session_key", cs.flosendoExit.key, "pid", cs.cmd.Process.Pid}
	mayBeStop := true
	if ps := cs.cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig := ws.Signal()
			attrs = append(attrs, "signal", sig.String())
			mayBeStop = sig == syscall.SIGTERM || sig == syscall.SIGINT || sig == syscall.SIGHUP
		} else {
			attrs = append(attrs, "status", ps.ExitCode())
		}
	}
	if !requested && mayBeStop {
		for deadline := time.Now().Add(flosendoStopGrace); !requested && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
			requested = cs.flosendoStopped()
		}
	}
	attrs = append(attrs, "requested", requested, "ran", ran)
	if turn := cs.flosendoTurnAtExit(); turn != "" {
		attrs = append(attrs, "in_turn", true, "turn", turn)
	} else {
		attrs = append(attrs, "in_turn", false)
	}
	slog.Info("flosendo: agent exit", attrs...)
}
