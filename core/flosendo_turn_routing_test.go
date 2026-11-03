package core

// EDEN-1137 (contract 4c7fee07, D0 621cab04, the chief of staff's calls 95053b0a and 7a599068), the engine's half.
// Ivan: "alright, continue all the build 24/7". A turn that waits for a reply that went elsewhere, until the idle
// kill takes the session and its background jobs, stops the build at night. The agent's half and the replays of the
// real CLI's lines are in agent/claudecode/flosendo_turn_routing_test.go.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTurnFixture(t *testing.T, as AgentSession) (*Engine, *stubMediaPlatform, *Session, *interactiveState, string) {
	t.Helper()
	p := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	key := "test:user1"
	session := e.sessions.GetOrCreateActive(key)
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx-1", canonicalSessionKey: key}
	e.interactiveStates[key] = state
	return e, p, session, state, key
}

func runTurn(e *Engine, state *interactiveState, session *Session, key string, sendDone <-chan error) <-chan any {
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		e.processInteractiveEvents(state, session, e.sessions, key, "m1", time.Now(), nil, sendDone, state.replyCtx, telemetryMsgCtx{})
	}()
	return done
}

func waitTurn(t *testing.T, done <-chan any, d time.Duration, what string) {
	t.Helper()
	select {
	case r := <-done:
		if r != nil {
			t.Fatalf("%s: the turn panicked: %v", what, r)
		}
	case <-time.After(d):
		t.Fatalf("%s: the turn did not end within %s", what, d)
	}
}

// Item 2, the engine's half (D3a; 2 Nov: the first system line of the mis-routed turn disarmed the phantom grace for
// good, and the turn waited 30 minutes). A session-id-only line and a tool result after an ignored phantom must not
// keep the turn alive: it ends at the grace.
func TestFlosendo1137_PhantomGraceSurvivesBypassEvents(t *testing.T) {
	orig := phantomResultGrace
	phantomResultGrace = 50 * time.Millisecond
	defer func() { phantomResultGrace = orig }()
	as := newControllableSession("s1")
	e, p, session, state, key := newTurnFixture(t, as)
	e.eventIdleTimeout = time.Hour
	as.events <- Event{Type: EventResult, Done: true} // the phantom
	as.events <- Event{Type: EventText, SessionID: "s1"}
	ok := 0
	as.events <- Event{Type: EventToolResult, ToolResult: "ok", ToolExitCode: &ok}
	waitTurn(t, runTurn(e, state, session, key, nil), 2*time.Second, "phantom then bypass lines")
	if got := p.getSent(); len(got) == 0 || got[len(got)-1] != e.i18n.T(MsgEmptyResponse) {
		t.Fatalf("sent = %#v, want the turn to end with the empty-response line", got)
	}
}

// Item 3, the engine's half: when the agent says the CLI never took the message in, the turn ends with EDEN-1618's
// notice quoting the message, posted once.
func TestFlosendo1137_NotTakenEndsTurnWithNotice(t *testing.T) {
	as := newControllableSession("s1")
	e, p, session, state, key := newTurnFixture(t, as)
	state.flosendoTurnFirstLine = "Reply with the single word THIRD."
	as.events <- Event{Type: EventFlosendoNotTaken}
	waitTurn(t, runTurn(e, state, session, key, nil), 2*time.Second, "not taken")
	got := p.getSent()
	want := notProcessedText(notTakenReason, "Reply with the single word THIRD.")
	n := 0
	for _, s := range got {
		if strings.HasPrefix(s, want) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("sent = %#v, want the notice %q once", got, want)
	}
}

type settlingSession struct {
	*controllableAgentSession
	settled atomic.Int32
	sendErr error
}

func (s *settlingSession) SettleTurn() { s.settled.Add(1) }

