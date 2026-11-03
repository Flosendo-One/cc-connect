package core

// EDEN-1137: the engine's half of routing a CLI turn by its command lifecycle (the agent's half is
// agent/claudecode/flosendo_turn_routing.go).
//
//   - EventFlosendoTurnMoved reaches the wakeup path when one of our messages is taken into a turn that was going
//     there: the part of the turn before it is flushed as a wakeup (text only, at once), and the rest is the engine
//     turn's reply.
//   - EventFlosendoNotTaken reaches the engine's turn when the CLI never took its message in: the turn ends with
//     EDEN-1618's "Not processed" notice as its reply, so nothing is lost unseen and the queue drains. It names the
//     messages it gives up on, and the engine acts on it only while the agent says it is current (NotTakenChecker):
//     one sent while the turn it was for is ending stays in the channel, and must not end the next turn.
//   - TurnSettler: whenever an engine turn ends, however it ended, the agent session settles its routing state.
//   - Only model output disarms the phantom grace (D3a): a session-id-only line or a tool result can still reach a
//     waiting turn from another turn, and it must not keep that turn alive past the grace.

import "log/slog"

const (
	// EventFlosendoTurnMoved: the rest of the open turn goes to the engine's turn (sent on the orphan channel only).
	EventFlosendoTurnMoved EventType = "flosendo_turn_moved"
	// EventFlosendoNotTaken: the CLI never took the waiting turn's message in (sent on the engine's channel only).
	EventFlosendoNotTaken EventType = "flosendo_not_taken"
)

// notTakenReason is the reason in the "Not processed" notice for a message the agent never took in.
const notTakenReason = "the agent did not take it in"

// flosendoNotTakenKey is the Metadata key of the message ids a not-taken signal gives up on.
const flosendoNotTakenKey = "flosendo_not_taken"

// NewFlosendoNotTaken is the not-taken signal for the messages ids.
func NewFlosendoNotTaken(sessionID string, ids []string) Event {
	return Event{Type: EventFlosendoNotTaken, SessionID: sessionID, Metadata: map[string]any{flosendoNotTakenKey: ids}}
}

// FlosendoNotTakenIDs returns the message ids a not-taken signal gives up on.
func FlosendoNotTakenIDs(ev Event) []string {
	ids, _ := ev.Metadata[flosendoNotTakenKey].([]string)
	return ids
}

// NotTakenChecker is implemented by agent sessions that send EventFlosendoNotTaken: the engine asks before acting on
// one whether it is still current.
type NotTakenChecker interface {
	NotTakenCurrent(ids []string) bool
}

// flosendoNotTakenCurrent: act on a not-taken signal only while its agent says it is current (an agent with no
// checker: always).
func flosendoNotTakenCurrent(as AgentSession, ev Event) bool {
	c, ok := as.(NotTakenChecker)
	return !ok || c.NotTakenCurrent(FlosendoNotTakenIDs(ev))
}

// TurnSettler is implemented by agent sessions that keep per-turn routing state.
type TurnSettler interface {
	SettleTurn()
}

// flosendoSettleTurn is deferred by processInteractiveEvents: every way a turn ends settles the agent's count.
func flosendoSettleTurn(as AgentSession) {
	if s, ok := as.(TurnSettler); ok {
		s.SettleTurn()
	}
}

// flosendoDisarmsPhantom: only events from the model show the turn is alive after an ignored phantom result.
func flosendoDisarmsPhantom(ev Event) bool {
	switch ev.Type {
	case EventText:
		return ev.Content != ""
	case EventToolResult:
		return false
	default:
		return true
	}
}

// flosendoNotTakenResult turns the not-taken signal into the turn's result: its reply is the notice, posted once by
// the normal reply path, after which the queue drains as after any reply.
func flosendoNotTakenResult(state *interactiveState, sessionKey string) Event {
	state.mu.Lock()
	first := state.flosendoTurnFirstLine
	state.mu.Unlock()
	if first == "" {
		first = "(a message with no text)"
	}
	slog.Warn("flosendo: the agent did not take the turn's message in; ending the turn with the not-processed notice",
		"session_key", sessionKey)
	return Event{Type: EventResult, Done: true, Content: notProcessedText(notTakenReason, first)}
}
