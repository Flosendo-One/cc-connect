package core

// EDEN-1943: where the wakeup path posts.
//
// On 16-17 Nov the chief of staff's replies in turns a stream started went out through the wakeup path with the last
// Slack turn's reply context: after Ivan answered inside one post's thread (20:42:51Z), the next two, the urgent
// "promote 1981" ask among them, went into that thread, not the channel. The chief of staff's ruling C-2: a wakeup, or
// a reply in a turn a stream started, posts where the session lives (the channel root for a channel's session key, the
// thread for a thread's key), never in the last thread.
//
//   - A whole wakeup turn (flushOrphanTurn) posts at the context rebuilt from the session key; the cached context only
//     when the platform cannot rebuild one.
//   - The part of a turn before one of our messages was taken into it (EDEN-1137's move, flushOrphanTurnNow) keeps
//     the cached context: it is that message's turn.
//   - EventFlosendoNotice (the agent's notices: a line too long to read, a session closed because its output stopped
//     being read; agent/claudecode/flosendo_read_line.go) is posted at once, as written, where the session lives,
//     with no wakeup header and no wait for the session lock.

import "log/slog"

// EventFlosendoNotice: a notice for the channel, posted as written (sent on the orphan channel only).
const EventFlosendoNotice EventType = "flosendo_notice"

// flosendoOrphanReplyCtx is where a wakeup posts: the context rebuilt from the session key, else the cached one.
func flosendoOrphanReplyCtx(platform Platform, sessionKey string, cached any) (any, error) {
	rc, err := reconstructOrphanReplyCtx(platform, sessionKey)
	if err == nil && rc != nil {
		return rc, nil
	}
	if cached != nil {
		slog.Info("orphan: reply context not rebuilt from the session key; using the cached one",
			"session_key", sessionKey, "error", err)
		return cached, nil
	}
	return nil, err
}

// flosendoPostNotice posts an agent's notice at once where the session lives.
func (e *Engine) flosendoPostNotice(state *interactiveState, text string) {
	if state == nil || text == "" {
		return
	}
	state.mu.Lock()
	platform := state.platform
	cached := state.replyCtx
	sessionKey := state.canonicalSessionKey
	state.mu.Unlock()
	if platform == nil {
		slog.Warn("flosendo: no platform for a notice", "session_key", sessionKey, "notice", text)
		return
	}
	rc, err := flosendoOrphanReplyCtx(platform, sessionKey, cached)
	if err != nil {
		slog.Warn("flosendo: no reply context for a notice", "session_key", sessionKey, "error", err, "notice", text)
		return
	}
	e.send(platform, rc, text)
}
