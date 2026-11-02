package main

// EDEN-1789, contract a77f8bc9 item 4: "/config reload does not apply or remove the rule". The real reloadConfig on a
// real config file: adding the line applies it, removing it removes it, and a malformed line is refused with the old
// rules kept (a reload that fails changes nothing).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

const reloadBaseTOML = `
[[projects]]
  name = "eden"
  [projects.agent]
    type = "claudecode"
    [projects.agent.options]
      work_dir = "/home/ubuntu/eden"
  [[projects.platforms]]
    type = "slack"
    [projects.platforms.options]
      share_session_in_channel = true
`

const reloadRuleTOML = `
  [[projects.session_env]]
    session_key = "slack:C0ADAUV6666"
    env = { CLAUDE_CONFIG_DIR = "/home/ubuntu/.claude-it" }
`

func TestFlosendoSessionEnv_ConfigReloadAppliesAndRemovesTheRule(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, "config.toml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := core.NewEngine("eden", nil, nil, "", core.LangEnglish)
	seat := func() string { return strings.Join(e.FlosendoSessionEnvFor("slack:C0ADAUV6666"), " ") }

	write(reloadBaseTOML + reloadRuleTOML)
	if _, err := reloadConfig(path, "eden", e); err != nil {
		t.Fatalf("reload with the line: %v", err)
	}
	if got := seat(); got != "CLAUDE_CONFIG_DIR=/home/ubuntu/.claude-it" {
		t.Fatalf("after the reload that adds the line, #eden-dev's env is %q", got)
	}

	write(reloadBaseTOML + "\n  [[projects.session_env]]\n    session_key = \"slack\"\n    env = { CLAUDE_CONFIG_DIR = \"/x\" }\n")
	if _, err := reloadConfig(path, "eden", e); err == nil {
		t.Fatal("a malformed line was accepted by the reload")
	}
	if got := seat(); got != "CLAUDE_CONFIG_DIR=/home/ubuntu/.claude-it" {
		t.Fatalf("a refused reload changed the rules: %q", got)
	}

	write(reloadBaseTOML)
	if _, err := reloadConfig(path, "eden", e); err != nil {
		t.Fatalf("reload without the line: %v", err)
	}
	if got := seat(); got != "" {
		t.Fatalf("after the reload that removes the line (the rollback), #eden-dev's env is still %q", got)
	}
}