// Item 7 (D3d): every way an engine turn ends settles the agent's routing state, so the next self-fired turn still goes
// to the wakeup path and the next Send still reaches the engine.
func TestFlosendo1137_TurnCountSettlesOnEveryEnd(t *testing.T) {
	origGrace := phantomResultGrace
	phantomResultGrace = 20 * time.Millisecond
	defer func() { phantomResultGrace = origGrace }()
	rows := []struct {
		name string
		run  func(e *Engine, as *settlingSession, state *interactiveState) <-chan error
	}{
		{"result", func(e *Engine, as *settlingSession, _ *interactiveState) <-chan error {
			as.events <- Event{Type: EventResult, Content: "ok", Done: true, OutputTokens: 3}
			return nil
		}},
		{"not taken", func(e *Engine, as *settlingSession, _ *interactiveState) <-chan error {
			as.events <- Event{Type: EventFlosendoNotTaken}
			return nil
		}},
		{"phantom accepted at the grace", func(e *Engine, as *settlingSession, _ *interactiveState) <-chan error {
			as.events <- Event{Type: EventResult, Done: true}
			return nil
		}},
		{"idle kill", func(e *Engine, as *settlingSession, _ *interactiveState) <-chan error {
			e.eventIdleTimeout = 50 * time.Millisecond
			return nil
		}},
		{"stop", func(e *Engine, as *settlingSession, state *interactiveState) <-chan error {
			go func() { time.Sleep(30 * time.Millisecond); state.markStopped() }()
			return nil
		}},
		{"max turn time", func(e *Engine, as *settlingSession, _ *interactiveState) <-chan error {
			e.maxTurnTime = 50 * time.Millisecond
			go func() { time.Sleep(150 * time.Millisecond); as.events <- Event{Type: EventResult, Done: true} }()
			return nil
		}},
		{"send error", func(e *Engine, as *settlingSession, _ *interactiveState) <-chan error {
			ch := make(chan error, 1)
			ch <- errors.New("write |1: broken pipe")
			return ch
		}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			as := &settlingSession{controllableAgentSession: newControllableSession("s1")}
			e, _, session, state, key := newTurnFixture(t, as)
			e.eventIdleTimeout = time.Hour
			sendDone := r.run(e, as, state)
			waitTurn(t, runTurn(e, state, session, key, sendDone), 3*time.Second, r.name)
			if n := as.settled.Load(); n < 1 {
				t.Fatalf("%s: the turn ended without settling the agent's count", r.name)
			}
		})
	}
}

// resetOnSendPlatform runs a /new (cleanupInteractiveState, what cmdNew runs) from inside the turn's instant reply,
// which processInteractiveEvents sends after it releases state.mu and before it reads the session's events: the
// window of upstream's nil-agentSession panic (found under EDEN-1759: engine.go:5365 in its plant P1's log). Live, the
// /new comes from another goroutine at that moment; here it is made to land there every time.
type resetOnSendPlatform struct {
	*stubMediaPlatform
	reset func()
	once  atomic.Bool
}

func (p *resetOnSendPlatform) Send(ctx context.Context, rctx any, content string) error {
	if p.once.CompareAndSwap(false, true) {
		p.reset()
	}
	return p.stubMediaPlatform.Send(ctx, rctx, content)
}

// Item 13: a /new that lands after a turn started, before it reads its events, must end that turn, not stop the
// bridge (a panic in the turn's goroutine takes every channel down; there is no recover there).
func TestFlosendo1137_SessionResetBeforeEventLoopDoesNotPanic(t *testing.T) {
	as := newControllableSession("s1")
	e, sp, session, state, key := newTurnFixture(t, as)
	p := &resetOnSendPlatform{stubMediaPlatform: sp}
	p.reset = func() { e.cleanupInteractiveState(key) }
	state.platform = p
	e.instantReply.Enabled = true
	waitTurn(t, runTurn(e, state, session, key, nil), 3*time.Second, "a /new before the event loop")
	if !p.once.Load() {
		t.Fatal("the /new was never made: the instant reply did not run")
	}
}

