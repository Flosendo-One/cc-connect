package claudecode

// EDEN-1137: the white-box half of the routing tests (they use the lifecycle state, so they cannot run on the code
// before it; their red proofs are the planted breaks). The behaviour tests are in flosendo_turn_routing_test.go.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Item 2, lifecycle half: with no turn open, a system line or a tool result goes to the wakeup path when no turn of
// ours is running, and to the engine when ours has been taken in.
func TestFlosendo1137_BypassEventsFollowLifecycle(t *testing.T) {
	newCS := func() *claudeSession {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		cs := &claudeSession{events: make(chan core.Event, 16), orphanEvents: make(chan core.Event, 16), ctx: ctx}
		cs.alive.Store(true)
		return cs
	}
	sys := map[string]any{"type": "system", "subtype": "task_notification", "session_id": "s"}
	tool := map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "content": "ok"}}}}
	t.Run("no turn open, lifecycle known, nothing of ours taken: the wakeup path", func(t *testing.T) {
		cs := newCS()
		cs.turns.lifecycle = true
		cs.turns.ours = map[string]flosendoCmdState{"u": cmdQueued}
		cs.handleSystem(sys)
		cs.handleUser(tool)
		if e := drainOne(cs.events); e != nil {
			t.Fatalf("a line of no turn of ours reached the engine's turn: %+v", e)
		}
	})
	t.Run("no turn open, ours taken: the engine", func(t *testing.T) {
		cs := newCS()
		cs.turns.lifecycle = true
		cs.turns.ours = map[string]flosendoCmdState{"u": cmdStarted}
		cs.handleSystem(sys)
		if e := drainOne(cs.events); e == nil {
			t.Fatal("the system line of our own turn did not reach the engine")
		}
	})
}

// Item 7, the agent's half: SettleTurn abandons messages no CLI turn is answering and puts the counter back to zero
// when no turn is open on the engine's channel; it leaves a turn that is open alone.
func TestFlosendo1137_SettleTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := &claudeSession{events: make(chan core.Event, 16), orphanEvents: make(chan core.Event, 16), ctx: ctx}
	cs.inTurn.Store(2)
	cs.turns.lifecycle = true
	cs.turns.ours = map[string]flosendoCmdState{"a": cmdQueued}
	cs.SettleTurn()
	if cs.inTurn.Load() != 0 || cs.turns.ours["a"] != cmdAbandoned {
		t.Fatalf("after SettleTurn: count %d, a=%v; want 0 and abandoned", cs.inTurn.Load(), cs.turns.ours["a"])
	}
	// An abandoned message that starts later is shown on the wakeup path, never handed to an engine turn.
	cs.handleCommandLifecycle(map[string]any{"command_uuid": "a", "state": "started"})
	cs.handleAssistant(map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "late"}}}})
	if e := drainOne(cs.orphanEvents); e == nil || e.Content != "late" {
		t.Fatalf("a late answer to an abandoned message did not go to the wakeup path: %+v", e)
	}
	// An open turn on the engine's channel is left alone.
	cs2 := &claudeSession{events: make(chan core.Event, 16), orphanEvents: make(chan core.Event, 16), ctx: ctx}
	cs2.inTurn.Store(1)
	cs2.handleAssistant(map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "x"}}}})
	cs2.SettleTurn()
	if cs2.inTurn.Load() != 1 {
		t.Fatalf("SettleTurn reset the count of a turn still open: %d", cs2.inTurn.Load())
	}
}

