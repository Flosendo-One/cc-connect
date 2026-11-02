package main

// EDEN-1759: the real reloadConfig sets the engine's effort guard from [[projects]] effort_guard, a removed line
// removes it (every lower effort is then refused), and a malformed line is refused with the old guard kept.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestFlosendoEffortGuard_ConfigReloadSetsAndRemovesIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, "config.toml")
	write := func(guard string) {
		t.Helper()
		s := reloadBaseTOML
		if guard != "" {
			s = strings.Replace(s, `name = "eden"`, `name = "eden"`+"\n  effort_guard = \""+guard+"\"", 1)
		}
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const guard = "/home/ubuntu/eden/scripts/ops/effort-guard/effort_guard.py"
	e := core.NewEngine("eden", nil, nil, "", core.LangEnglish)

	write(guard)
	if _, err := reloadConfig(path, "eden", e); err != nil {
		t.Fatalf("reload with the line: %v", err)
	}
	if got := e.EffortGuard(); got != guard {
		t.Fatalf("after the reload that adds the line, the guard is %q", got)
	}
	write("relative/effort_guard.py")
	if _, err := reloadConfig(path, "eden", e); err == nil {
		t.Fatal("a relative effort_guard was accepted by the reload")
	}
	if got := e.EffortGuard(); got != guard {
		t.Fatalf("a refused reload changed the guard: %q", got)
	}
	write("")
	if _, err := reloadConfig(path, "eden", e); err != nil {
		t.Fatalf("reload without the line: %v", err)
	}
	if got := e.EffortGuard(); got != "" {
		t.Fatalf("after the reload that removes the line, the guard is still %q", got)
	}
}
