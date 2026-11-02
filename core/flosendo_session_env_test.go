package core

// EDEN-1789 (Ivan, 2 Nov 2026 05:52:55Z: "he is it so he also will be used to build stream A (eden-dev), while ivan
// will be doing orchestration and stream B (eden-two)"; 07:12Z: "the job description is really human + ai"): a
// channel is bound to a person's Claude seat. A session_env rule adds its env at every agent start whose session key
// is the rule's key, a thread of it (key + ":") or a cron run of it (key + "#"); every other start gets none.
// One test per item of the contract's failure list (comment a77f8bc9):
//   1. #eden-dev's start misses the env (exact, thread or cron key)
//   2. another channel gets it@ (a stale env after #eden-dev started; "slack:C0ADAUV66661")
//   4. /config reload does not apply or remove the rule (the engine half; the reload half: cmd/cc-connect)
//  15. rollback: no rule means no env for any key
//  16. the log line leaks a value

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

const (
	edenDev   = "slack:C0ADAUV6666"
	edenTwo   = "slack:C0ASQ5LPS4W"
	itSeatDir = "/home/ubuntu/.claude-it"
)

// startEnvAgent keeps the env each StartSession saw: what the claude process would get.
type startEnvAgent struct {
	stubAgent
	mu     sync.Mutex
	env    []string
	starts [][]string
}

func (a *startEnvAgent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.env = append([]string(nil), env...)
}

func (a *startEnvAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.starts = append(a.starts, append([]string(nil), a.env...))
	return &stubAgentSession{}, nil
}

// lastStartValue is the value the last start's process sees for name (the last duplicate wins, as in os/exec).
func (a *startEnvAgent) lastStartValue(t *testing.T, name string) (string, bool) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.starts) == 0 {
		t.Fatal("no agent start recorded")
	}
	val, found := "", false
	for _, kv := range a.starts[len(a.starts)-1] {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			val, found = v, true
		}
	}
	return val, found
}

func itSeatRule() []SessionEnvRule {
	return []SessionEnvRule{{SessionKey: edenDev, Env: map[string]string{"CLAUDE_CONFIG_DIR": itSeatDir}}}
}

func newSeatEngine(rules []SessionEnvRule) (*Engine, *startEnvAgent, Platform) {
	agent := &startEnvAgent{}
	p := &stubPlatformEngine{n: "slack"}
	e := NewEngine("eden", agent, []Platform{p}, "", LangEnglish)
	e.SetSessionEnvRules(rules)
	return e, agent, p
}

// start drives the engine's real start path: every Slack message, cron, timer and /new goes through it.
func start(e *Engine, p Platform, interactiveKey, ccKey string) {
	session := e.sessions.GetOrCreateActive(interactiveKey)
	e.getOrCreateInteractiveStateWith(interactiveKey, p, "ctx", session, e.sessions, nil, ccKey)
}

func TestFlosendoSessionEnv_EdenDevStartsOnItsSeat(t *testing.T) {
	cases := []struct{ name, interactiveKey, ccKey string }{
		{"the channel", edenDev, ""},
		{"a thread", edenDev + ":1730000000.000100", ""},
		{"a cron run (new_per_run)", edenDev + "#cron:a1b2c3", edenDev},
		{"a cron run keyed only by its own key", edenDev + "#cron:a1b2c3", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, agent, p := newSeatEngine(itSeatRule())
			start(e, p, c.interactiveKey, c.ccKey)
			got, ok := agent.lastStartValue(t, "CLAUDE_CONFIG_DIR")
			if !ok || got != itSeatDir {
				t.Fatalf("start %q (cc %q): CLAUDE_CONFIG_DIR = %q (set %v), want %q", c.interactiveKey, c.ccKey, got, ok, itSeatDir)
			}
			if cc, _ := agent.lastStartValue(t, "CC_SESSION_KEY"); cc == "" {
				t.Fatal("the engine's own CC_SESSION_KEY is gone: the rule must add to the session env, not replace it")
			}
		})
	}
}

