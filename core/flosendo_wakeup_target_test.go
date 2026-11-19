package core

// EDEN-1943, contract 52ea6a01, ruling C-2 (the chief of staff, PLAN v1577): a wakeup, or a reply in a turn a stream
// started, posts at the channel root, never in the last thread. On 16-17 Nov the chief of staff's replies in turns a
// stream started were posted with the last Slack turn's reply context: after Ivan answered inside one post's thread
// (20:42:51Z), the next two, the urgent "promote 1981" ask among them, went into that thread.
//
//   8  a whole wakeup turn posts where the session lives (the channel root for a channel's key, the thread for a
//      thread's key), not with the cached context (WakeupPostsWhereTheSessionLives)
//   9  the part of a turn before one of our messages was taken in (1137's move) keeps that message's context
//      (ReplyKeepsItsThread; with 1137's TestFlosendo1137_MovedTurnFlushesItsTextAtOnce)
//   10 a context that cannot be rebuilt falls back to the cached one, as before (WakeupFallsBackToCachedReplyCtx)
//   and the two new channel notices (C-3) post at once where the session lives, with no wakeup header, even while an
//      engine turn holds the session lock (NoticePostsAtOnceWhereTheSessionLives)
// The notice's event type is written as its string, so this file compiles on the code before the fix.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type wakeupPost struct {
	ctx  any
	text string
}

// wakeupTargetPlatform records where each post goes. It rebuilds a reply context the way Slack does
// (platform/slack ReconstructReplyCtx): "test:<C>" is the channel root, "test:<C>:t:<ts>" that thread.
type wakeupTargetPlatform struct {
	stubPlatformEngine
	rebuildErr error
	pmu        sync.Mutex
	posts      []wakeupPost
}

func (p *wakeupTargetPlatform) record(rctx any, c string) error {
	p.pmu.Lock()
	p.posts = append(p.posts, wakeupPost{rctx, c})
	p.pmu.Unlock()
	return nil
}
func (p *wakeupTargetPlatform) Send(_ context.Context, rctx any, c string) error {
	return p.record(rctx, c)
}
func (p *wakeupTargetPlatform) Reply(_ context.Context, rctx any, c string) error {
	return p.record(rctx, c)
}
func (p *wakeupTargetPlatform) ReconstructReplyCtx(key string) (any, error) {
	if p.rebuildErr != nil {
		return nil, p.rebuildErr
	}
	parts := strings.SplitN(key, ":", 3)
	if len(parts) == 3 && strings.HasPrefix(parts[2], "t:") {
		return "thread:" + strings.TrimPrefix(parts[2], "t:"), nil
	}
	return "root:" + parts[1], nil
}

func (p *wakeupTargetPlatform) waitPosts(t *testing.T, n int, d time.Duration) []wakeupPost {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		p.pmu.Lock()
		got := append([]wakeupPost(nil), p.posts...)
		p.pmu.Unlock()
		if len(got) >= n {
			time.Sleep(50 * time.Millisecond)
			p.pmu.Lock()
			got = append([]wakeupPost(nil), p.posts...)
			p.pmu.Unlock()
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d posts, got %d: %#v", n, len(got), got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 16 Nov 20:42:51Z: Ivan answered inside a wakeup post's thread, so the cached context is that thread.
const ivansThread = "thread:1794858943.874629"

func newWakeupTarget(t *testing.T, key string) (*Engine, *wakeupTargetPlatform, *interactiveState) {
	t.Helper()
	p := &wakeupTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	state := &interactiveState{platform: p, replyCtx: ivansThread, canonicalSessionKey: key}
	return e, p, state
}

// Item 8.
func TestFlosendo1943_WakeupPostsWhereTheSessionLives(t *testing.T) {
	for _, c := range []struct{ key, want string }{
		{"test:C0B1FCC755W", "root:C0B1FCC755W"},
		{"test:C0B1FCC755W:t:1700000000.000100", "thread:1700000000.000100"},
	} {
		t.Run(c.key, func(t *testing.T) {
			e, p, state := newWakeupTarget(t, c.key)
			ch := make(chan Event, 4)
			go e.orphanLoop(state, ch)
			ch <- Event{Type: EventText, Content: "promote 1981 is waiting for you"}
			ch <- Event{Type: EventResult, Done: true}
			got := p.waitPosts(t, 1, 2*time.Second)
			close(ch)
			if len(got) != 1 || !strings.Contains(got[0].text, "promote 1981") {
				t.Fatalf("want the wakeup posted once, got %#v", got)
			}
			if got[0].ctx != c.want {
				t.Fatalf("the wakeup went to %v, want %v (the cached context is Ivan's last thread, %v)", got[0].ctx, c.want, ivansThread)
			}
		})
	}
}

// Item 9: the self-fired part before our message was taken in posts with that message's context.
func TestFlosendo1943_ReplyKeepsItsThread(t *testing.T) {
	e, p, state := newWakeupTarget(t, "test:C0B1FCC755W")
	ch := make(chan Event, 4)
	go e.orphanLoop(state, ch)
	ch <- Event{Type: EventText, Content: "the self-fired words"}
	ch <- Event{Type: EventFlosendoTurnMoved}
	got := p.waitPosts(t, 1, 2*time.Second)
	close(ch)
	if len(got) != 1 || got[0].ctx != ivansThread {
		t.Fatalf("the moved part must keep its message's context %v, got %#v", ivansThread, got)
	}
}

// Item 10.
func TestFlosendo1943_WakeupFallsBackToCachedReplyCtx(t *testing.T) {
	e, p, state := newWakeupTarget(t, "test:C0B1FCC755W")
	p.rebuildErr = errors.New("no proactive messaging here")
	ch := make(chan Event, 4)
	go e.orphanLoop(state, ch)
	ch <- Event{Type: EventText, Content: "a wakeup"}
	ch <- Event{Type: EventResult, Done: true}
	got := p.waitPosts(t, 1, 2*time.Second)
	close(ch)
	if len(got) != 1 || got[0].ctx != ivansThread {
		t.Fatalf("a context that cannot be rebuilt must fall back to the cached one, got %#v", got)
	}
}

// The notices: posted at once, as written, where the session lives, while an engine turn holds the session lock.
func TestFlosendo1943_NoticePostsAtOnceWhereTheSessionLives(t *testing.T) {
	key := "test:C0B1FCC755W"
	e, p, state := newWakeupTarget(t, key)
	state.sessionManager = e.sessions
	session := e.sessions.GetOrCreateActive(key)
	if !session.TryLock() {
		t.Fatal("could not hold the session lock")
	}
	defer session.Unlock()
	ch := make(chan Event, 4)
	go e.orphanLoop(state, ch)
	const notice = "❌ Session closed (a notice)."
	ch <- Event{Type: EventType("flosendo_notice"), Content: notice}
	got := p.waitPosts(t, 1, time.Second)
	close(ch)
	if len(got) != 1 || got[0].text != notice || got[0].ctx != "root:C0B1FCC755W" {
		t.Fatalf("want the notice posted once, as written, at the channel root, at once; got %#v", got)
	}
}
