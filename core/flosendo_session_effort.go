package core

// Flosendo fork (EDEN-1759): one session's effort, asked for at /new. Ivan, 31 Oct 2026: "for the build, try to find a
// balance between quality of using extra high and high of opus 5.5 but first get a baseline too".
//
//	cc-connect send --session-cmd "/new effort=high" --message "<kickoff>"
//
// stores "high" on the new session. Its first start asks the project's effort guard (effort_guard, an eden script)
// whether this session may run at it, and keeps the answer with the session, so a resume (a bridge restart, a recycle,
// a rate-limit retry) starts at the same effort without asking again. The effort reaches the agent in StartSession's
// context, for that one start only: the agent's own setting is never changed, so no other session can inherit it.
// A /new without effort= stores nothing, and the agent starts exactly as before (settings' xhigh). Only an effort
// below the default is asked about; xhigh and max start as asked. Any refusal, error, timeout, missing or unset guard
// starts at the default and says so in the channel: when unsure, the default (EDEN-1759 D5).
// Kept in this file so an upstream merge meets only the calls in engine.go and session.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"
)

// SessionEffort is one session's effort, stored with it (sessions.json "flosendo_effort").
type SessionEffort struct {
	Requested string `json:"requested"`        // as asked at /new
	Effort    string `json:"effort,omitempty"` // as decided: "" is the default (no --effort)
	From      string `json:"from,omitempty"`   // "new" (as asked) or "refused" (the default); "" not decided yet
	Reason    string `json:"reason,omitempty"` // why refused: the guard's first line, or ours
}

const (
	effortFromNew     = "new"
	effortFromRefused = "refused"
)

// knownEfforts are Claude Code's --effort values. needsGuard: below the default (xhigh), so the guard decides.
var knownEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

func effortNeedsGuard(e string) bool { return e == "low" || e == "medium" || e == "high" }

// effortGuardTimeout bounds one guard run (a Linear read); a test shortens it.
var effortGuardTimeout = 10 * time.Second

type effortGuardConfig struct {
	mu   sync.RWMutex
	path string
}

// SetEffortGuard sets the project's effort guard (at start and at every /config reload); "" removes it, and every
// effort below the default is then refused.
func (e *Engine) SetEffortGuard(path string) {
	e.flosendoEffortGuard.mu.Lock()
	e.flosendoEffortGuard.path = path
	e.flosendoEffortGuard.mu.Unlock()
	slog.Info("flosendo: effort guard set", "project", e.name, "path", path)
}

// EffortGuard is the guard's path as set ("" none), for the reload test and /status-like reads.
func (e *Engine) EffortGuard() string { return e.effortGuardPath() }

func (e *Engine) effortGuardPath() string {
	e.flosendoEffortGuard.mu.RLock()
	defer e.flosendoEffortGuard.mu.RUnlock()
	return e.flosendoEffortGuard.path
}

func (v *SessionEffort) clone() *SessionEffort {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// FlosendoEffortRecord returns a copy of the session's effort record, or nil when none was asked for.
func (s *Session) FlosendoEffortRecord() *SessionEffort {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.FlosendoEffort.clone()
}

func (s *Session) setFlosendoEffort(v *SessionEffort) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.FlosendoEffort = v.clone()
}

// parseNewEffort takes effort=<v> out of /new's arguments (case-insensitive). It returns the effort, the rest (the
// session name), and the value when it is not an effort or effort= was given twice.
func parseNewEffort(args []string) (effort string, rest []string, bad string) {
	seen := 0
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || !strings.EqualFold(k, "effort") {
			rest = append(rest, a)
			continue
		}
		seen++
		effort = strings.ToLower(strings.TrimSpace(v))
	}
	switch {
	case seen > 1:
		return "", rest, "given twice"
	case seen == 1 && effort == "":
		return "", rest, "(empty)" // review r1 note: an empty value is said too, never a silent default
	case seen == 1 && !knownEfforts[effort]:
		return "", rest, effort
	}
	return effort, rest, ""
}

// flosendoNewEffort records what /new asked for on the new session, and says so when the value is no effort.
func (e *Engine) flosendoNewEffort(p Platform, msg *Message, s *Session, sessions *SessionManager, effort, bad string) {
	switch {
	case bad != "":
		reason := "effort= given twice"
		if bad != "given twice" {
			if r := []rune(guardReason(bad)); len(r) > 16 {
				bad = string(r[:16])
			}
			reason = fmt.Sprintf("effort %s is not an effort (low, medium, high, xhigh or max)", bad)
		}
		s.setFlosendoEffort(&SessionEffort{From: effortFromRefused, Reason: reason})
		slog.Info("flosendo: session effort", "project", e.name, "session_key", msg.SessionKey, "effort", "default",
			"from", effortFromRefused, "reason", reason)
		e.reply(p, msg.ReplyCtx, reason+"; this session runs at the default")
	case effort != "":
		s.setFlosendoEffort(&SessionEffort{Requested: effort})
	default:
		return
	}
	sessions.Save()
}

