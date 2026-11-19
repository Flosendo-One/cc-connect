package claudecode

// EDEN-1943: a CLI line of any size does not stop the bridge's reader, and a reader that ends while the CLI runs
// closes the session.
//
// On 15 Nov the chief of staff read a PDF Ivan had attached; the tool result came back as one stream-json line of
// 13.6 MB, over the read loop's 10 MB bufio.Scanner cap. The Scanner stopped ("token too long"), the loop ended, and
// the session was never closed: finishReadLoop waits for the CLI to exit, and it ran for six more hours. Nothing it
// wrote was read again, so every reply was lost, and each new message got EDEN-1137's false "Not processed" notice.
//
//   - flosendoReadLines reads a line of up to flosendoLineCap (64 MB, the chief of staff's ruling C-3). A longer line
//     is read to its end without being kept, and skipped visibly: a WARN, a notice in the channel, and, for a reply
//     line (assistant or result) while a turn is open, the existing error path ends that turn.
//   - If the reader ends for any reason while the CLI runs (a read error, or the CLI's stdout closing) and the process
//     lives on past flosendoReaderEndGrace, the session stops being alive, the channel is told, the engine's waiting
//     turn ends through the error path, and the process is stopped (the 1852 exit line says requested=true). The next
//     message starts the agent again; no "Not processed" notice is sent for a message nobody could read the answer to.
//
// Notices reach the channel as core.EventFlosendoNotice on the wakeup channel: the engine posts them at once, as
// written, where the session lives (core/flosendo_wakeup_target.go).
//
// Declared limits: the type of a skipped line is read from its first 4 KB only ("" when its "type" comes later); a
// message queued behind a turn the CLI started itself, when the reader ends, waits for the engine to see the process
// end (about 40 ms after the SIGTERM, measured for the CLI by EDEN-1852).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"syscall"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// flosendoLineCap is the longest line the reader keeps (C-3: 64 MB). Variable so tests can shorten it.
var flosendoLineCap = 64 << 20

// flosendoLineHead is how much of a skipped line is kept, to read its type.
const flosendoLineHead = 4 << 10

// flosendoLineKeep: a line buffer that grew past this is not kept for the next line.
const flosendoLineKeep = 1 << 20

// flosendoReaderEndGrace: the reader's end while the process still runs is normal for up to WaitDelay (3 s, a child
// holding stderr) after an EOF, and for a moment after a read error (Wait closes the pipe when a child holds stdout);
// after this long the reader counts as ended while the CLI runs.
var flosendoReaderEndGrace = 5 * time.Second

// flosendoWrapStdout is a test seam: the reader reads what it returns.
var flosendoWrapStdout = func(r io.Reader) io.Reader { return r }

// flosendoReadLines reads r line by line until EOF (nil) or a read error (returned; a partial line is dropped, as
// bufio.Scanner did). A line, without its newline and one trailing \r, goes to onLine, which must not keep the slice;
// a last line without a newline is delivered at EOF. A line whose bytes before its newline are more than
// flosendoLineCap is read to its end without being kept and goes to onSkip with its first flosendoLineHead bytes and
// its size.
func flosendoReadLines(r io.Reader, onLine func([]byte), onSkip func(head []byte, size int)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var line, head []byte
	size, skipping := 0, false
	for {
		chunk, err := br.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) && !errors.Is(err, io.EOF) {
			return err
		}
		complete := err == nil
		if complete {
			chunk = chunk[:len(chunk)-1]
		}
		size += len(chunk)
		switch {
		case skipping:
		case size > flosendoLineCap:
			skipping = true
			head = append([]byte(nil), line[:min(len(line), flosendoLineHead)]...)
			head = append(head, chunk[:min(len(chunk), flosendoLineHead-len(head))]...)
			line = nil
		default:
			line = append(line, chunk...)
		}
		if !complete && !errors.Is(err, io.EOF) {
			continue // the buffer filled: the line goes on
		}
		switch {
		case skipping:
			onSkip(head, size)
		case complete || len(line) > 0:
			onLine(bytes.TrimSuffix(line, []byte{'\r'}))
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		size, skipping, head = 0, false, nil
		if cap(line) > flosendoLineKeep {
			line = nil
		} else {
			line = line[:0]
		}
	}
}

// flosendoLineType is the top-level "type" of a stream-json line, read from its first bytes ("" if not there).
func flosendoLineType(head []byte) string {
	dec := json.NewDecoder(bytes.NewReader(head))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return ""
		}
		if k == "type" {
			v, err := dec.Token()
			if s, ok := v.(string); err == nil && ok {
				return s
			}
			return ""
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return ""
		}
	}
	return ""
}

