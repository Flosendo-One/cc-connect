package claudecode

// EDEN-1137: which turn does the CLI's output belong to?
//
// The bridge used to guess with a counter (inTurn: +1 per Send, -1 per result on the engine's channel). Three shapes
// broke the guess, all measured on the real CLI 2.1.286 (D0, testdata/eden1137/):
//   - a resume's synthetic task-notification result used up the count, so the real reply went to the wakeup path and
//     the engine's turn waited for the idle kill (2 Nov, #eden-dev);
//   - a message written while a turn the CLI started itself is at a tool call is answered inside that turn, whose
//     channel (wakeup) was chosen before the message: the reply posts as a wakeup and the engine's turn waits for the
//     idle kill (22 Oct, the chief of staff's channel; D0 s5);
//   - a message written while a turn the CLI started itself has no tool call gets its own turn, but the counter gives
//     the self-fired turn to the engine and the real reply to the wakeup path (D0 s3).
//
// The CLI says which turn takes which message: Send puts a uuid on each message and the CLI echoes it as
// command_uuid in command_lifecycle events (queued, started, completed). Once it has echoed one of ours, a CLI turn's
// events go to the engine's turn exactly when one of our commands has started, and to the wakeup path otherwise; when
// ours starts inside a turn already going to the wakeup path, the turn moves to the engine there and then (the part
// before is flushed as a wakeup). Every line reaches one channel, so a reply is posted once. If the turn's result has
// come, ours is still only queued and the CLI writes nothing for notTakenQuiet, the engine's turn is told
// (EventFlosendoNotTaken) and posts EDEN-1618's "Not processed" notice; the check waits while a turn runs (from its
// system init to its result), and a message the CLI ends without starting it is not taken at once. The engine acts on
// a signal only while it is current (NotTakenCurrent). A CLI that never echoes keeps the counter.

import (
	"log/slog"
	"slices"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
)

// notTakenQuiet is how long the CLI must be silent, after a turn's result and with one of our messages still only
// queued, before that message counts as never taken in. D0 measured 5-14 ms from a result to the next queued
// command's start; 30 s leaves room for a loaded box. Variable so tests can shorten it.
var notTakenQuiet = 30 * time.Second

type flosendoCmdState int

const (
	cmdQueued    flosendoCmdState = iota // written to stdin, not yet taken into a turn
	cmdStarted                           // the CLI took it into the open turn; its result is still to come
	cmdDone                              // the CLI said completed before the turn's result arrived
	cmdAbandoned                         // the engine's turn ended without it; if it starts later, the wakeup path shows it
)

// flosendoTurns is the lifecycle half of the routing state. Guarded by claudeSession.turnMu.
type flosendoTurns struct {
	ours        map[string]flosendoCmdState // our messages by uuid
	lifecycle   bool                        // the CLI has echoed one of our uuids: route by lifecycle, not the counter
	modeLogged  bool
	closed      bool        // the events channel is closed (finishReadLoop): the quiet timer must not send
	quietTimer  *time.Timer // armed while the CLI may still owe a queued message of ours a turn
	lastLineAt  time.Time   // the last line the CLI wrote, of any kind
	notTakenCnt int         // for tests and the log
	cliTurnOpen bool        // from a turn's system init to its result: the CLI is busy, not ignoring our message
	signalled   []string    // the ids of the last not-taken signal; nil once the engine's turn has settled
}

func (t *flosendoTurns) anyTaken() bool {
	for _, s := range t.ours {
		if s == cmdStarted || s == cmdDone {
			return true
		}
	}
	return false
}

func (t *flosendoTurns) anyQueued() bool {
	for _, s := range t.ours {
		if s == cmdQueued {
			return true
		}
	}
	return false
}

