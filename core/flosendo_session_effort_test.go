package core

// EDEN-1759 (Ivan, 31 Oct 2026: "for the build, try to find a balance between quality of using extra high and high of
// opus 5.5 but first get a baseline too so we can see if it meaningfully is better execution"): one session's effort,
// asked for at /new. `/new effort=high` gives that one session `--effort high`, if the eden guard clears it; a plain
// /new passes nothing, so every session runs at the default (xhigh, from settings) exactly as before.
// One test per item of the contract's failure list (comment 38f09593) that the fork carries:
//   1. a start without effort= changes (the default moves), on any channel
//   2. effort=high is lost (taken as the session name, dropped on the send path); the table low..max; an unknown value
//   3. the effort leaks to another channel, a thread, a relay or the next plain /new; a scheduled run: a reuse-mode
//      cron or timer run (the default mode) is a turn of the channel's session and runs at its effort, a new_per_run
//      one starts fresh at the default (QA round 1 finding 1; the CoS kept it, 2 Nov, reversal word "1759 cron at default")
//   4. a resume (a bridge restart) changes the effort mid-ticket, or asks the guard again
//   5. a refusal, an error, a timeout, a missing or unset guard starts anywhere but the default, or says nothing
//   6. the guard is asked for a session that asks for nothing, xhigh or max; the log carries the message or stderr
// Every test drives the engine's real paths: ExecuteSessionCmdAndSend (what `cc-connect send --session-cmd "/new ..."
// --message` calls), handleMessage and HandleRelay, with a stub agent that keeps the effort each start was given.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	cosChan       = "slack:C0B1FCC755W"
	clearedKickof = "You are stream B (eden-two, #eden-two), a builder\n\n**Ticket:** EDEN-4242. **Step:** build. KICKOFF-SECRET-1759"
)

// effortAgent keeps, per start, the effort its context carried ("" = none) and the session id it was asked to resume.
type effortAgent struct {
	stubAgent
	mu      sync.Mutex
	efforts []string
	resumes []string
}

func (a *effortAgent) SetSessionEnv([]string) {}

func (a *effortAgent) StartSession(ctx context.Context, id string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.efforts = append(a.efforts, SessionEffortFromContext(ctx))
	a.resumes = append(a.resumes, id)
	return newResultAgentSession("ok"), nil
}

func (a *effortAgent) starts() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.efforts)
}

// waitStart waits for start number n (1-based) and returns its effort and resume id.
func (a *effortAgent) waitStart(t *testing.T, n int) (string, string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for a.starts() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for agent start %d (have %d)", n, a.starts())
		}
		time.Sleep(5 * time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.efforts[n-1], a.resumes[n-1]
}

// guard writes a fake effort guard: it keeps each stdin it was given (one file per call) and stderr gets a secret.
type guard struct {
	path, dir string
}

func writeGuard(t *testing.T, exit int, reason string, sleep string) guard {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nf=\"%s/call.$$.json\"\ncat > \"$f\"\necho STDERR-SECRET-1759 >&2\n", dir)
	if sleep != "" {
		script += "sleep " + sleep + "\n"
	}
	script += fmt.Sprintf("printf '%%s\\n' %q\nexit %d\n", reason, exit)
	path := filepath.Join(dir, "effort_guard.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return guard{path: path, dir: dir}
}

func (g guard) calls(t *testing.T) []map[string]string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(g.dir, "call.*.json"))
	var out []map[string]string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("the guard's stdin is not one JSON object: %q (%v)", data, err)
		}
		out = append(out, m)
	}
	return out
}

func newEffortEngine(t *testing.T, store string, guardPath string) (*Engine, *effortAgent, *reconstructingPlatform) {
	t.Helper()
	agent := &effortAgent{}
	p := &reconstructingPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}
	e := NewEngine("eden", agent, []Platform{p}, store, LangEnglish)
	e.SetEffortGuard(guardPath)
	t.Cleanup(func() { e.cancel() })
	return e, agent, p
}

// kickoff is what the outgoing session runs: cc-connect send --session-cmd "<cmd>" --message "<msg>".
func kickoff(t *testing.T, e *Engine, key, cmd, msg string) {
	t.Helper()
	if err := e.ExecuteSessionCmdAndSend(key, cmd, msg); err != nil {
		t.Fatalf("ExecuteSessionCmdAndSend(%q, %q): %v", key, cmd, err)
	}
}