// The wakeup half of a moved turn (D0 s5, 22 Oct): text of the self-fired part before our message was taken in is
// posted at once as a wakeup, although the engine's turn holds the session lock; a tool-only part posts nothing.
func TestFlosendo1137_MovedTurnFlushesItsTextAtOnce(t *testing.T) {
	as := newControllableSession("s1")
	e, p, session, state, _ := newTurnFixture(t, as)
	state.sessionManager = e.sessions
	if !session.TryLock() {
		t.Fatal("could not hold the session lock")
	}
	defer session.Unlock()
	ch := make(chan Event, 8)
	go e.orphanLoop(state, ch)
	ch <- Event{Type: EventToolUse, ToolName: "Bash"}
	ch <- Event{Type: EventFlosendoTurnMoved}
	ch <- Event{Type: EventText, Content: "the self-fired words"}
	ch <- Event{Type: EventFlosendoTurnMoved}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(p.getSent()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	got := p.getSent()
	if len(got) != 1 || !strings.Contains(got[0], "Scheduled wakeup") || !strings.Contains(got[0], "the self-fired words") {
		t.Fatalf("sent = %#v, want one wakeup with the self-fired words, at once (the lock is held)", got)
	}
	close(ch)
}

// staleCheckSession answers the engine's question whether a not-taken signal is still current.
type staleCheckSession struct {
	*controllableAgentSession
	current string
}

func (s *staleCheckSession) NotTakenCurrent(ids []string) bool {
	return len(ids) == 1 && ids[0] == s.current
}

// Round 2 (a), a race of my own: a not-taken signal sent while the turn it was for is ending (between its last read
// and its SettleTurn) stays in the channel. The next turn must not end with that signal's notice: the engine asks the
// agent whether it is still current and drops a stale one; a current one still ends the turn with the notice, once.
func TestFlosendo1137_StaleNotTakenSignalIsIgnored(t *testing.T) {
	signal := func(id string) Event {
		return Event{Type: EventFlosendoNotTaken, Metadata: map[string]any{"flosendo_not_taken": []string{id}}}
	}
	t.Run("stale", func(t *testing.T) {
		as := &staleCheckSession{controllableAgentSession: newControllableSession("s1"), current: "now"}
		e, p, session, state, key := newTurnFixture(t, as)
		e.eventIdleTimeout = time.Hour
		state.flosendoTurnFirstLine = "the next message"
		as.events <- signal("before")
		as.events <- Event{Type: EventResult, Content: "the real reply", Done: true, OutputTokens: 3}
		waitTurn(t, runTurn(e, state, session, key, nil), 2*time.Second, "a stale signal, then the reply")
		got := p.getSent()
		for _, s := range got {
			if strings.Contains(s, "Not processed") {
				t.Fatalf("a stale signal ended the next turn with the notice: %#v", got)
			}
		}
		if len(got) == 0 || !strings.Contains(strings.Join(got, "\n"), "the real reply") {
			t.Fatalf("sent = %#v, want the turn's real reply", got)
		}
	})
	t.Run("current", func(t *testing.T) {
		as := &staleCheckSession{controllableAgentSession: newControllableSession("s1"), current: "now"}
		e, p, session, state, key := newTurnFixture(t, as)
		e.eventIdleTimeout = time.Hour
		state.flosendoTurnFirstLine = "this message"
		as.events <- signal("now")
		waitTurn(t, runTurn(e, state, session, key, nil), 2*time.Second, "a current signal")
		want := notProcessedText(notTakenReason, "this message")
		n := 0
		for _, s := range p.getSent() {
			if strings.HasPrefix(s, want) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("sent = %#v, want the notice %q once", p.getSent(), want)
		}
	})
}

// A message that waited in the queue behind a busy turn, then was never taken in by the CLI, gets the notice quoting
// itself, not the message before it: the queued path's `queuedFirstLine(queued.content)` line in engine.go (stream A's
// QA of round 2, F1; its probe, kept as a suite test by the CoS's call, PLAN v657).
func TestFlosendo1137_QueuedTurnNoticeQuotesItsOwnMessage(t *testing.T) {
	as := newControllableSession("s1")
	e, p, session, state, key := newTurnFixture(t, as)
	e.eventIdleTimeout = time.Hour
	state.flosendoTurnFirstLine = "the first message"
	state.pendingMessages = []queuedMessage{{messageID: "m2", platform: p, replyCtx: "ctx-2", content: "the queued message"}}
	as.events <- Event{Type: EventResult, Content: "first reply", Done: true, OutputTokens: 3}
	done := runTurn(e, state, session, key, nil)
	time.Sleep(300 * time.Millisecond) // past the queued path's drainEvents and its Send
	as.events <- Event{Type: EventFlosendoNotTaken}
	waitTurn(t, done, 3*time.Second, "the queued message, never taken in")
	var notice string
	for _, s := range p.getSent() {
		if strings.Contains(s, "Not processed") {
			notice = s
		}
	}
	if !strings.Contains(notice, "the queued message") {
		t.Fatalf("the notice for the queued message = %q; sent = %#v", notice, p.getSent())
	}
}
