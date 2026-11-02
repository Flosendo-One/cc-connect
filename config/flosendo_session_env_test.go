package config

// EDEN-1789, contract a77f8bc9 item 3: "the config line is malformed or misplaced and silently ignored". A good
// [[projects.session_env]] entry loads; each malformed one fails the load (and so the reload, which keeps the old
// rules) with a message naming it. Item 15: a binary without the field (the rollback, v1.10-flosendo.5) still loads
// the file.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// liveShapeTOML is the shape of the box's config.toml: one project, one Slack app, three channels.
const liveShapeTOML = `
[[projects]]
  name = "eden"

  [projects.agent]
    type = "claudecode"

    [projects.agent.options]
      mode = "bypassPermissions"
      work_dir = "/home/ubuntu/eden"

  [[projects.platforms]]
    type = "slack"

    [projects.platforms.options]
      share_session_in_channel = true
`

const itSeatRuleTOML = `
  [[projects.session_env]]
    session_key = "slack:C0ADAUV6666"
    env = { CLAUDE_CONFIG_DIR = "/home/ubuntu/.claude-it" }
`

func loadFixture(t *testing.T, content string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestFlosendoSessionEnv_ConfigLoadsTheRule(t *testing.T) {
	cfg, err := loadFixture(t, liveShapeTOML+itSeatRuleTOML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rules := cfg.Projects[0].SessionEnv
	if len(rules) != 1 || rules[0].SessionKey != "slack:C0ADAUV6666" || rules[0].Env["CLAUDE_CONFIG_DIR"] != "/home/ubuntu/.claude-it" {
		t.Fatalf("session_env = %+v", rules)
	}
}

func TestFlosendoSessionEnv_ConfigRefusesMalformedRules(t *testing.T) {
	rule := func(key, env string) string {
		return "\n  [[projects.session_env]]\n    session_key = " + key + "\n    env = " + env + "\n"
	}
	cases := map[string]struct{ toml, want string }{
		"no key":             {"\n  [[projects.session_env]]\n    env = { CLAUDE_CONFIG_DIR = \"/a\" }\n", "session_key"},
		"no colon":           {rule(`"slack"`, `{ CLAUDE_CONFIG_DIR = "/a" }`), "session_key"},
		"trailing colon":     {rule(`"slack:C0ADAUV6666:"`, `{ CLAUDE_CONFIG_DIR = "/a" }`), "must not end"},
		"trailing hash":      {rule(`"slack:C0ADAUV6666#"`, `{ CLAUDE_CONFIG_DIR = "/a" }`), "must not end"},
		"space in key":       {rule(`"slack:C0ADAUV6666 "`, `{ CLAUDE_CONFIG_DIR = "/a" }`), "session_key"},
		"key twice":          {itSeatRuleTOML + itSeatRuleTOML, "appears twice"},
		"no env":             {rule(`"slack:C0ADAUV6666"`, `{}`), "env is empty"},
		"lower-case name":    {rule(`"slack:C0ADAUV6666"`, `{ claude_config_dir = "/a" }`), "must match"},
		"PATH":               {rule(`"slack:C0ADAUV6666"`, `{ PATH = "/tmp" }`), "must not set PATH"},
		"HOME":               {rule(`"slack:C0ADAUV6666"`, `{ HOME = "/tmp" }`), "must not set HOME"},
		"a cc-connect name":  {rule(`"slack:C0ADAUV6666"`, `{ CC_SESSION_KEY = "slack:C0B1FCC755W" }`), "CC_*"},
		"relative seat":      {rule(`"slack:C0ADAUV6666"`, `{ CLAUDE_CONFIG_DIR = ".claude-it" }`), "absolute"},
		"newline in a value": {rule(`"slack:C0ADAUV6666"`, `{ CLAUDE_CONFIG_DIR = "/a\nb" }`), "control character"},
		"a table, not a list of tables": {
			"\n  [projects.session_env]\n    session_key = \"slack:C0ADAUV6666\"\n", "session_env"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadFixture(t, liveShapeTOML+c.toml)
			if err == nil {
				t.Fatalf("Load accepted a malformed rule:\n%s", c.toml)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not name %q", err, c.want)
			}
		})
	}
}

// The default seat named as a rule: with CLAUDE_CONFIG_DIR set, Claude reads $CLAUDE_CONFIG_DIR/.claude.json, so a
// rule naming ~/.claude would move that channel off ~/.claude.json (its MCP servers, its onboarding) without a word.
func TestFlosendoSessionEnv_ConfigRefusesTheDefaultSeat(t *testing.T) {
	for _, dir := range []string{"/.claude", "/.claude/", "/./.claude"} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		path := filepath.Join(home, "config.toml")
		rule := "\n  [[projects.session_env]]\n    session_key = \"slack:C0ASQ5LPS4W\"\n    env = { CLAUDE_CONFIG_DIR = \"" + home + dir + "\" }\n"
		if err := os.WriteFile(path, []byte(liveShapeTOML+rule), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), "default seat") {
			t.Fatalf("CLAUDE_CONFIG_DIR=%s%s: err = %v, want a refusal naming the default seat", home, dir, err)
		}
	}
	if _, err := loadFixture(t, liveShapeTOML+itSeatRuleTOML); err != nil {
		t.Fatalf("another seat folder is fine: %v", err)
	}
}

// The rollback binary (v1.10-flosendo.5) has no SessionEnv field. BurntSushi's Unmarshal ignores a key it has no
// field for, so the old bridge loads the file with the line still in it and simply applies no rule.
func TestFlosendoSessionEnv_ABinaryWithoutTheFieldIgnoresIt(t *testing.T) {
	type oldProject struct {
		Name string `toml:"name"`
	}
	type oldConfig struct {
		Projects []oldProject `toml:"projects"`
	}
	var old oldConfig
	if err := toml.Unmarshal([]byte(liveShapeTOML+itSeatRuleTOML), &old); err != nil {
		t.Fatalf("a binary without the field fails on the line: %v", err)
	}
	if len(old.Projects) != 1 || old.Projects[0].Name != "eden" {
		t.Fatalf("decoded %+v", old)
	}
}