func message(e *Engine, p Platform, key, content string) {
	e.handleMessage(p, &Message{SessionKey: key, Platform: "slack", Content: content, ReplyCtx: "rc", UserID: "U1"})
}

func sentContaining(p *reconstructingPlatform, sub string) []string {
	var out []string
	for _, s := range p.getSent() {
		if strings.Contains(s, sub) {
			out = append(out, s)
		}
	}
	return out
}

// [1] a /new without effort= passes nothing at any channel: the agent's own setting (settings' xhigh) decides.
func TestFlosendoEffort_PlainNewPassesNothing(t *testing.T) {
	g := writeGuard(t, 0, "allowed", "")
	buf := captureInfoLog(t)
	e, agent, _ := newEffortEngine(t, "", g.path)
	for i, key := range []string{edenDev, edenTwo, cosChan} {
		kickoff(t, e, key, "/new", clearedKickof)
		if eff, _ := agent.waitStart(t, i+1); eff != "" {
			t.Fatalf("plain /new on %s started with effort %q; it must pass none", key, eff)
		}
	}
	if n := len(g.calls(t)); n != 0 {
		t.Fatalf("the guard was asked %d time(s) for sessions that asked for nothing", n)
	}
	log := buf.String()
	for _, key := range []string{edenDev, edenTwo, cosChan} {
		if !strings.Contains(log, "session_key="+key+" ") || !regexpLine(log, key, "effort=default effort_from=none") {
			t.Fatalf("the start line of %s does not say effort=default effort_from=none:\n%s", key, log)
		}
	}
}

// regexpLine reports whether the "flosendo: agent start" line of key carries want.
func regexpLine(log, key, want string) bool {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, `msg="flosendo: agent start"`) && strings.Contains(line, "session_key="+key+" ") &&
			strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// [2] effort=<v> reaches the start, never the session name; low..max; case; an unknown value is refused, said.
func TestFlosendoEffort_NewEffortReachesTheStart(t *testing.T) {
	cases := []struct {
		cmd, want, name string
		guarded         bool
	}{
		{"/new effort=high", "high", "", true},
		{"/new effort=low", "low", "", true},
		{"/new effort=medium", "medium", "", true},
		{"/new effort=xhigh", "xhigh", "", false},
		{"/new effort=max", "max", "", false},
		{"/new EFFORT=High", "high", "", true},
		{"/new effort=high ticket 1759", "high", "ticket 1759", true},
		{"/new ticket effort=high", "high", "ticket", true},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			g := writeGuard(t, 0, "allowed", "")
			e, agent, _ := newEffortEngine(t, "", g.path)
			kickoff(t, e, edenTwo, c.cmd, clearedKickof)
			if eff, _ := agent.waitStart(t, 1); eff != c.want {
				t.Fatalf("%q started at effort %q, want %q", c.cmd, eff, c.want)
			}
			if got := e.sessions.GetOrCreateActive(edenTwo).GetName(); got != c.name {
				t.Fatalf("%q named the session %q, want %q (effort= is not part of the name)", c.cmd, got, c.name)
			}
			if n := len(g.calls(t)); (n == 1) != c.guarded || n > 1 {
				t.Fatalf("%q asked the guard %d time(s); guarded=%v", c.cmd, n, c.guarded)
			}
		})
	}
	for _, bad := range []struct{ cmd, reply string }{
		{"/new effort=turbo", "effort turbo is not an effort"},
		{"/new effort=", "effort (empty) is not an effort"},
		{"/new effort=high effort=low", "effort= given twice"},
	} {
		t.Run(bad.cmd, func(t *testing.T) {
			g := writeGuard(t, 0, "allowed", "")
			e, agent, p := newEffortEngine(t, "", g.path)
			kickoff(t, e, edenTwo, bad.cmd, clearedKickof)
			if eff, _ := agent.waitStart(t, 1); eff != "" {
				t.Fatalf("%q started at %q, want the default", bad.cmd, eff)
			}
			if got := sentContaining(p, bad.reply); len(got) != 1 || !strings.HasSuffix(got[0], "; this session runs at the default") {
				t.Fatalf("want one reply %q ending with the default: %q", bad.reply, p.getSent())
			}
			if got := e.sessions.GetOrCreateActive(edenTwo).GetName(); got != "" {
				t.Fatalf("%q became the session name %q", bad.cmd, got)
			}
			if n := len(g.calls(t)); n != 0 {
				t.Fatalf("%q asked the guard %d time(s)", bad.cmd, n)
			}
		})
	}
}

