package core

// EDEN-1618 (Ivan, 20 Oct 2026, "promote 1618"): a message that arrives while its session is busy is queued, and
// until now a reset, an idle kill or a restart of the bridge threw that queue away. A reset and an idle kill posted a
// bare "❌ Error: session reset" without saying which message; a restart posted nothing at all. On 19 Oct, Ivan's
// "sign 1" and ten numbered answers sat in such a queue for two hours and were lost to the idle kill.
//
// Now:
//   - Idle kill (no agent events for the idle limit): the queued messages are handed to a fresh session, in order,
//     once the dead turn has let go of the session; and one line goes to the alert channel, if one is set.
//   - Reset (/new, /stop, a self-reset by `cc-connect send --session-cmd /new`): each queued message gets
//     "Not processed (<why>)" in its own thread, quoting its first line, so the sender knows what to send again.
//     A reset is asked for, so its queue is not replayed into the new session.
//   - Restart of the bridge: the queues are written to <data dir>/run/pending_queue_<project>.json before anything is
//     cancelled (a running turn would otherwise drain or drop them as its agent dies, and nothing can be posted on a
//     closing connection), and anything queued after that is saved too; once a platform is ready again each message
//     gets the same "Not processed" notice in its thread.
//   - Every drop writes a log line with remaining_queue=0, so `flosendo/deploy.sh queued` stops counting it.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// savedQueueMu serialises every read and write of the saved-queue files, so a stop's save and a restart's notices
// never lose each other's items.
var savedQueueMu sync.Mutex

// queuedFirstLineMax is how much of a message the notice quotes.
const queuedFirstLineMax = 120

// replayWaitLimit is how long an idle-kill replay waits for the dead turn to release the session before it falls
// back to the notice.
var replayWaitLimit = 2 * time.Minute

// replayPollInterval is how often the replay checks whether the session is free.
var replayPollInterval = 200 * time.Millisecond

// ReplyTargetKeyer is implemented by a platform that can name the thread a reply context points at as a session key
// its ReconstructReplyCtx understands (Slack: "slack:<channel>:t:<thread ts>"). It lets a queued message's notice land
// in that message's thread after a restart, when the live reply context is gone.
type ReplyTargetKeyer interface {
	ReplyTargetKey(replyCtx any) (string, bool)
}

// queuedFirstLine is the first non-empty line of a message, cut to queuedFirstLineMax characters.
func queuedFirstLine(content string) string {
	line := ""
	for _, l := range strings.Split(content, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			line = t
			break
		}
	}
	if line == "" {
		return "(a message with no text)"
	}
	if utf8.RuneCountInString(line) > queuedFirstLineMax {
		r := []rune(line)
		line = string(r[:queuedFirstLineMax]) + "…"
	}
	return line
}

// notProcessedText is the notice a queued message gets when it will not be processed.
func notProcessedText(reason, firstLine string) string {
	return fmt.Sprintf("❌ Not processed (%s): “%s”. Please send it again if it still matters.", reason, firstLine)
}

// takePendingMessages empties a state's queue and returns what was in it.
func takePendingMessages(state *interactiveState) []queuedMessage {
	state.mu.Lock()
	defer state.mu.Unlock()
	items := state.pendingMessages
	state.pendingMessages = nil
	return items
}

// notifyNotProcessed tells each queued message's sender, in that message's own thread, that it was not processed
// and why, quoting its first line; and logs the drop with remaining_queue=0 for deploy.sh's queue check.
func (e *Engine) notifyNotProcessed(sessionKey string, items []queuedMessage, reason string) {
	if len(items) == 0 {
		return
	}
	if e.ctx.Err() != nil {
		// The bridge is stopping: a notice would go to a closing connection. Save them for after the restart.
		e.saveQueuedItems(sessionKey, items)
		return
	}
	for _, q := range items {
		e.send(q.platform, q.replyCtx, notProcessedText(reason, queuedFirstLine(q.content)))
	}
	slog.Warn("queued messages dropped", "session", sessionKey, "reason", reason, "dropped", len(items), "remaining_queue", 0)
}