// flosendoNewCommand records a message we are about to write and returns its uuid.
func (cs *claudeSession) flosendoNewCommand() string {
	id := uuid.NewString()
	cs.turnMu.Lock()
	if cs.turns.ours == nil {
		cs.turns.ours = map[string]flosendoCmdState{}
	}
	cs.turns.ours[id] = cmdQueued
	cs.turnMu.Unlock()
	return id
}

// flosendoForgetCommand undoes flosendoNewCommand when the write failed.
func (cs *claudeSession) flosendoForgetCommand(id string) {
	cs.turnMu.Lock()
	delete(cs.turns.ours, id)
	cs.rearmQuietLocked()
	cs.turnMu.Unlock()
}

// flosendoSent arms the quiet check after a write: an idle CLI takes the message within milliseconds.
func (cs *claudeSession) flosendoSent() {
	cs.turnMu.Lock()
	cs.rearmQuietLocked()
	cs.turnMu.Unlock()
}

// pickChannelLocked chooses the channel of a CLI turn at its first routed event. Caller holds turnMu.
func (cs *claudeSession) pickChannelLocked() chan core.Event {
	if cs.orphanEvents == nil {
		return cs.events
	}
	if cs.turns.lifecycle {
		if cs.turns.anyTaken() {
			return cs.events
		}
		return cs.orphanEvents
	}
	if cs.inTurn.Load() > 0 {
		return cs.events
	}
	return cs.orphanEvents
}

// endTurnLocked runs at a turn's terminal event on channel ch. Caller holds turnMu.
func (cs *claudeSession) endTurnLocked(ch chan core.Event) {
	if cs.turns.lifecycle {
		if ch == cs.events {
			// Every command of ours taken into this turn ends with its result (D0 s2: two messages, one result).
			for id, s := range cs.turns.ours {
				if s == cmdStarted || s == cmdDone {
					delete(cs.turns.ours, id)
				}
			}
		}
	} else if ch == cs.events {
		if cs.inTurn.Add(-1) < 0 {
			cs.inTurn.Store(0)
		}
		if len(cs.turns.ours) > 0 {
			// A turn of ours ended and the CLI echoed none of our uuids: it never will, so the counter routes, and
			// the map must not grow.
			if !cs.turns.modeLogged {
				cs.turns.modeLogged = true
				slog.Info("flosendo: turn routing by counter (the CLI echoed no command_lifecycle)", "session_id", cs.CurrentSessionID(),
					"limit", "a message taken into a turn the CLI started itself waits for the idle timeout")
			}
			for id := range cs.turns.ours {
				delete(cs.turns.ours, id)
			}
		}
	}
	cs.turnChannel = nil
	cs.turns.cliTurnOpen = false
	cs.rearmQuietLocked()
}

// bypassChannel is where a system line or a tool result goes. They never pick a turn's channel (a stray system
// snapshot at start-up must not lock one in), but they follow the open turn's (D1b): a turn going to the wakeup path
// no longer feeds the engine's phantom grace or idle timer. With no turn open they go to the engine as before, except
// when the lifecycle shows no turn of ours is running: then they belong to no turn of ours, and the wakeup path
// ignores them.
func (cs *claudeSession) bypassChannel() chan core.Event {
	cs.turnMu.Lock()
	defer cs.turnMu.Unlock()
	if cs.turnChannel != nil {
		return cs.turnChannel
	}
	if cs.turns.lifecycle && cs.orphanEvents != nil && !cs.turns.anyTaken() {
		return cs.orphanEvents
	}
	return cs.events
}

func (cs *claudeSession) emitBypass(evt core.Event) {
	ch := cs.bypassChannel()
	select {
	case ch <- evt:
	case <-cs.ctx.Done():
	}
}

