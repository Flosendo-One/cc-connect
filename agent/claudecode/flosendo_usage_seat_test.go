package claudecode

// EDEN-1789 review round 1 (Opus): the /usage probe must run on the asking channel's Claude seat. a.sessionEnv holds
// the env of the last agent start, whichever channel that was; a /usage from #eden-two right after an #eden-dev start
// reported it@'s quota. The real GetUsage runs a fake `claude` that writes the env it was given and exits.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func probeSeat(t *testing.T, a *Agent, ctx context.Context) (string, bool) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	// It stays up like the real CLI: a probe whose claude exits at once can block in upstream's deferred <-readDone.
	script := "#!/bin/sh\nenv > \"$FAKE_CLAUDE_ENV_OUT\"\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_ENV_OUT", out)
	t.Setenv("CLAUDE_CONFIG_DIR", "") // the bridge's own seat: none (ivan@, ~/.claude)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _ = a.GetUsage(ctx) // the fake prints no usage screen: an error is expected; the env it saw is the point
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the fake claude did not run: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "CLAUDE_CONFIG_DIR="); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

func TestFlosendoUsageProbe_RunsOnTheAskingSeat(t *testing.T) {
	lastStart := []string{"CC_SESSION_KEY=slack:C0ADAUV6666", "CLAUDE_CONFIG_DIR=/home/ubuntu/.claude-it"} // #eden-dev started last
	a := &Agent{sessionEnv: append([]string(nil), lastStart...), activeIdx: -1}

	if seat, ok := probeSeat(t, a, core.WithSessionEnv(context.Background(), nil)); ok {
		t.Fatalf("/usage from #eden-two (no rule) probed seat %q, the last start's; want the bridge's own", seat)
	}
	if seat, _ := probeSeat(t, a, core.WithSessionEnv(context.Background(), []string{"CLAUDE_CONFIG_DIR=/home/ubuntu/.claude-it"})); seat != "/home/ubuntu/.claude-it" {
		t.Fatalf("/usage from #eden-dev probed %q, want it@'s seat", seat)
	}
	if strings.Join(a.sessionEnv, " ") != strings.Join(lastStart, " ") {
		t.Fatalf("the probe changed the agent's session env: %q", a.sessionEnv)
	}
}
