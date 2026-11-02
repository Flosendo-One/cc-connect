package config

// EDEN-1759: [[projects]] effort_guard, the script asked before a /new effort=<v> session starts below the default. An
// absolute path loads; a relative one (it would resolve against the bridge's work dir) fails the load, and so a reload,
// which then keeps the old guard. No line is valid: every lower effort is then refused (core's tests).

import (
	"strings"
	"testing"
)

func TestFlosendoEffortGuard_ConfigLoadsAnAbsolutePathOnly(t *testing.T) {
	withGuard := func(v string) string {
		return strings.Replace(liveShapeTOML, `name = "eden"`, `name = "eden"`+"\n  effort_guard = "+v, 1)
	}
	cfg, err := loadFixture(t, withGuard(`"/home/ubuntu/eden/scripts/ops/effort-guard/effort_guard.py"`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Projects[0].EffortGuard; got != "/home/ubuntu/eden/scripts/ops/effort-guard/effort_guard.py" {
		t.Fatalf("effort_guard = %q", got)
	}
	if _, err := loadFixture(t, withGuard(`"scripts/ops/effort-guard/effort_guard.py"`)); err == nil ||
		!strings.Contains(err.Error(), "effort_guard must be an absolute path") {
		t.Fatalf("a relative effort_guard loaded (err %v)", err)
	}
	cfg, err = loadFixture(t, liveShapeTOML)
	if err != nil || cfg.Projects[0].EffortGuard != "" {
		t.Fatalf("no line: err %v, effort_guard %q", err, cfg.Projects[0].EffortGuard)
	}
}