// toMessage rebuilds the message a queued item came from, so it can be handled again.
func (q queuedMessage) toMessage() *Message {
	return &Message{
		SessionKey:        q.msgSessionKey,
		Platform:          q.msgPlatform,
		MessageID:         q.messageID,
		UserID:            q.userID,
		UserName:          q.userName,
		ChatName:          q.msgChatName,
		Content:           q.content,
		Images:            q.images,
		Files:             q.files,
		ChannelKey:        q.channelKey,
		ReplyCtx:          q.replyCtx,
		FromVoice:         q.fromVoice,
		UserMessageTimeMs: q.userMessageTimeMs,
	}
}

// replayAfterIdleKill hands an idle-killed session's queued messages to a fresh session, in order, once the dead turn
// has released the session; if it never does within replayWaitLimit, each message gets the notice instead.
func (e *Engine) replayAfterIdleKill(session *Session, sessionKey string, items []queuedMessage) {
	if len(items) == 0 {
		return
	}
	deadline := time.Now().Add(replayWaitLimit)
	for session != nil && session.Busy() && e.ctx.Err() == nil {
		if time.Now().After(deadline) {
			e.notifyNotProcessed(sessionKey, items, "the session timed out and stayed busy")
			return
		}
		time.Sleep(replayPollInterval)
	}
	if e.ctx.Err() != nil {
		e.saveQueuedItems(sessionKey, items)
		return
	}
	slog.Warn("queued messages replayed after idle kill", "session", sessionKey, "replayed", len(items), "remaining_queue", 0)
	for _, q := range items {
		e.handleMessage(q.platform, q.toMessage())
	}
}

// SetAlertSessionKey names where the bridge posts its own alerts (an idle kill), as a session key its platform can
// reconstruct, e.g. "slack:<channel id>". Empty means no alerts.
func (e *Engine) SetAlertSessionKey(key string) {
	e.alertSessionKey = strings.TrimSpace(key)
}

// sendToKey posts to a session key's channel or thread, reconstructed from the key itself (never a live state's
// current thread).
func (e *Engine) sendToKey(key, text string) error {
	name := key
	if i := strings.Index(key, ":"); i > 0 {
		name = key[:i]
	}
	for _, p := range e.platforms {
		if p.Name() != name {
			continue
		}
		rc, ok := p.(ReplyContextReconstructor)
		if !ok {
			return fmt.Errorf("platform %s cannot post to %q", name, key)
		}
		replyCtx, err := rc.ReconstructReplyCtx(key)
		if err != nil {
			return err
		}
		return e.sendWithError(p, replyCtx, text)
	}
	return fmt.Errorf("no platform %q for %q", name, key)
}

// alert posts one line to the alert session, if one is set.
func (e *Engine) alert(text string) {
	if e.alertSessionKey == "" {
		return
	}
	if err := e.sendToKey(e.alertSessionKey, text); err != nil {
		slog.Warn("alert not posted", "alert_session", e.alertSessionKey, "error", err)
	}
}

// savedQueuedMessage is one queued message written to disk when the engine stops.
type savedQueuedMessage struct {
	Session     string    `json:"session"`
	ReplyTarget string    `json:"reply_target"`
	FirstLine   string    `json:"first_line"`
	MessageID   string    `json:"message_id,omitempty"`
	SavedAt     time.Time `json:"saved_at"`
}

func (e *Engine) pendingQueuePath() string {
	if e.dataDir == "" {
		return ""
	}
	name := e.name
	if name == "" {
		name = "default"
	}
	return filepath.Join(e.dataDir, "run", "pending_queue_"+name+".json")
}

// savedItems turns queued messages into what is written to disk: the session, the thread to answer in, the first line.
func savedItems(sessionKey string, items []queuedMessage) []savedQueuedMessage {
	now := time.Now().UTC()
	saved := make([]savedQueuedMessage, 0, len(items))
	for _, q := range items {
		target := q.msgSessionKey
		if k, ok := q.platform.(ReplyTargetKeyer); ok {
			if t, ok := k.ReplyTargetKey(q.replyCtx); ok && t != "" {
				target = t
			}
		}
		saved = append(saved, savedQueuedMessage{Session: sessionKey, ReplyTarget: target, FirstLine: queuedFirstLine(q.content), MessageID: q.messageID, SavedAt: now})
	}
	return saved
}