// D0 s2: two messages answered by one turn (the second folded in at a tool result, completed just before the result,
// the first just after). Both must be settled by that result: no stale "queued" may later fire the quiet backstop.
func TestFlosendo1137_TwoMessagesOneTurnLeaveNothingQueued(t *testing.T) {
	old := notTakenQuiet
	notTakenQuiet = 200 * time.Millisecond
	t.Cleanup(func() { notTakenQuiet = old })
	cs := newReplaySession(t, fakeCLI{fixture: fixturePath("s2_midturn_own.jsonl"), scale: 0.01})
	c := collect(cs)
	if err := cs.Send("sleep 8, then FIRST", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := cs.Send("Also reply with the single word SECOND.", "m2", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "the one result", 10*time.Second, func(rs []routed) bool { return len(results(rs, "engine")) == 1 })
	time.Sleep(notTakenQuiet * 3)
	rs := c.snapshot()
	for _, r := range rs {
		if r.evt.Type == core.EventFlosendoNotTaken {
			t.Fatalf("the backstop fired for a message the CLI had answered:\n%s", dumpRouted(rs))
		}
	}
	if got := results(rs, "engine"); got[0].Content != "FIRST\nSECOND" {
		t.Fatalf("result = %q", got[0].Content)
	}
	cs.turnMu.Lock()
	left := len(cs.turns.ours)
	cs.turnMu.Unlock()
	if left != 0 {
		t.Fatalf("%d message(s) still tracked after the turn that answered them", left)
	}
}

// D0 s4: messages written back to back each get their own turn (started 7-14 ms after the previous result). All six
// answers reach the engine in order, none the wakeup path, and the backstop never fires in the gaps.
func TestFlosendo1137_QueuedMessagesEachGetTheirTurn(t *testing.T) {
	old := notTakenQuiet
	notTakenQuiet = 300 * time.Millisecond
	t.Cleanup(func() { notTakenQuiet = old })
	sig := t.TempDir()
	cs := newReplaySession(t, fakeCLI{fixture: fixturePath("s4_queued_gap.jsonl"), scale: 0.01, signalDir: sig})
	c := collect(cs)
	want := []string{"ONE0", "TWO0", "ONE1", "TWO1", "ONE2", "TWO2"}
	for i := 0; i < 3; i++ {
		_ = cs.Send("Reply with the single word ONE.", "a", nil, nil)
		_ = cs.Send("Reply with the single word TWO.", "b", nil, nil)
		n := 2 * (i + 1)
		c.waitFor(t, "the pair's results", 10*time.Second, func(rs []routed) bool { return len(results(rs, "engine")) >= n })
	}
	time.Sleep(notTakenQuiet * 2)
	rs := c.snapshot()
	got := results(rs, "engine")
	if len(got) != len(want) {
		t.Fatalf("engine results = %d, want %d:\n%s", len(got), len(want), dumpRouted(rs))
	}
	for i, w := range want {
		if got[i].Content != w {
			t.Fatalf("result %d = %q, want %q", i, got[i].Content, w)
		}
	}
	for _, r := range rs {
		if r.ch == "wakeup" && r.evt.Type == core.EventResult || r.evt.Type == core.EventFlosendoNotTaken {
			t.Fatalf("a queued message's answer went astray:\n%s", dumpRouted(rs))
		}
	}
}

// Round 2 (b): the quiet check is paused from a turn's system init to its result, and every result ends the pause: a
// routed one, and the resume's synthetic one, which is dropped (D0 s1b: init, then that result, then our turn). A
// pause left on would leave a message the CLI never takes in waiting for the idle kill again.
func TestFlosendo1137_QuietPauseEndsAtEveryResult(t *testing.T) {
	rows := []struct {
		name   string
		result map[string]any
	}{
		{"a routed result", map[string]any{"type": "result", "subtype": "success", "num_turns": float64(1), "result": "done"}},
		{"the dropped synthetic result", map[string]any{"type": "result", "subtype": "success", "num_turns": float64(0),
			"origin": map[string]any{"kind": "task-notification"}, "result": ""}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cs := &claudeSession{events: make(chan core.Event, 16), orphanEvents: make(chan core.Event, 16), ctx: ctx}
			cs.alive.Store(true)
			defer cs.flosendoCloseTurns()
			cs.turns.lifecycle = true
			cs.turns.ours = map[string]flosendoCmdState{"u": cmdQueued}
			owes := func() bool {
				cs.turnMu.Lock()
				defer cs.turnMu.Unlock()
				return cs.owesQuietCheckLocked()
			}
			cs.handleSystem(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
			if owes() {
				t.Fatal("the quiet check runs between a turn's system init and its result")
			}
			cs.handleResult(r.result)
			if !owes() {
				t.Fatal("the quiet check stayed paused after the turn's result")
			}
		})
	}
}

// Round 2 (a): a not-taken signal names the messages it gives up on, and it is current only until the engine's turn
// settles (however it ended) or a newer signal replaces it. One still in the channel after that must not end the
// next turn (the engine half: core's TestFlosendo1137_StaleNotTakenSignalIsIgnored).
func TestFlosendo1137_NotTakenSignalCurrentUntilSettled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := &claudeSession{events: make(chan core.Event, 16), orphanEvents: make(chan core.Event, 16), ctx: ctx}
	defer cs.flosendoCloseTurns()
	cs.turns.lifecycle = true
	signal := func(id string) []string {
		t.Helper()
		cs.turnMu.Lock()
		cs.turns.ours[id] = cmdQueued
		cs.turnMu.Unlock()
		cs.quietFired()
		e := drainOne(cs.events)
		if e == nil || e.Type != core.EventFlosendoNotTaken {
			t.Fatalf("no not-taken signal for %s: %+v", id, e)
		}
		ids := core.FlosendoNotTakenIDs(*e)
		if !slices.Equal(ids, []string{id}) {
			t.Fatalf("the signal names %q, want [%s]", ids, id)
		}
		return ids
	}
	cs.turns.ours = map[string]flosendoCmdState{}
	first := signal("u1")
	if !cs.NotTakenCurrent(first) {
		t.Fatal("a signal was not current before the engine's turn settled")
	}
	cs.SettleTurn()
	if cs.NotTakenCurrent(first) {
		t.Fatal("a signal stayed current after the engine's turn settled")
	}
	second := signal("u2")
	third := signal("u3")
	if cs.NotTakenCurrent(second) || !cs.NotTakenCurrent(third) {
		t.Fatalf("after a newer signal: older current=%v, newer current=%v; want false, true",
			cs.NotTakenCurrent(second), cs.NotTakenCurrent(third))
	}
	if cs.NotTakenCurrent(nil) {
		t.Fatal("a signal that names no message counted as current")
	}
}
