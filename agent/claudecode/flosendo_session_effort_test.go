package claudecode

// EDEN-1759 (failure list items 1-4, the agent's half): the real StartSession with a fake `claude` binary. A start's
// effort comes from its context (one session's, asked for at /new) and reaches the process as --effort; a start
// without one passes no --effort (settings' xhigh decides), whatever the start before it was given; xhigh is Claude
// Code's own level and is no longer dropped (normalizeEffort returned "" for it, so a config or preset at xhigh meant
// "no flag"); a resume keeps the effort it was given.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func fakeClaudeAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Agent{model: "claude-opus-5-5", activeIdx: -1, workDir: dir, cmd: bin, ccDataDir: dir}
}

// effortArg starts a session and returns its --effort value ("" when there is none) and whether it resumed id.
func effortArg(t *testing.T, a *Agent, ctx context.Context, id string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sess, err := a.StartSession(ctx, id)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()
	args := sess.(*claudeSession).cmd.Args
	effort, resumed, n := "", false, 0
	for i, arg := range args {
		if arg == "--effort" && i+1 < len(args) {
			effort, n = args[i+1], n+1
		}
		if arg == "--resume" && i+1 < len(args) && args[i+1] == id {
			resumed = true
		}
	}
	if n > 1 {
		t.Fatalf("--effort given %d times: %q", n, args)
	}
	return effort, resumed
}

func TestFlosendoEffort_StartTakesTheContextsEffortOnly(t *testing.T) {
	a := fakeClaudeAgent(t)
	if got, _ := effortArg(t, a, context.Background(), ""); got != "" {
		t.Fatalf("a start without an effort passed --effort %q; settings must decide", got)
	}
	if got, _ := effortArg(t, a, core.WithSessionEffort(context.Background(), "high"), ""); got != "high" {
		t.Fatalf("a start with effort high passed --effort %q", got)
	}
	if got, _ := effortArg(t, a, context.Background(), ""); got != "" {
		t.Fatalf("the start after a high one passed --effort %q: the effort leaked into the agent", got)
	}
	if got, _ := effortArg(t, a, core.WithSessionEffort(context.Background(), "xhigh"), ""); got != "xhigh" {
		t.Fatalf("effort xhigh passed --effort %q (normalizeEffort must keep xhigh)", got)
	}
	got, resumed := effortArg(t, a, core.WithSessionEffort(context.Background(), "high"), "sess-1759")
	if !resumed || got != "high" {
		t.Fatalf("a resume with effort high: resumed=%v --effort %q", resumed, got)
	}
}

func TestFlosendoEffort_XhighIsAnEffort(t *testing.T) {
	a := fakeClaudeAgent(t)
	a.SetReasoningEffort("xhigh") // a config's reasoning_effort, or a preset at xhigh
	if got, _ := effortArg(t, a, context.Background(), ""); got != "xhigh" {
		t.Fatalf("reasoning_effort xhigh passed --effort %q", got)
	}
}