// saveQueuedItems adds queued messages to the saved-queue file, for the restarted bridge to tell each sender.
func (e *Engine) saveQueuedItems(sessionKey string, items []queuedMessage) {
	if len(items) == 0 {
		return
	}
	path := e.pendingQueuePath()
	if path == "" {
		slog.Error("queued messages lost at stop: no data dir to save them in", "session", sessionKey, "lost", len(items), "remaining_queue", 0)
		return
	}
	savedQueueMu.Lock()
	defer savedQueueMu.Unlock()
	saved := savedItems(sessionKey, items)
	if prev, err := readSavedQueue(path); err == nil {
		saved = append(prev, saved...)
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			err = AtomicWriteFile(path, data, 0o600)
		}
	}
	if err != nil {
		slog.Error("queued messages could not be saved at stop", "path", path, "error", err, "lost", len(items), "remaining_queue", 0)
		return
	}
	slog.Warn("queued messages saved at stop", "session", sessionKey, "saved", len(items), "remaining_queue", 0)
}

// savePendingQueues takes every state's queue and saves it. Called before the engine's context is cancelled
// (StopPlatforms, Stop), so a running turn cannot drain or drop it as it dies, and again in Stop for anything later.
func (e *Engine) savePendingQueues(states map[string]*interactiveState) {
	for key, state := range states {
		if state == nil {
			continue
		}
		e.saveQueuedItems(key, takePendingMessages(state))
	}
}

// saveQueuesBeforeStop saves every live state's queue without removing the states (the stop that follows does that).
func (e *Engine) saveQueuesBeforeStop() {
	e.interactiveMu.Lock()
	states := make(map[string]*interactiveState, len(e.interactiveStates))
	for k, v := range e.interactiveStates {
		states[k] = v
	}
	e.interactiveMu.Unlock()
	e.savePendingQueues(states)
}

func readSavedQueue(path string) ([]savedQueuedMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var saved []savedQueuedMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, err
	}
	return saved, nil
}

// savedQueueMaxAge is how long a saved message keeps being retried; after that its notice is dropped, not posted late.
var savedQueueMaxAge = 7 * 24 * time.Hour

func (s savedQueuedMessage) id() string {
	return s.ReplyTarget + "\x00" + s.MessageID + "\x00" + s.FirstLine + "\x00" + s.SavedAt.Format(time.RFC3339Nano)
}

// noticeSavedQueue posts the "Not processed" notice for every saved message whose reply target is on platform p, once
// p is ready after a restart, and keeps the rest (another platform's, or one whose notice failed) on disk. The file
// lock is not held while posting: a slow send must never hold up a stop's save.
func (e *Engine) noticeSavedQueue(p Platform) {
	path := e.pendingQueuePath()
	if path == "" {
		return
	}
	savedQueueMu.Lock()
	saved, err := readSavedQueue(path)
	savedQueueMu.Unlock()
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("saved queue unreadable", "path", path, "error", err)
		}
		return
	}
	done := map[string]bool{}
	posted, expired := 0, 0
	for _, s := range saved {
		if !strings.HasPrefix(s.ReplyTarget, p.Name()+":") {
			continue
		}
		if time.Since(s.SavedAt) > savedQueueMaxAge {
			done[s.id()] = true
			expired++
			continue
		}
		if err := e.sendToKey(s.ReplyTarget, notProcessedText("the bridge restarted", s.FirstLine)); err != nil {
			slog.Warn("saved queued message: notice not posted", "target", s.ReplyTarget, "error", err)
			continue
		}
		done[s.id()] = true
		posted++
	}
	if posted > 0 || expired > 0 {
		slog.Warn("queued messages from before the restart: senders told", "posted", posted, "expired", expired, "remaining_queue", 0)
	}
	if len(done) == 0 {
		return
	}
	// Remove only what was handled; a stop may have saved more meanwhile.
	savedQueueMu.Lock()
	defer savedQueueMu.Unlock()
	current, err := readSavedQueue(path)
	if err != nil {
		return
	}
	var rest []savedQueuedMessage
	for _, s := range current {
		if !done[s.id()] {
			rest = append(rest, s)
		}
	}
	if len(rest) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Error("saved queue not removed", "path", path, "error", err)
		}
		return
	}
	if data, err := json.MarshalIndent(rest, "", "  "); err == nil {
		_ = AtomicWriteFile(path, data, 0o600)
	}
}