func TestFlosendoSessionEnv_OtherChannelsKeepTheBridgeSeat(t *testing.T) {
	e, agent, p := newSeatEngine(itSeatRule())
	start(e, p, edenDev, "")
	if got, _ := agent.lastStartValue(t, "CLAUDE_CONFIG_DIR"); got != itSeatDir {
		t.Fatalf("precondition: #eden-dev started with %q", got)
	}
	for _, key := range []string{
		edenTwo,                     // #eden-two, right after #eden-dev started (a stale env would carry it@)
		"slack:C0B1FCC755W",         // the chief of staff
		"slack:C0ADAUV66661",        // the rule's key is a prefix, but not at a ":" boundary
		"slack:C0ADAUV666",          // shorter than the rule's key
		"slack:C0ADAUV6666x:thread", // the key plus other characters, then a thread
		"discord:" + edenDev[6:],    // the same id on another platform
		"workspace-a:" + edenTwo,    // a multi-workspace interactive key of another channel
	} {
		start(e, p, key, "")
		if got, ok := agent.lastStartValue(t, "CLAUDE_CONFIG_DIR"); ok {
			t.Fatalf("start %q got CLAUDE_CONFIG_DIR=%q; only #eden-dev's keys may", key, got)
		}
	}
}

func TestFlosendoSessionEnv_ReloadAppliesAndRemovesTheRule(t *testing.T) {
	e, agent, p := newSeatEngine(nil)
	start(e, p, edenDev, "")
	if got, ok := agent.lastStartValue(t, "CLAUDE_CONFIG_DIR"); ok {
		t.Fatalf("no rule yet, but the start got CLAUDE_CONFIG_DIR=%q", got)
	}

	e.SetSessionEnvRules(itSeatRule()) // "/config reload" with the line added
	start(e, p, edenDev+":t1", "")
	if got, _ := agent.lastStartValue(t, "CLAUDE_CONFIG_DIR"); got != itSeatDir {
		t.Fatalf("after the rule was set, CLAUDE_CONFIG_DIR = %q, want %q", got, itSeatDir)
	}

	e.SetSessionEnvRules(nil) // the rollback: the line commented out, "/config reload"
	start(e, p, edenDev+":t2", "")
	if got, ok := agent.lastStartValue(t, "CLAUDE_CONFIG_DIR"); ok {
		t.Fatalf("after the rule was removed, the start still got CLAUDE_CONFIG_DIR=%q", got)
	}
}

