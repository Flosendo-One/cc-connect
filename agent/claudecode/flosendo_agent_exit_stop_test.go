//go:build !windows

package claudecode

// EDEN-1852 slice 2, the bridge stopping: systemd stops the bridge with KillMode=control-group, so its SIGTERM reaches
// every agent process with the bridge, and an agent can be reaped a moment before the bridge marks its stop (main does,
// before "shutting down..."). That end is the bridge's own; read as "on its own", S1's guard would post a false alarm
// at every restart. These tests replace the stop flag, so they live apart from the ones that run on today's code.

import (
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestFlosendo1852_TermJustBeforeTheBridgeMarksItsStop(t *testing.T) {
	var stopAt atomic.Int64 // when the "bridge" marks its stop, in unix ns; 0 = not yet
	old := flosendoBridgeStopping
	flosendoBridgeStopping = func() bool { n := stopAt.Load(); return n != 0 && time.Now().UnixNano() >= n }
	t.Cleanup(func() { flosendoBridgeStopping = old })
	lb := captureLog(t)
	cs, _ := newExitSession(t, exitFake{fixture: "idle_after_turn.jsonl", end: "die:TERM"}, "slack:C0ASQ5LPS4W")
	c := collect(cs)
	if err := cs.Send("start the background job", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "the turn's result", 10*time.Second, turnDone(c))
	stopAt.Store(time.Now().Add(300 * time.Millisecond).UnixNano()) // the agent is gone; the bridge marks 300 ms later
	line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
	want(t, line, "requested", "true", "signal", "terminated")
}

// The CLI traps a stop's SIGTERM and exits 0 within about 40 ms (2.1.286, measured 10 Nov): that end waits for the
// bridge's mark too. systemd's SIGTERM reaches the agent first; the bridge marks its stop 300 ms later.
func TestFlosendo1852_StopSIGTERMTheCLITrapsExitsZero(t *testing.T) {
	var stopAt atomic.Int64
	old := flosendoBridgeStopping
	flosendoBridgeStopping = func() bool { n := stopAt.Load(); return n != 0 && time.Now().UnixNano() >= n }
	t.Cleanup(func() { flosendoBridgeStopping = old })
	lb := captureLog(t)
	cs, _ := newExitSession(t, exitFake{fixture: "idle_after_turn.jsonl", end: "trap-term"}, "slack:C0B1FCC755W")
	c := collect(cs)
	if err := cs.Send("start the background job", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "the turn's result", 10*time.Second, turnDone(c))
	time.Sleep(100 * time.Millisecond) // the fake reaches its ending
	stopAt.Store(time.Now().Add(300 * time.Millisecond).UnixNano())
	if err := syscall.Kill(pidOf(cs), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
	want(t, line, "requested", "true", "status", "0")
}

// QA round 1, finding 1: a SIGKILL (the memory fence, a person's kill -9, 6 Nov) is never a stop's: it is logged at
// once, so a stop marked 300 ms later does not make it requested.
func TestFlosendo1852_SigkillIsLoggedAtOnce(t *testing.T) {
	var stopAt atomic.Int64
	old := flosendoBridgeStopping
	flosendoBridgeStopping = func() bool { n := stopAt.Load(); return n != 0 && time.Now().UnixNano() >= n }
	t.Cleanup(func() { flosendoBridgeStopping = old })
	lb := captureLog(t)
	cs, _ := newExitSession(t, exitFake{fixture: "idle_after_turn.jsonl", end: "die:KILL"}, "slack:C0ASQ5LPS4W")
	c := collect(cs)
	if err := cs.Send("start the background job", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "the turn's result", 10*time.Second, turnDone(c))
	stopAt.Store(time.Now().Add(300 * time.Millisecond).UnixNano())
	line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
	want(t, line, "requested", "false", "signal", "killed")
}

// The exit line reads core's flag, the one main marks.
func TestFlosendo1852_StopFlagIsCores(t *testing.T) {
	if reflect.ValueOf(flosendoBridgeStopping).Pointer() != reflect.ValueOf(core.FlosendoBridgeStopping).Pointer() {
		t.Fatal("the exit line does not read core.FlosendoBridgeStopping")
	}
}
