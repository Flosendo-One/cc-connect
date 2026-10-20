package core

// EDEN-1618: a message queued behind a busy session is never lost without its sender being told. The three ways a
// queue used to vanish, each driven here: a reset, an idle kill, and a restart of the bridge.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func queuedItem(p Platform, key, id, content string) queuedMessage {
	return queuedMessage{messageID: id, platform: p, replyCtx: "ctx-" + id, content: content,
		userID: "U1", userName: "Ivan", msgPlatform: p.Name(), msgSessionKey: key}
}

func containsAll(t *testing.T, sent []string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		found := false
		for _, s := range sent {
			if strings.Contains(s, w) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no message contains %q; sent: %q", w, sent)
		}
	}
}

func TestQueuedFirstLine(t *testing.T) {
	cases := map[string]string{
		"sign 1":                     "sign 1",
		"\n\n  first line  \nsecond": "first line",
		"":                           "(a message with no text)",
		"   \n \n":                   "(a message with no text)",
	}
	for in, want := range cases {
		if got := queuedFirstLine(in); got != want {
			t.Fatalf("queuedFirstLine(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 200)
	if got := queuedFirstLine(long); got != strings.Repeat("é", queuedFirstLineMax)+"…" {
		t.Fatalf("a long line is cut at %d characters, got %d", queuedFirstLineMax, len([]rune(got)))
	}
}

// Reset (/new, /stop, a self-reset): each queued message is named in its own thread as not processed.
func TestFlosendoQueue_ResetNamesEachQueuedMessage(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	key := "test:channel"
	state := &interactiveState{platform: p, replyCtx: "ctx", pendingMessages: []queuedMessage{
		queuedItem(p, key, "m1", "sign 1\nthe rest of it"),
		queuedItem(p, key, "m2", "1385 words ok"),
	}}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	e.cleanupInteractiveState(key)

	sent := p.getSent()
	containsAll(t, sent, "Not processed (session reset): “sign 1”", "Not processed (session reset): “1385 words ok”")
	for _, s := range sent {
		if strings.Contains(s, "the rest of it") {
			t.Fatalf("only the first line is quoted: %q", s)
		}
	}
}

// Idle kill: the queued messages go to a fresh session, in order, once the dead turn lets go; one alert line is posted.
func TestFlosendoQueue_IdleKillReplaysQueueAndAlerts(t *testing.T) {
	p := &stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	dead := newControllableSession("dead")
	fresh := newQueuingSession("fresh")
	var starts atomic.Int32
	agent := &controllableAgent{startSessionFn: func(_ context.Context, _ string) (AgentSession, error) {
		starts.Add(1)
		return fresh, nil
	}}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.SetEventIdleTimeout(100 * time.Millisecond)
	e.SetAlertSessionKey("test:alerts")
	oldWait, oldPoll := replayWaitLimit, replayPollInterval
	replayWaitLimit, replayPollInterval = 3*time.Second, 20*time.Millisecond
	t.Cleanup(func() { replayWaitLimit, replayPollInterval = oldWait, oldPoll })

	key := "test:channel"
	state := &interactiveState{agentSession: dead, platform: p, replyCtx: "ctx", pendingMessages: []queuedMessage{
		queuedItem(p, key, "m1", "sign 1"),
	}}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()
	session := e.sessions.GetOrCreateActive(key)
	session.TryLock()

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "", time.Now(), nil, nil, nil, telemetryMsgCtx{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the idle kill did not end the turn")
	}
	// The turn's caller releases the session after the kill; the replay must wait for that, not race it.
	time.Sleep(100 * time.Millisecond)
	if starts.Load() != 0 {
		t.Fatal("the replay started while the dead turn still held the session")
	}
	session.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for {
		fresh.sendMu.Lock()
		got := strings.Join(fresh.sendCalls, "\n")
		fresh.sendMu.Unlock()
		if strings.Contains(got, "sign 1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the queued message never reached a fresh session; prompts: %q; sent: %q", got, p.getSent())
		}
		time.Sleep(20 * time.Millisecond)
	}
	sent := p.getSent()
	containsAll(t, sent, "timed out", "test:channel", "1 queued message")
	for _, s := range sent {
		if strings.Contains(s, "Not processed") {
			t.Fatalf("a replayed message must not also be reported as not processed: %q", s)
		}
	}
	if p.reconstructSessionKey != "test:alerts" {
		t.Fatalf("the alert went to %q, want the alert session", p.reconstructSessionKey)
	}
}

// Restart: queues are written to disk at Stop; after the restart each sender is told, in the message's thread.
func TestFlosendoQueue_RestartSavesQueueAndTellsSenders(t *testing.T) {
	dir := t.TempDir()
	p1 := &stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e1 := NewEngine("eden", &stubAgent{}, []Platform{p1}, "", LangEnglish)
	e1.SetDataDir(dir)
	key := "test:channel"
	e1.interactiveMu.Lock()
	e1.interactiveStates[key] = &interactiveState{platform: p1, replyCtx: "ctx", pendingMessages: []queuedMessage{
		queuedItem(p1, key, "m1", "promote 1618"),
	}}
	e1.interactiveMu.Unlock()

	_ = e1.Stop()

	path := filepath.Join(dir, "run", "pending_queue_eden.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the queue was not saved at stop: %v", err)
	}
	if !strings.Contains(string(data), "promote 1618") {
		t.Fatalf("the saved queue does not name the message: %s", data)
	}

	p2 := &stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e2 := NewEngine("eden", &stubAgent{}, []Platform{p2}, "", LangEnglish)
	e2.SetDataDir(dir)
	e2.onPlatformReady(p2)

	deadline := time.Now().Add(3 * time.Second)
	for len(p2.getSent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	containsAll(t, p2.getSent(), "Not processed (the bridge restarted): “promote 1618”")
	if p2.reconstructSessionKey != key {
		t.Fatalf("the notice went to %q, want the message's own target %q", p2.reconstructSessionKey, key)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the saved queue must be removed once every sender is told (err %v)", err)
	}
}

// The second review's case: a real restart stops the platforms first, which cancels the engine's context, and the
// running turn then drains or drops its own queue before Stop runs. The queue must be on disk before the cancel, and a
// message that arrives after it must be saved, never sent into the dying turn or reported to a closed connection.
func TestFlosendoQueue_RestartWithALiveTurnSavesBeforeTheCancel(t *testing.T) {
	dir := t.TempDir()
	p := &stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("eden", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDataDir(dir)
	key := "test:channel"
	live := newControllableSession("live")
	state := &interactiveState{agentSession: live, platform: p, replyCtx: "ctx", pendingMessages: []queuedMessage{
		queuedItem(p, key, "m1", "sign 1"),
	}}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()
	session := e.sessions.GetOrCreateActive(key)
	session.TryLock()

	_ = e.StopPlatforms()

	// A message that slipped in after the cancel, then the turn's caller drains as it does when its turn ends.
	state.mu.Lock()
	state.pendingMessages = append(state.pendingMessages, queuedItem(p, key, "m2", "promote 1618"))
	state.mu.Unlock()
	e.drainPendingMessages(state, session, e.sessions, key)
	_ = e.Stop()

	data, err := os.ReadFile(filepath.Join(dir, "run", "pending_queue_eden.json"))
	if err != nil {
		t.Fatalf("nothing saved: %v", err)
	}
	for _, want := range []string{"sign 1", "promote 1618"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("the saved queue lacks %q: %s", want, data)
		}
	}
	for _, s := range p.getSent() {
		if strings.Contains(s, "Not processed") {
			t.Fatalf("nothing is reported while the bridge stops (the connection is closing): %q", s)
		}
	}
}

// A saved message older than a week is dropped, not announced late; a newer one is announced and removed.
func TestFlosendoQueue_SavedQueueExpiresAfterAWeek(t *testing.T) {
	dir := t.TempDir()
	p := &stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("eden", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDataDir(dir)
	old := savedQueuedMessage{ReplyTarget: "test:channel", FirstLine: "stale one", SavedAt: time.Now().Add(-8 * 24 * time.Hour)}
	fresh := savedQueuedMessage{ReplyTarget: "test:channel", FirstLine: "fresh one", SavedAt: time.Now()}
	path := filepath.Join(dir, "run", "pending_queue_eden.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.Marshal([]savedQueuedMessage{old, fresh})
	_ = os.WriteFile(path, data, 0o600)

	e.noticeSavedQueue(p)

	sent := strings.Join(p.getSent(), "\n")
	if !strings.Contains(sent, "fresh one") || strings.Contains(sent, "stale one") {
		t.Fatalf("want only the fresh notice, got %q", sent)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("both handled, so the file goes (err %v)", err)
	}
}