// flosendoMB is n bytes in MB, rounded up.
func flosendoMB(n int) int { return (n + 1<<20 - 1) >> 20 }

// flosendoSkipNotice is the channel notice for a skipped line.
func flosendoSkipNotice(typ string, size int) string {
	what := "a line"
	switch typ {
	case "assistant":
		what = "a reply"
	case "result":
		what = "the end of a reply"
	case "user":
		what = "a tool's output"
	}
	return fmt.Sprintf("⚠️ Not read (too long: %d MB, over the %d MB limit): %s from the agent was skipped. "+
		"If an answer is missing, please ask again.", flosendoMB(size), flosendoMB(flosendoLineCap), what)
}

// flosendoClosedNotice is the channel notice for a session closed because its output stopped being read.
const flosendoClosedNotice = "❌ Session closed (the bridge stopped reading the agent's output). " +
	"Your next message starts it again. Please send again anything still unanswered."

// flosendoSkippedLine handles a line over the cap: logged, told in the channel, and a reply line ends its open turn
// through the error path (routed as the line would have been).
func (cs *claudeSession) flosendoSkippedLine(head []byte, size int) {
	cs.flosendoLine() // the CLI is writing: the quiet check waits
	typ := flosendoLineType(head)
	turn := cs.flosendoTurnAtExit()
	slog.Warn("flosendo: oversized CLI line skipped", "session_key", cs.flosendoExit.key, "type", typ, "bytes", size,
		"cap", flosendoLineCap, "turn", turn)
	cs.flosendoNotice(flosendoSkipNotice(typ, size))
	if (typ == "assistant" || typ == "result") && turn != "" {
		err := fmt.Errorf("read stdout: a %d MB %s line was over the %d MB limit and was skipped",
			flosendoMB(size), typ, flosendoMB(flosendoLineCap))
		cs.emitEvent(core.Event{Type: core.EventError, Error: err})
	}
}

// flosendoNotice sends a notice for the channel on the wakeup channel. The reader calls it, before finishReadLoop
// closes that channel.
func (cs *claudeSession) flosendoNotice(text string) {
	if cs.orphanEvents == nil {
		slog.Warn("flosendo: no wakeup channel for a notice", "session_key", cs.flosendoExit.key, "notice", text)
		return
	}
	select {
	case cs.orphanEvents <- core.Event{Type: core.EventFlosendoNotice, Content: text}:
	case <-cs.ctx.Done():
	}
}

// flosendoReaderEnded runs when the reader has ended while the CLI runs: the session stops being alive (Send refuses,
// and the engine starts the agent again for the next message), the quiet check stops (no false "Not processed"), the
// channel is told, an engine turn that waits ends through the error path, and the process is stopped. Nothing reads
// its output any more, so it gets SIGTERM at once (the CLI exits within about 40 ms, EDEN-1852), before Close's own
// steps: a turn it is still running cannot reach anyone, and a process the next message resumes must not overlap it.
func (cs *claudeSession) flosendoReaderEnded(err error) {
	if !cs.alive.CompareAndSwap(true, false) {
		return
	}
	cs.flosendoDeaf.Store(true)
	cs.turnMu.Lock()
	waits := cs.turnChannel != nil && cs.turnChannel == cs.events
	if cs.turns.lifecycle {
		waits = waits || cs.turns.anyTaken() || cs.turns.anyQueued()
	} else {
		waits = waits || cs.inTurn.Load() > 0
	}
	cs.turnMu.Unlock()
	cs.flosendoCloseTurns()
	pid := -1
	if cs.cmd != nil && cs.cmd.Process != nil {
		pid = cs.cmd.Process.Pid
	}
	why := "its output closed while it runs"
	if err != nil {
		why = err.Error()
	}
	slog.Warn("flosendo: the CLI's output stopped being read while it runs; closing the session",
		"session_key", cs.flosendoExit.key, "pid", pid, "error", why, "turn_waits", waits)
	cs.flosendoNotice(flosendoClosedNotice)
	if waits {
		select {
		case cs.events <- core.Event{Type: core.EventError, Error: fmt.Errorf("read stdout: %s; the session was closed",
			why)}:
		case <-cs.ctx.Done():
		}
	}
	cs.flosendoExit.requested.Store(true)
	if serr := signalProcessGroup(cs.cmd, syscall.SIGTERM); serr != nil {
		slog.Warn("flosendo: SIGTERM to a session nobody reads", "session_key", cs.flosendoExit.key, "error", serr)
	}
	go func() {
		if cerr := cs.Close(); cerr != nil {
			slog.Error("flosendo: closing a session nobody reads", "session_key", cs.flosendoExit.key, "error", cerr)
		}
	}()
}