func TestFlosendoSessionEnv_LongestKeyWins(t *testing.T) {
	e, agent, p := newSeatEngine([]SessionEnvRule{
		{SessionKey: edenDev, Env: map[string]string{"CLAUDE_CONFIG_DIR": itSeatDir}},
		{SessionKey: edenDev + ":thread9", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/home/ubuntu/.claude-third"}},
	})
	start(e, p, edenDev+":thread9", "")
	if got, _ := agent.lastStartValue(t, "CLAUDE_CONFIG_DIR"); got != "/home/ubuntu/.claude-third" {
		t.Fatalf("the more specific rule must win, got %q", got)
	}
	start(e, p, edenDev+":thread8", "")
	if got, _ := agent.lastStartValue(t, "CLAUDE_CONFIG_DIR"); got != itSeatDir {
		t.Fatalf("another thread falls back to the channel's rule, got %q", got)
	}
}

func TestFlosendoSessionEnv_RelayStartTakesTheSourceKeysRule(t *testing.T) {
	e := newTestEngine()
	agent := &sessionEnvRecordingAgent{session: newResultAgentSession("ok")}
	e.agent = agent
	e.SetSessionEnvRules(itSeatRule())
	if _, err := e.HandleRelay(context.Background(), "source", edenDev, "hello"); err != nil {
		t.Fatalf("HandleRelay: %v", err)
	}
	if got := agent.EnvValue("CLAUDE_CONFIG_DIR"); got != itSeatDir {
		t.Fatalf("relay from #eden-dev: CLAUDE_CONFIG_DIR = %q, want %q", got, itSeatDir)
	}
	agent.session = newResultAgentSession("ok") // a fake session answers once; the second relay starts its own
	if _, err := e.HandleRelay(context.Background(), "source", edenTwo, "hello"); err != nil {
		t.Fatalf("HandleRelay: %v", err)
	}
	if got := agent.EnvValue("CLAUDE_CONFIG_DIR"); got != "" {
		t.Fatalf("relay from #eden-two: CLAUDE_CONFIG_DIR = %q, want none", got)
	}
}

func captureInfoLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestFlosendoSessionEnv_LogNamesTheSeatNeverAValue(t *testing.T) {
	const ruleSeat, bridgeSeat = "/tmp/seat-1789-rule", "/tmp/seat-1789-bridge"
	t.Setenv("CLAUDE_CONFIG_DIR", bridgeSeat) // the bridge's own seat (the service env), not this box's
	buf := captureInfoLog(t)
	e, _, p := newSeatEngine([]SessionEnvRule{{SessionKey: edenDev, Env: map[string]string{
		"CLAUDE_CONFIG_DIR": ruleSeat,
		"EXTRA_API_TOKEN":   "s3cret-value-1789",
	}}})
	start(e, p, edenDev, "")
	start(e, p, edenTwo, "")
	out := buf.String()
	if strings.Contains(out, "s3cret-value-1789") {
		t.Fatalf("a rule's value reached the log:\n%s", out)
	}
	// One line per start, naming its key and the seat it runs on: the per-session account record EDEN-1782 reads.
	lineFor := func(key string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "flosendo: agent start") && strings.Contains(l, "session_key="+key+" ") {
				return l
			}
		}
		t.Fatalf("no agent start line for %s:\n%s", key, out)
		return ""
	}
	dev, two := lineFor(edenDev), lineFor(edenTwo)
	for _, want := range []string{"EXTRA_API_TOKEN", "CLAUDE_CONFIG_DIR", "claude_config_dir=" + ruleSeat} {
		if !strings.Contains(dev, want) {
			t.Fatalf("#eden-dev's start line does not name %q: %s", want, dev)
		}
	}
	if !strings.Contains(two, "claude_config_dir="+bridgeSeat) || strings.Contains(two, ruleSeat) {
		t.Fatalf("#eden-two's start line must name the bridge's seat only: %s", two)
	}
}

// Review round 1 (Opus, EDEN-1789): the agent's session env is agent-wide and holds the last start's env, so a /usage
// probe built from it reported the seat of whichever channel started an agent last. /usage now hands the agent the
// asking channel's own rule env (possibly none).
type usageSeatAgent struct {
	stubAgent
	mu   sync.Mutex
	seen [][]string
	ok   []bool
}

func (a *usageSeatAgent) GetUsage(ctx context.Context) (*UsageReport, error) {
	env, ok := SessionEnvFromContext(ctx)
	a.mu.Lock()
	a.seen, a.ok = append(a.seen, env), append(a.ok, ok)
	a.mu.Unlock()
	return &UsageReport{}, nil
}

func TestFlosendoSessionEnv_UsageRunsOnTheAskingChannelsSeat(t *testing.T) {
	agent := &usageSeatAgent{}
	p := &stubPlatformEngine{n: "slack"}
	e := NewEngine("eden", agent, []Platform{p}, "", LangEnglish)
	e.SetSessionEnvRules(itSeatRule())
	e.cmdUsage(p, &Message{SessionKey: edenDev, ReplyCtx: "ctx"})
	e.cmdUsage(p, &Message{SessionKey: edenTwo, ReplyCtx: "ctx"})
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.seen) != 2 || !agent.ok[0] || !agent.ok[1] {
		t.Fatalf("/usage did not say whose seat to probe: %d calls, marked %v", len(agent.seen), agent.ok)
	}
	if got := strings.Join(agent.seen[0], " "); got != "CLAUDE_CONFIG_DIR="+itSeatDir {
		t.Fatalf("/usage in #eden-dev probes %q, want it@'s seat", got)
	}
	if len(agent.seen[1]) != 0 {
		t.Fatalf("/usage in #eden-two probes %q, want the bridge's own seat (no rule)", agent.seen[1])
	}
}