// [3] the effort is that one session's: not the next plain /new, a thread, another channel or a relay.
func TestFlosendoEffort_DoesNotLeak(t *testing.T) {
	g := writeGuard(t, 0, "allowed", "")
	e, agent, p := newEffortEngine(t, "", g.path)
	kickoff(t, e, edenTwo, "/new effort=high", clearedKickof)
	if eff, _ := agent.waitStart(t, 1); eff != "high" {
		t.Fatalf("precondition: the cleared session started at %q", eff)
	}
	n := 1
	for _, c := range []struct{ what, key string }{
		{"a thread of #eden-two", edenTwo + ":1730000000.000100"},
		{"#eden-dev", edenDev},
		{"the chief of staff", cosChan},
	} {
		message(e, p, c.key, "hello")
		n++
		if eff, _ := agent.waitStart(t, n); eff != "" {
			t.Fatalf("%s started at effort %q after #eden-two's high session; want none", c.what, eff)
		}
	}
	waitTurnDone(t, e, edenTwo) // a /new while the turn is starting hits upstream's nil agentSession (README, 1759 QA)
	kickoff(t, e, edenTwo, "/new", clearedKickof)
	n++
	if eff, _ := agent.waitStart(t, n); eff != "" {
		t.Fatalf("the next plain /new on #eden-two started at %q; want none", eff)
	}

	relay := &startEffortRelayAgent{session: newResultAgentSession("ok")}
	e.agent = relay
	if _, err := e.HandleRelay(context.Background(), "source", edenTwo, "hello"); err != nil {
		t.Fatalf("HandleRelay: %v", err)
	}
	if relay.effort != "" {
		t.Fatalf("a relay from #eden-two started at effort %q; want none", relay.effort)
	}
}