// handleCommandLifecycle reads the CLI's command_lifecycle events (D0: queued when written, started when taken into
// a turn, completed with that turn's result).
func (cs *claudeSession) handleCommandLifecycle(raw map[string]any) {
	id, _ := raw["command_uuid"].(string)
	state, _ := raw["state"].(string)
	moved, endedUnstarted := false, false
	cs.turnMu.Lock()
	s, ours := cs.turns.ours[id]
	if !ours {
		// A command the CLI made itself: its turn is a self-fired one, routed at its first event.
		cs.turnMu.Unlock()
		return
	}
	if !cs.turns.lifecycle {
		cs.turns.lifecycle = true
	}
	logMode := !cs.turns.modeLogged
	cs.turns.modeLogged = true
	switch state {
	case "started":
		if s == cmdQueued {
			cs.turns.ours[id] = cmdStarted
			if cs.turnChannel != nil && cs.turnChannel == cs.orphanEvents {
				// Ours was taken into a turn already going to the wakeup path (D0 s5, 22 Oct): from here on the turn
				// answers our message, so it is the engine's.
				cs.turnChannel = cs.events
				moved = true
			}
		}
	case "completed", "cancelled", "discarded":
		switch s {
		case cmdStarted:
			if state == "completed" {
				cs.turns.ours[id] = cmdDone // its result is still to come (D0 s2, s5: completed just before the result)
			}
		case cmdQueued:
			// The CLI is done with a message it never took in (D0 saw this for none of ours; the bridge sends no
			// interrupt): it will never answer it.
			delete(cs.turns.ours, id)
			endedUnstarted = true
		case cmdAbandoned:
			delete(cs.turns.ours, id)
		}
	}
	if endedUnstarted && !cs.turns.closed && !cs.turns.anyTaken() && !cs.turns.anyQueued() {
		// Nothing of ours is left for the engine's turn to wait for: it ends now, not after the quiet window.
		slog.Warn("flosendo: the CLI ended a message without taking it in; ending the turn that waits for it",
			"session_id", cs.CurrentSessionID(), "state", state)
		cs.signalNotTakenLocked([]string{id})
	}
	cs.rearmQuietLocked()
	cs.turnMu.Unlock()
	if logMode {
		slog.Info("flosendo: turn routing by command lifecycle", "session_id", cs.CurrentSessionID())
	}
	if moved {
		slog.Info("flosendo: a message was taken into a self-fired turn; the rest of the turn is the reply",
			"session_id", cs.CurrentSessionID())
		select {
		case cs.orphanEvents <- core.Event{Type: core.EventFlosendoTurnMoved}:
		case <-cs.ctx.Done():
		}
	}
}

// flosendoLine notes that the CLI wrote a line, for the quiet check.
func (cs *claudeSession) flosendoLine() {
	cs.turnMu.Lock()
	cs.turns.lastLineAt = time.Now()
	cs.rearmQuietLocked()
	cs.turnMu.Unlock()
}

// flosendoCLITurn notes a turn's system init (open) and a result the router never sees (the resume's dropped
// synthetic one): the quiet check waits while the CLI runs a turn, however long its first model call takes.
func (cs *claudeSession) flosendoCLITurn(open bool) {
	cs.turnMu.Lock()
	cs.turns.cliTurnOpen = open
	cs.rearmQuietLocked()
	cs.turnMu.Unlock()
}

// owesQuietCheckLocked: a message of ours is waiting to be taken in, and no turn is open. Caller holds turnMu.
func (cs *claudeSession) owesQuietCheckLocked() bool {
	return cs.turns.lifecycle && !cs.turns.closed && !cs.turns.cliTurnOpen && cs.turnChannel == nil &&
		!cs.turns.anyTaken() && cs.turns.anyQueued()
}

// rearmQuietLocked (re)starts the quiet timer from now, or stops it. Caller holds turnMu.
func (cs *claudeSession) rearmQuietLocked() {
	if !cs.owesQuietCheckLocked() {
		if cs.turns.quietTimer != nil {
			cs.turns.quietTimer.Stop()
		}
		return
	}
	if cs.turns.quietTimer == nil {
		cs.turns.quietTimer = time.AfterFunc(notTakenQuiet, cs.quietFired)
		return
	}
	cs.turns.quietTimer.Reset(notTakenQuiet)
}