// flosendoDecideEffort decides a requested effort before the session's first start: as asked for xhigh and max, else
// the guard's answer. It runs outside interactiveMu, so a slow guard holds only this session's turn. A decided record
// (a resume) is left as it is.
func (e *Engine) flosendoDecideEffort(p Platform, msg *Message, session *Session, sessions *SessionManager, ccKey string) {
	rec := session.FlosendoEffortRecord()
	if rec == nil || rec.From != "" {
		return
	}
	if !effortNeedsGuard(rec.Requested) {
		rec.Effort, rec.From = rec.Requested, effortFromNew
	} else if ok, reason := e.runEffortGuard(ccKey, rec.Requested, msg.Content); ok {
		rec.Effort, rec.From, rec.Reason = rec.Requested, effortFromNew, ""
	} else {
		rec.Effort, rec.From, rec.Reason = "", effortFromRefused, reason
		e.send(p, msg.ReplyCtx, fmt.Sprintf("effort %s refused: %s; this session runs at the default", rec.Requested, reason))
	}
	session.setFlosendoEffort(rec)
	sessions.Save()
	slog.Info("flosendo: session effort", "project", e.name, "session_key", ccKey, "requested", rec.Requested,
		"effort", effortOrDefault(rec.Effort), "from", rec.From, "reason", rec.Reason)
}

// runEffortGuard runs the guard with {project, session_key, requested_effort, first_message} on stdin. Exit 0 allows;
// anything else refuses, with the first line of its stdout as the reason. Its stderr is never read or logged.
func (e *Engine) runEffortGuard(ccKey, requested, firstMessage string) (bool, string) {
	path := e.effortGuardPath()
	if path == "" {
		return false, "no effort guard is configured"
	}
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return false, "the effort guard is missing"
	}
	in, err := json.Marshal(map[string]string{"project": e.name, "session_key": ccKey, "requested_effort": requested,
		"first_message": firstMessage})
	if err != nil {
		return false, "the effort guard's input could not be written"
	}
	ctx, cancel := context.WithTimeout(e.ctx, effortGuardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	cmd.Stdin = bytes.NewReader(in)
	out := &cappedBuffer{max: 4096}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second // a child that keeps stdout open cannot hold the start past the timeout
	runErr := cmd.Run()
	reason := guardReason(out.String())
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return false, "the effort guard timed out"
	}
	if runErr != nil {
		if reason == "" {
			code := -1
			var ee *exec.ExitError
			if errors.As(runErr, &ee) {
				code = ee.ExitCode()
			}
			reason = fmt.Sprintf("the effort guard failed (exit %d)", code)
		}
		return false, reason
	}
	return true, reason
}

// guardReason is the first line of the guard's stdout, printable characters only, at most 200.
func guardReason(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	line = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, line)
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > 200 {
		line = string(r[:200])
	}
	return line
}

type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// startEffort is the effort a start of this session gets ("" the default) and where it came from, for the start line.
func startEffort(session *Session) (effort, from string) {
	if session == nil {
		return "", "none"
	}
	rec := session.FlosendoEffortRecord()
	switch {
	case rec == nil:
		return "", "none"
	case rec.From == effortFromNew:
		return rec.Effort, effortFromNew
	case rec.From == effortFromRefused:
		return "", effortFromRefused
	}
	return "", "none" // requested but not decided: a start no decision ran for gets the default
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func effortOrDefault(e string) string {
	if e == "" {
		return "default"
	}
	return e
}

// flosendoEffortContext is ctx with this session's effort for its agent start, when it has one.
func (e *Engine) flosendoEffortContext(ctx context.Context, session *Session) context.Context {
	if eff, _ := startEffort(session); eff != "" {
		return WithSessionEffort(ctx, eff)
	}
	return ctx
}

type sessionEffortCtxKey struct{}

// WithSessionEffort marks ctx with the effort of the one agent start it is passed to.
func WithSessionEffort(ctx context.Context, effort string) context.Context {
	return context.WithValue(ctx, sessionEffortCtxKey{}, effort)
}

// SessionEffortFromContext returns the effort WithSessionEffort put in ctx, or "" (the agent's own setting).
func SessionEffortFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionEffortCtxKey{}).(string)
	return v
}
