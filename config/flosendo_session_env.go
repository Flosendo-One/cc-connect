package config

// Flosendo fork (EDEN-1789): [[projects.session_env]], a channel bound to a person's Claude seat. Example:
//
//	[[projects.session_env]]
//	  session_key = "slack:C0ADAUV6666"                     # #eden-dev; covers its threads and cron runs too
//	  env = { CLAUDE_CONFIG_DIR = "/home/ubuntu/.claude-it" } # it@'s seat
//
// Hot-reloaded by /config reload. A malformed entry fails the load (and the reload, which then keeps the old rules),
// never silently does nothing.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SessionEnvConfig is one per-session-key env rule.
type SessionEnvConfig struct {
	SessionKey string            `toml:"session_key"`
	Env        map[string]string `toml:"env"`
}

var sessionEnvNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func validateSessionEnv(prefix string, rules []SessionEnvConfig) error {
	seen := make(map[string]bool, len(rules))
	for i, r := range rules {
		at := fmt.Sprintf("config: %s.session_env[%d]", prefix, i)
		key := r.SessionKey
		platform, id, ok := strings.Cut(key, ":")
		if key == "" || strings.TrimSpace(key) != key || strings.ContainsAny(key, " \t\r\n") || !ok || platform == "" || id == "" {
			return fmt.Errorf("%s.session_key %q must be a session key such as \"slack:C0123\" (platform, a colon, an id)", at, key)
		}
		if strings.HasSuffix(key, ":") || strings.HasSuffix(key, "#") {
			return fmt.Errorf("%s.session_key %q must not end with %q", at, key, key[len(key)-1:])
		}
		if seen[key] {
			return fmt.Errorf("%s.session_key %q appears twice", at, key)
		}
		seen[key] = true
		if len(r.Env) == 0 {
			return fmt.Errorf("%s.env is empty (session %q)", at, key)
		}
		for name, val := range r.Env {
			switch {
			case !sessionEnvNameRe.MatchString(name):
				return fmt.Errorf("%s.env name %q must match [A-Z_][A-Z0-9_]*", at, name)
			case dangerousEnvVars[name]:
				return fmt.Errorf("%s.env must not set %s", at, name)
			case strings.HasPrefix(name, "CC_"):
				return fmt.Errorf("%s.env must not set %s: CC_* names belong to cc-connect", at, name)
			case strings.ContainsAny(val, "\x00\r\n"):
				return fmt.Errorf("%s.env %s holds a control character", at, name)
			case name == "CLAUDE_CONFIG_DIR" && !filepath.IsAbs(val):
				return fmt.Errorf("%s.env CLAUDE_CONFIG_DIR must be an absolute path (a relative one resolves against the work dir)", at)
			case name == "CLAUDE_CONFIG_DIR" && isDefaultClaudeDir(val):
				return fmt.Errorf("%s.env CLAUDE_CONFIG_DIR names the default seat (~/.claude): leave %q without an entry; set, "+
					"Claude reads ~/.claude/.claude.json instead of ~/.claude.json and loses that config's MCP servers", at, key)
			}
		}
	}
	return nil
}

// isDefaultClaudeDir reports whether dir is ~/.claude, Claude Code's own folder when CLAUDE_CONFIG_DIR is unset.
func isDefaultClaudeDir(dir string) bool {
	home, err := os.UserHomeDir()
	return err == nil && home != "" && filepath.Clean(dir) == filepath.Join(home, ".claude")
}
