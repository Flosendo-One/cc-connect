package core

import (
	"strings"
	"testing"
	"time"
)

// Flosendo: a scheduled prompt (timer or cron job) that fires while its session is mid-turn waits for
// that turn to end and then runs, instead of being dropped as "session is busy" while its "⏰" notice
// had already posted (9 Oct 2026: overnight nudges that said they started and did nothing).

func withScheduledLockTiming(t *testing.T, poll, max time.Duration) {
	t.Helper()
	oldPoll, oldMax := scheduledLockPoll, scheduledBusyWaitMax
	scheduledLockPoll, scheduledBusyWaitMax = poll, max
	t.Cleanup(func() { scheduledLockPoll, scheduledBusyWaitMax = oldPoll, oldMax })
}

func TestWaitForScheduledTurnLock(t *testing.T) {
	free := &Session{}
	if !waitForScheduledTurnLock(free, time.Second, time.Millisecond) {
		t.Fatal("a free session should lock at once")
	}

	busy := &Session{}
	busy.TryLock()
	go func() {
		time.Sleep(30 * time.Millisecond)
		busy.Unlock()
	}()
	if !waitForScheduledTurnLock(busy, time.Second, 5*time.Millisecond) {
		t.Fatal("a session freed within the wait should be locked by the waiter")
	}

	stuck := &Session{}
	stuck.TryLock()
	start := time.Now()
	if waitForScheduledTurnLock(stuck, 40*time.Millisecond, 5*time.Millisecond) {
		t.Fatal("a session that stays busy must not be locked")
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("the waiter gave up before its limit")
	}
}

func newScheduledTestEngine(t *testing.T) (*Engine, *stubCronReplyTargetPlatform) {
	t.Helper()
	platform := &stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "discord"}}
	agent := &resultAgent{session: newResultAgentSession("scheduled complete")}
	e := NewEngine("test", agent, []Platform{platform}, "", LangEnglish)
	t.Cleanup(e.cancel)
	return e, platform
}

func TestExecuteTimerJob_WaitsForBusySessionThenRuns(t *testing.T) {
	withScheduledLockTiming(t, 5*time.Millisecond, 5*time.Second)
	e, platform := newScheduledTestEngine(t)
	key := "discord:channel-1:user-1"
	session := e.sessions.GetOrCreateActive(key)
	if !session.TryLock() {
		t.Fatal("setup: could not take the session lock")
	}

	job := &TimerJob{ID: "t1", SessionKey: key, Prompt: "overnight nudge", Description: "Overnight build"}
	done := make(chan error, 1)
	go func() { done <- e.ExecuteTimerJob(job) }()

	time.Sleep(60 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the timer returned while its session was busy (err=%v); it must wait", err)
	default:
	}
	if sent := platform.getSent(); len(sent) != 0 {
		t.Fatalf("nothing may post while the timer waits (no premature start notice), got %q", sent)
	}

	session.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ExecuteTimerJob() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the timer did not run after the session was freed")
	}
	sent := platform.getSent()
	if len(sent) < 2 || sent[0] != "⏰ Overnight build" || sent[len(sent)-1] != "scheduled complete" {
		t.Fatalf("sent = %q, want the start notice then the result", sent)
	}
}

func TestExecuteCronJob_WaitsForBusySessionThenRuns(t *testing.T) {
	withScheduledLockTiming(t, 5*time.Millisecond, 5*time.Second)
	e, platform := newScheduledTestEngine(t)
	key := "discord:channel-1:user-1"
	session := e.sessions.GetOrCreateActive(key)
	session.TryLock()

	job := &CronJob{ID: "c1", SessionKey: key, Prompt: "daily summary", Description: "Daily summary"}
	done := make(chan error, 1)
	go func() { done <- e.ExecuteCronJob(job) }()
	time.Sleep(60 * time.Millisecond)
	if sent := platform.getSent(); len(sent) != 0 {
		t.Fatalf("nothing may post while the cron job waits, got %q", sent)
	}
	session.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ExecuteCronJob() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cron job did not run after the session was freed")
	}
	if sent := platform.getSent(); len(sent) < 2 || sent[0] != "⏰ Daily summary" {
		t.Fatalf("sent = %q, want the start notice then the result", sent)
	}
}

func TestExecuteTimerJob_GivesUpOnASessionThatStaysBusy(t *testing.T) {
	withScheduledLockTiming(t, 5*time.Millisecond, 50*time.Millisecond)
	e, platform := newScheduledTestEngine(t)
	key := "discord:channel-1:user-1"
	e.sessions.GetOrCreateActive(key).TryLock()

	err := e.ExecuteTimerJob(&TimerJob{ID: "t2", SessionKey: key, Prompt: "x", Description: "Nudge"})
	if err == nil || !strings.Contains(err.Error(), "stayed busy") {
		t.Fatalf("err = %v, want a stayed-busy error", err)
	}
	if sent := platform.getSent(); len(sent) != 0 {
		t.Fatalf("a nudge that never ran must post nothing, got %q", sent)
	}
}