// [3] a scheduled run, through the scheduler's own paths (ExecuteCronJob, ExecuteTimerJob): in the default mode
// (reuse) it is a turn of the channel's active session, so inside a cleared session it runs at that session's effort,
// with the stored decision (the guard is not asked again); new_per_run starts a fresh session at the default. QA round 1
// (stream A, d8f6b1f1) found the docs said every cron and timer run passes no effort; the CoS kept the behaviour.
func TestFlosendoEffort_ScheduledRunsJoinTheirSession(t *testing.T) {
	for _, c := range []struct {
		name, want string
		run        func(e *Engine) error
	}{
		{"a cron run in reuse mode (the default)", "high", func(e *Engine) error {
			return e.ExecuteCronJob(&CronJob{ID: "c1", SessionKey: edenTwo, Prompt: "weekly check", Description: "d"})
		}},
		{"a timer run in reuse mode (the default)", "high", func(e *Engine) error {
			return e.ExecuteTimerJob(&TimerJob{ID: "t1", SessionKey: edenTwo, Prompt: "wake", Description: "d"})
		}},
		{"a cron run in new_per_run mode", "", func(e *Engine) error {
			return e.ExecuteCronJob(&CronJob{ID: "c2", SessionKey: edenTwo, Prompt: "weekly check", Description: "d", SessionMode: "new_per_run"})
		}},
		{"a timer run in new_per_run mode", "", func(e *Engine) error {
			return e.ExecuteTimerJob(&TimerJob{ID: "t2", SessionKey: edenTwo, Prompt: "wake", Description: "d", SessionMode: "new_per_run"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := writeGuard(t, 0, "allowed", "")
			e, agent, _ := newEffortEngine(t, "", g.path)
			kickoff(t, e, edenTwo, "/new effort=high", clearedKickof)
			if eff, _ := agent.waitStart(t, 1); eff != "high" {
				t.Fatalf("precondition: the cleared session started at %q", eff)
			}
			waitTurnDone(t, e, edenTwo)
			e.cleanupInteractiveState(edenTwo) // its process has gone (idle, recycle); the session stays the channel's active one
			if err := c.run(e); err != nil {
				t.Fatalf("the scheduled run: %v", err)
			}
			if eff, _ := agent.waitStart(t, 2); eff != c.want {
				t.Fatalf("%s into #eden-two's cleared session started at %q; want %q (\"\" = the default)", c.name, eff, c.want)
			}
			if n := len(g.calls(t)); n != 1 {
				t.Fatalf("the guard was asked %d times; a scheduled run never asks it", n)
			}
		})
	}
}

// waitTurnDone waits until key's active session has finished its turn (unlocked), so its process is kept by then.
func waitTurnDone(t *testing.T, e *Engine, key string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s := e.sessions.GetOrCreateActive(key); s.Busy() || s.GetAgentSessionID() == ""; {
		if time.Now().After(deadline) {
			t.Fatalf("the turn of %s never finished", key)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type startEffortRelayAgent struct {
	stubAgent
	session AgentSession
	effort  string
}

func (a *startEffortRelayAgent) StartSession(ctx context.Context, _ string) (AgentSession, error) {
	a.effort = SessionEffortFromContext(ctx)
	return a.session, nil
}

// [4] the effort is kept with the session: after a bridge restart the resume starts at it, and the guard is not asked
// again; the next plain /new after the restart is at the default.
func TestFlosendoEffort_KeptAcrossARestart(t *testing.T) {
	store := filepath.Join(t.TempDir(), "sessions.json")
	g := writeGuard(t, 0, "allowed", "")
	e1, agent1, _ := newEffortEngine(t, store, g.path)
	kickoff(t, e1, edenTwo, "/new effort=high", clearedKickof)
	if eff, _ := agent1.waitStart(t, 1); eff != "high" {
		t.Fatalf("precondition: started at %q", eff)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e1.sessions.GetOrCreateActive(edenTwo).GetAgentSessionID() == "" {
		if time.Now().After(deadline) {
			t.Fatal("the session never recorded its agent session id")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e1.sessions.Save()
	e1.cancel()

	e2, agent2, p2 := newEffortEngine(t, store, g.path) // the restarted bridge: the same session store
	message(e2, p2, edenTwo, "and the next step")
	eff, resume := agent2.waitStart(t, 1)
	if resume == "" {
		t.Fatal("precondition: the restarted bridge did not resume the session")
	}
	if eff != "high" {
		t.Fatalf("the resume after a restart started at %q; the ticket's session must stay at high", eff)
	}
	if n := len(g.calls(t)); n != 1 {
		t.Fatalf("the guard was asked %d times; a resume must use the stored decision", n)
	}
	rec := e2.sessions.GetOrCreateActive(edenTwo).FlosendoEffortRecord()
	if rec == nil || rec.Effort != "high" || rec.From != "new" {
		t.Fatalf("the stored record after the restart: %+v", rec)
	}
	waitTurnDone(t, e2, edenTwo) // as in DoesNotLeak: no /new while the resumed turn is starting
	kickoff(t, e2, edenTwo, "/new", clearedKickof)
	if eff, _ := agent2.waitStart(t, 2); eff != "" {
		t.Fatalf("a plain /new after the restart started at %q", eff)
	}
	waitTurnDone(t, e2, edenTwo) // return with no turn writing the session store inside t.TempDir()
}

// [5] when unsure, the default (D5): a refusal, an error, a timeout, a missing or unset guard; each said in the channel.
func TestFlosendoEffort_RefusalsStartAtTheDefault(t *testing.T) {
	prev := effortGuardTimeout
	effortGuardTimeout = 500 * time.Millisecond
	t.Cleanup(func() { effortGuardTimeout = prev })
	missing := filepath.Join(t.TempDir(), "no-such-guard.py")
	cases := []struct {
		name, guardPath, reply string
	}{
		{"the guard refuses", writeGuard(t, 1, "EDEN-4242 has no effort:high-ok label", "").path,
			"effort high refused: EDEN-4242 has no effort:high-ok label; this session runs at the default"},
		{"the guard errors with no reason", writeGuard(t, 2, "", "").path, "effort high refused: the effort guard failed (exit 2)"},
		{"the guard hangs", writeGuard(t, 0, "allowed", "5").path, "effort high refused: the effort guard timed out"},
		{"the guard is missing", missing, "effort high refused: the effort guard is missing"},
		{"no guard is configured", "", "effort high refused: no effort guard is configured"},
		{"the guard is a folder", t.TempDir(), "effort high refused: the effort guard is missing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, agent, p := newEffortEngine(t, "", c.guardPath)
			kickoff(t, e, edenTwo, "/new effort=high", clearedKickof)
			if eff, _ := agent.waitStart(t, 1); eff != "" {
				t.Fatalf("started at %q; a refusal starts at the default", eff)
			}
			if len(sentContaining(p, c.reply)) != 1 {
				t.Fatalf("want one reply %q, got %q", c.reply, p.getSent())
			}
			rec := e.sessions.GetOrCreateActive(edenTwo).FlosendoEffortRecord()
			if rec == nil || rec.From != "refused" || rec.Effort != "" {
				t.Fatalf("the stored decision: %+v", rec)
			}
		})
	}
}

// [6] the guard is asked only for a lower effort, once, with the session key, the effort and the first message; the
// log never carries the message or the guard's stderr.
func TestFlosendoEffort_GuardAskedOnlyWhenNeeded(t *testing.T) {
	g := writeGuard(t, 0, "allowed", "")
	buf := captureInfoLog(t)
	e, agent, p := newEffortEngine(t, "", g.path)
	n := 0
	for _, cmd := range []string{"/new", "/new effort=xhigh", "/new effort=max"} {
		kickoff(t, e, edenTwo, cmd, clearedKickof)
		n++
		agent.waitStart(t, n)
		waitTurnDone(t, e, edenTwo) // the next /new (here and below) never lands while this turn is starting
	}
	if c := len(g.calls(t)); c != 0 {
		t.Fatalf("the guard was asked %d time(s) for none, xhigh and max", c)
	}
	kickoff(t, e, edenTwo, "/new effort=high", clearedKickof)
	n++
	agent.waitStart(t, n)
	message(e, p, edenTwo, "a second turn in the same session")
	calls := g.calls(t)
	if len(calls) != 1 {
		t.Fatalf("the guard was asked %d times for one high session; want once", len(calls))
	}
	want := map[string]string{"project": "eden", "session_key": edenTwo, "requested_effort": "high", "first_message": clearedKickof}
	for k, v := range want {
		if calls[0][k] != v {
			t.Fatalf("guard stdin %s = %q, want %q", k, calls[0][k], v)
		}
	}
	if !regexpLine(buf.String(), edenTwo, "effort=high effort_from=new") {
		t.Fatalf("no start line with effort=high effort_from=new:\n%s", buf.String())
	}
	for _, secret := range []string{"KICKOFF-SECRET-1759", "STDERR-SECRET-1759"} {
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("the log carries %s", secret)
		}
	}
	// The kickoff's echo is upstream's own reply (one per kickoff); nothing of ours repeats the message or stderr.
	if got := len(sentContaining(p, "KICKOFF-SECRET-1759")); got != n {
		t.Fatalf("%d replies carry the kickoff's text; want only the %d echoes", got, n)
	}
	if got := sentContaining(p, "STDERR-SECRET-1759"); len(got) != 0 {
		t.Fatalf("a reply carries the guard's stderr: %q", got)
	}
}

// failResumeAgent refuses every resume, as a claude that cannot find the transcript does; the engine then starts fresh.
type failResumeAgent struct{ effortAgent }

func (a *failResumeAgent) StartSession(ctx context.Context, id string) (AgentSession, error) {
	if id != "" {
		return nil, fmt.Errorf("no conversation found with session ID %s", id)
	}
	return a.effortAgent.StartSession(ctx, id)
}

// [4] the fresh start after a failed resume (getOrCreateInteractiveStateWith's fallback) keeps the session's effort.
func TestFlosendoEffort_ResumeFailureFallbackKeepsTheEffort(t *testing.T) {
	store := filepath.Join(t.TempDir(), "sessions.json")
	g := writeGuard(t, 0, "allowed", "")
	e1, agent1, _ := newEffortEngine(t, store, g.path)
	kickoff(t, e1, edenTwo, "/new effort=high", clearedKickof)
	agent1.waitStart(t, 1)
	deadline := time.Now().Add(5 * time.Second)
	for e1.sessions.GetOrCreateActive(edenTwo).GetAgentSessionID() == "" {
		if time.Now().After(deadline) {
			t.Fatal("the session never recorded its agent session id")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e1.sessions.Save()
	e1.cancel()

	agent2 := &failResumeAgent{}
	p2 := &reconstructingPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}
	e2 := NewEngine("eden", agent2, []Platform{p2}, store, LangEnglish)
	e2.SetEffortGuard(g.path)
	t.Cleanup(func() { e2.cancel() })
	message(e2, p2, edenTwo, "and the next step")
	if eff, resume := agent2.waitStart(t, 1); resume != "" || eff != "high" {
		t.Fatalf("the fresh start after a failed resume: effort %q (resume %q); want high", eff, resume)
	}
	waitTurnDone(t, e2, edenTwo) // as in KeptAcrossARestart: no turn writing the store after the test returns
}