// quietFired: the CLI has been silent for notTakenQuiet with a message of ours still queued and no turn open. The
// message was never taken in: mark it abandoned and tell the engine's turn, which posts the "Not processed" notice.
func (cs *claudeSession) quietFired() {
	cs.turnMu.Lock()
	defer cs.turnMu.Unlock()
	if !cs.owesQuietCheckLocked() {
		return
	}
	if quiet := time.Since(cs.turns.lastLineAt); !cs.turns.lastLineAt.IsZero() && quiet < notTakenQuiet {
		cs.turns.quietTimer.Reset(notTakenQuiet - quiet)
		return
	}
	var ids []string
	for id, s := range cs.turns.ours {
		if s == cmdQueued {
			cs.turns.ours[id] = cmdAbandoned
			ids = append(ids, id)
		}
	}
	slog.Warn("flosendo: the CLI did not take a message in; ending the turn that waits for it",
		"session_id", cs.CurrentSessionID(), "messages", len(ids), "quiet", notTakenQuiet)
	cs.signalNotTakenLocked(ids)
}

// signalNotTakenLocked tells the engine's turn that the CLI will not answer the messages ids. Caller holds turnMu.
// Not blocking, and under turnMu so finishReadLoop cannot close the channel in between: the engine's turn is reading
// it; if the buffer is ever full, the turn waits for the idle timeout as before. The signal is current until the
// engine's turn settles or a newer one is sent (NotTakenCurrent).
func (cs *claudeSession) signalNotTakenLocked(ids []string) {
	slices.Sort(ids)
	cs.turns.signalled = ids
	cs.turns.notTakenCnt += len(ids)
	select {
	case cs.events <- core.NewFlosendoNotTaken(cs.CurrentSessionID(), ids):
	default:
		slog.Warn("flosendo: not-taken signal dropped (events buffer full)", "session_id", cs.CurrentSessionID())
	}
}

// NotTakenCurrent is the engine's check before it acts on a not-taken signal: only the latest one, and only until the
// engine's turn settles. One sent while the turn it was for is ending (between its last read and its SettleTurn) is
// still in the channel when the next turn reads it, and must not end that turn.
func (cs *claudeSession) NotTakenCurrent(ids []string) bool {
	cs.turnMu.Lock()
	defer cs.turnMu.Unlock()
	return len(ids) > 0 && slices.Equal(ids, cs.turns.signalled)
}

// SettleTurn is called by the engine whenever its turn ends, however it ended (EDEN-1137 D3d): a not-taken signal
// still unread is no longer current, commands of ours that no CLI turn is answering are abandoned, and the counter
// goes back to zero when no turn is open on the engine's channel, so the next self-fired turn still goes to the wakeup
// path and the next Send still reaches the engine.
func (cs *claudeSession) SettleTurn() {
	cs.turnMu.Lock()
	defer cs.turnMu.Unlock()
	cs.turns.signalled = nil
	open := cs.turnChannel != nil && cs.turnChannel == cs.events
	if !open {
		for id, s := range cs.turns.ours {
			if s == cmdQueued {
				cs.turns.ours[id] = cmdAbandoned
			}
		}
		cs.inTurn.Store(0)
	}
	cs.rearmQuietLocked()
}

// flosendoCloseTurns stops the quiet timer for good before the channels close.
func (cs *claudeSession) flosendoCloseTurns() {
	cs.turnMu.Lock()
	cs.turns.closed = true
	if cs.turns.quietTimer != nil {
		cs.turns.quietTimer.Stop()
	}
	cs.turnMu.Unlock()
}

var (
	_ core.TurnSettler     = (*claudeSession)(nil)
	_ core.NotTakenChecker = (*claudeSession)(nil)
)
