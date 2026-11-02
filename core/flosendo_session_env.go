package core

// Flosendo fork (EDEN-1789): a channel bound to a person's Claude seat. A project's session_env rules add env to the
// agent of the sessions they name, so #eden-dev's agent can run with CLAUDE_CONFIG_DIR=~/.claude-it (it@'s seat)
// while the other channels keep the bridge's own. Kept in this file so an upstream merge meets only the two calls in
// engine.go (getOrCreateInteractiveStateWith and HandleRelay, the only places an agent session starts).

import (
	"context"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
)

// SessionEnvRule is one [[projects.session_env]] entry: the env added to every agent start of SessionKey, of a thread
// of it (SessionKey + ":...") and of a cron run of it (SessionKey + "#cron:...").
type SessionEnvRule struct {
	SessionKey string
	Env        map[string]string
}

// sessionEnvSeatVar is the one value the log may name: a seat's config folder is where its transcripts and sign-in
// live, the per-session account record EDEN-1782 reads. Every other value stays out of the log.
const sessionEnvSeatVar = "CLAUDE_CONFIG_DIR"

type sessionEnvRules struct {
	mu    sync.RWMutex
	rules []SessionEnvRule
}

// SetSessionEnvRules replaces the rules (at start and at every /config reload); nil removes them all. The engine
// keeps its own copy, so the caller's maps can change afterwards.
func (e *Engine) SetSessionEnvRules(rules []SessionEnvRule) {
	cp := make([]SessionEnvRule, 0, len(rules))
	summary := make([]string, 0, len(rules))
	for _, r := range rules {
		env := make(map[string]string, len(r.Env))
		for k, v := range r.Env {
			env[k] = v
		}
		cp = append(cp, SessionEnvRule{SessionKey: r.SessionKey, Env: env})
		summary = append(summary, r.SessionKey+"["+strings.Join(sortedEnvNames(env), ",")+"]")
	}
	e.flosendoSessionEnv.mu.Lock()
	e.flosendoSessionEnv.rules = cp
	e.flosendoSessionEnv.mu.Unlock()
	slog.Info("flosendo: session env rules set", "project", e.name, "count", len(cp), "rules", strings.Join(summary, " "))
}

// sessionKeyCovers reports whether a rule for ruleKey covers a session key: the key itself, a thread of it (":") or a
// cron run of it ("#cron:..."). A bare prefix does not: "slack:C1" must not cover "slack:C12".
func sessionKeyCovers(ruleKey, key string) bool {
	if ruleKey == "" || !strings.HasPrefix(key, ruleKey) {
		return false
	}
	rest := key[len(ruleKey):]
	return rest == "" || rest[0] == ':' || rest[0] == '#'
}

// FlosendoSessionEnvFor returns the env a start of this session key (its CC_SESSION_KEY: the channel, a thread of it,
// or for a cron run the channel or thread it replies in) gets, as sorted "NAME=value" entries: the rule with the
// longest key that covers it, or none.
func (e *Engine) FlosendoSessionEnvFor(key string) []string {
	e.flosendoSessionEnv.mu.RLock()
	defer e.flosendoSessionEnv.mu.RUnlock()
	var best *SessionEnvRule
	for i := range e.flosendoSessionEnv.rules {
		r := &e.flosendoSessionEnv.rules[i]
		if sessionKeyCovers(r.SessionKey, key) && (best == nil || len(r.SessionKey) > len(best.SessionKey)) {
			best = r
		}
	}
	if best == nil {
		return nil
	}
	names := sortedEnvNames(best.Env)
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, n+"="+best.Env[n])
	}
	return out
}

// flosendoStartEnv is what each start site appends to its session env, and its log line: one per agent start, naming
// the session key, the rule's env names and the seat folder the agent runs on (the rule's, else the bridge's own),
// and the session's effort (EDEN-1759: its value or "default", and from "new", "refused" or "none"; nil for a relay),
// never another value.
func (e *Engine) flosendoStartEnv(key string, session *Session) []string {
	env := e.FlosendoSessionEnvFor(key)
	names := make([]string, 0, len(env))
	seat := ""
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		names = append(names, k)
		if k == sessionEnvSeatVar {
			seat = v
		}
	}
	source := "rule"
	if len(env) == 0 || seat == "" {
		source = "bridge"
		seat = bridgeSeatDir()
	}
	effort, effortFrom := startEffort(session)
	slog.Info("flosendo: agent start", "project", e.name, "session_key", key,
		"env_names", strings.Join(names, ","), "claude_config_dir", seat, "seat_from", source,
		"effort", effortOrDefault(effort), "effort_from", effortFrom)
	return env
}

// bridgeSeatDir is the seat a start with no CLAUDE_CONFIG_DIR rule runs on: the bridge's own env, else "~/.claude"
// (Claude Code's default; unset also means ~/.claude.json, not ~/.claude/.claude.json).
func bridgeSeatDir() string {
	if v := os.Getenv(sessionEnvSeatVar); v != "" {
		return v
	}
	return "~/.claude (default)"
}

func sortedEnvNames(env map[string]string) []string {
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

type sessionEnvCtxKey struct{}

// WithSessionEnv marks ctx with the asking session's rule env (possibly none), for work an agent runs outside a
// session start: the /usage probe must run on the asking channel's seat, not on the seat of the agent's last start.
func WithSessionEnv(ctx context.Context, env []string) context.Context {
	return context.WithValue(ctx, sessionEnvCtxKey{}, append([]string{}, env...))
}

// SessionEnvFromContext returns the env WithSessionEnv put in ctx, and whether it was put there at all.
func SessionEnvFromContext(ctx context.Context) ([]string, bool) {
	env, ok := ctx.Value(sessionEnvCtxKey{}).([]string)
	return env, ok
}
