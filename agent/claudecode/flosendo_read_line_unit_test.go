//go:build !windows

package claudecode

// EDEN-1943, contract 52ea6a01 (Ivan, 18 Nov: "promote 1943"; the chief of staff's ruling C-3: a 64 MB cap, a longer
// line skipped visibly, ending its turn through the error path; if the reader ends for any reason while the CLI lives,
// a notice in the channel and the session closed). The checks that need the fix's own names (flosendo_read_line.go):
//   2   a line over the parse cap is not kept: it is skipped, the next line is read, and a WARN names its type and
//       size (LineOverParseCapSkipped)
//   3   a skipped line is visible: a notice in the channel and a WARN for every skip; a skipped reply line (assistant
//       or result) ends its open turn through the error path (SkippedLineIsVisible)
//   4b  a read error while the CLI runs closes the session, as an EOF does in ReaderEndStopsTheSession
//       (ReaderErrorStopsTheSession); a read error from a CLI that then ends on its own does not
//       (ReaderErrorThenExitIsNoReaderEnd, round 1's review note); the waiting turn ends when the reader fails right
//       after our message was taken in (ReaderErrorAfterStartedEndsTheWaitingTurn, QA's probe 2)
//   and flosendoReadLines' edges (ReadLinesEdges) and the type read from a skipped line's head (LineTypeFromHead).

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// newSession1943 starts a real claudeSession on the fake (as EDEN-1137's newReplaySession does). Set any package
// variable the session reads BEFORE calling it: its Close (registered here) then runs before the variable's restore.
func newSession1943(t *testing.T, f fake1943) *claudeSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var extraEnv []string
	for k, v := range f.env() {
		extraEnv = append(extraEnv, k+"="+v)
	}
	cs, err := newClaudeSession(ctx, t.TempDir(), os.Args[0], []string{"-test.run=^TestFlosendo1943Helper$", "--"}, "",
		"", "", "", "bypassPermissions", "", "", nil, nil, nil, extraEnv, "", false, core.SpawnOptions{}, 0, t.TempDir(), "")
	if err != nil {
		t.Fatalf("newClaudeSession: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func setLineCap1943(t *testing.T, n int) {
	old := flosendoLineCap
	flosendoLineCap = n
	t.Cleanup(func() { flosendoLineCap = old })
}

func logsWith(lb *logBuf, msg string) []string {
	var out []string
	for _, l := range lb.lines() {
		if msgIs(l, msg) {
			out = append(out, l)
		}
	}
	return out
}

func noticesIn(rs []routed) []string {
	var out []string
	for _, r := range rs {
		if r.evt.Type == core.EventFlosendoNotice {
			out = append(out, r.ch+": "+r.evt.Content)
		}
	}
	return out
}

func errorsIn(rs []routed, ch string) []string {
	var out []string
	for _, r := range rs {
		if r.ch == ch && r.evt.Type == core.EventError {
			out = append(out, r.evt.Error.Error())
		}
	}
	return out
}

const skipLog1943 = "flosendo: oversized CLI line skipped"

// Item 2: with the cap at 256 KB, 15 Nov's 13,599,627-byte tool result is skipped, not kept or parsed: one WARN with
// its type and size, one notice, no error (a tool's output is not a reply), and the turn's reply and the next turn are
// read as usual.
func TestFlosendo1943_LineOverParseCapSkipped(t *testing.T) {
	setLineCap1943(t, 256<<10)
	lb := captureLog(t)
	dir := t.TempDir()
	cs := newSession1943(t, fake1943{fixtures: []string{"big_tool_result.jsonl"}, dir: dir})
	c := collect(cs)
	if err := cs.Send("read the brochure", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	got := c.waitFor(t, "the turn's result after the skipped line", 15*time.Second, func(s []routed) bool {
		return len(results(s, "engine")) == 1
	})
	if r := results(got, "engine")[0]; !strings.Contains(r.Content, "AFTER-BIG") {
		t.Fatalf("the turn's result is not the reply after the skipped line:\n%s", dumpRouted(got))
	}
	skips := logsWith(lb, skipLog1943)
	if len(skips) != 1 {
		t.Fatalf("want one %q line, got %d:\n%s", skipLog1943, len(skips), notable(lb))
	}
	for k, want := range map[string]string{"type": "user", "bytes": "13599627", "cap": "262144", "turn": "engine"} {
		if v, _ := field(skips[0], k); v != want {
			t.Fatalf("the skip line's %s is %q, want %q: %s", k, v, want, skips[0])
		}
	}
	if n := noticesIn(got); len(n) != 1 || !strings.Contains(n[0], "wakeup: ⚠️ Not read (too long: 13 MB, over the 1 MB limit): a tool's output") {
		t.Fatalf("want one skip notice on the wakeup channel, got %q", n)
	}
	if e := append(errorsIn(got, "engine"), errorsIn(got, "wakeup")...); len(e) != 0 {
		t.Fatalf("a skipped tool output must not end the turn: %q", e)
	}
	waitFile(t, dir+"/p1-waiting-2", 10*time.Second)
	if err := cs.Send("and the next one", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	got = c.waitFor(t, "the next turn's result", 10*time.Second, func(s []routed) bool { return len(results(s, "engine")) == 2 })
	if r := results(got, "engine")[1]; !strings.Contains(r.Content, "SECOND-OK") || !cs.Alive() {
		t.Fatalf("the session did not read on after the skip (alive %v):\n%s", cs.Alive(), dumpRouted(got))
	}
}

// Item 3: a skipped reply line, an assistant line or the result, is told in the channel and logged, and ends the
// open turn through the error path, so the engine's turn does not wait for a reply nobody will read.
func TestFlosendo1943_SkippedLineIsVisible(t *testing.T) {
	for _, c := range []struct{ fixture, typ, what string }{
		{"big_reply.jsonl", "assistant", "a reply"},
		{"big_result.jsonl", "result", "the end of a reply"},
	} {
		t.Run(c.typ, func(t *testing.T) {
			setLineCap1943(t, 64<<10)
			lb := captureLog(t)
			cs := newSession1943(t, fake1943{fixtures: []string{c.fixture}, dir: t.TempDir()})
			col := collect(cs)
			if err := cs.Send("first", "", nil, nil); err != nil {
				t.Fatal(err)
			}
			got := col.waitFor(t, "the error that ends the turn", 15*time.Second, func(s []routed) bool {
				return len(errorsIn(s, "engine")) == 1 && len(noticesIn(s)) == 1
			})
			if e := errorsIn(got, "engine")[0]; !strings.Contains(e, "read stdout: a 1 MB "+c.typ+" line was over the 1 MB limit and was skipped") {
				t.Fatalf("the turn's error does not name the skipped line: %q", e)
			}
			if n := noticesIn(got)[0]; !strings.Contains(n, "wakeup: ⚠️ Not read (too long: 1 MB, over the 1 MB limit): "+c.what+" from the agent was skipped") {
				t.Fatalf("the notice: %q", n)
			}
			skips := logsWith(lb, skipLog1943)
			if len(skips) != 1 {
				t.Fatalf("want one %q line, got %d:\n%s", skipLog1943, len(skips), notable(lb))
			}
			if v, _ := field(skips[0], "type"); v != c.typ {
				t.Fatalf("the skip line's type is %q, want %q", v, c.typ)
			}
			if v, _ := field(skips[0], "bytes"); v != "300000" {
				t.Fatalf("the skip line's size is %q: %s", v, skips[0])
			}
		})
	}
}

type failAfterLines struct {
	r     io.Reader
	lines int
	seen  int
	err   error
}

func (f *failAfterLines) Read(p []byte) (int, error) {
	if f.seen >= f.lines {
		return 0, f.err
	}
	n, err := f.r.Read(p)
	for i := 0; i < n; i++ {
		if p[i] == '\n' {
			if f.seen++; f.seen == f.lines {
				return i + 1, nil
			}
		}
	}
	return n, err
}

// Item 4b: the reader fails (here after the reply's text, while the engine's turn waits for its result) and the CLI
// runs on: the turn ends through the error path, the channel is told once, the session is no longer alive (Send
// refuses, so the engine starts the agent again), and the bridge stops the process (the 1852 line: requested=true).
// The CLI has written to its stderr, so the stopped process's stderr is not a second error post (QA's N2).
func TestFlosendo1943_ReaderErrorStopsTheSession(t *testing.T) {
	old := flosendoWrapStdout
	flosendoWrapStdout = func(r io.Reader) io.Reader {
		return &failAfterLines{r: r, lines: 4, err: errors.New("planted read error")}
	}
	t.Cleanup(func() { flosendoWrapStdout = old })
	lb := captureLog(t)
	dir := t.TempDir()
	cs := newSession1943(t, fake1943{fixtures: []string{"fresh_stderr.jsonl"}, dir: dir})
	pid := pidOf(cs)
	c := collect(cs)
	if err := cs.Send("first", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cs.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("the session was not closed after its reader failed:\n%s\n%s", dumpRouted(c.snapshot()), notable(lb))
	}
	time.Sleep(300 * time.Millisecond) // the collector reads the last events after the channels close
	if !strings.Contains(notable(lb), "a line the CLI wrote to its stderr") {
		t.Fatalf("the stopped CLI's stderr never reached the bridge, so its error post is not exercised:\n%s", notable(lb))
	}
	got := c.snapshot()
	if e := errorsIn(got, "engine"); len(e) != 1 || e[0] != "read stdout: planted read error; the session was closed" {
		t.Fatalf("want the waiting turn ended by one error, got %q:\n%s", e, dumpRouted(got))
	}
	if e := errorsIn(got, "wakeup"); len(e) != 0 {
		t.Fatalf("an error on the wakeup channel would post a failed wakeup: %q", e)
	}
	if n := noticesIn(got); len(n) != 1 || !strings.HasPrefix(n[0], "wakeup: ❌ Session closed (") {
		t.Fatalf("want the closed notice once, got %q", n)
	}
	if cs.Alive() {
		t.Fatal("the session is still alive")
	}
	if err := cs.Send("second", "", nil, nil); err == nil {
		t.Fatal("Send accepted a message for a session nobody reads")
	}
	line := waitExitLine(t, lb, pid, 10*time.Second)
	if v, _ := field(line, "requested"); v != "true" {
		t.Fatalf("the process's end was not the bridge's doing: %s", line)
	}
}

// Round 1's review note: when the CLI exits while a child of it still holds its stdout, Wait closes the pipe, and the
// reader's error can come just before Wait's channel closes. The reader's end counts only if the CLI still runs after
// the grace, so a process that ends on its own (here 0.3 s after the read error) gets no closed notice: its end is
// not the bridge's doing (the 1852 line: requested=false).
func TestFlosendo1943_ReaderErrorThenExitIsNoReaderEnd(t *testing.T) {
	old := flosendoWrapStdout
	flosendoWrapStdout = func(r io.Reader) io.Reader {
		return &failAfterLines{r: r, lines: 6, err: errors.New("read |0: file already closed")}
	}
	t.Cleanup(func() { flosendoWrapStdout = old })
	lb := captureLog(t)
	cs := newSession1943(t, fake1943{fixtures: []string{"exit_after.jsonl"}, dir: t.TempDir()})
	pid := pidOf(cs)
	c := collect(cs)
	if err := cs.Send("first", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cs.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("the session did not end after its CLI exited:\n%s\n%s", dumpRouted(c.snapshot()), notable(lb))
	}
	got := c.snapshot()
	if n := noticesIn(got); len(n) != 0 {
		t.Fatalf("a CLI that ended on its own got a notice: %q", n)
	}
	if l := logsWith(lb, "flosendo: the CLI's output stopped being read while it runs; closing the session"); len(l) != 0 {
		t.Fatalf("a CLI that ended on its own was closed as deaf: %q", l)
	}
	if r := results(got, "engine"); len(r) != 1 || r[0].Content != "BYE" {
		t.Fatalf("the turn's reply:\n%s", dumpRouted(got))
	}
	line := waitExitLine(t, lb, pid, 10*time.Second)
	if v, _ := field(line, "requested"); v != "false" {
		t.Fatalf("the CLI's own end read as the bridge's: %s", line)
	}
}

// QA probe 2 (stream A eden-e5, EDEN-1943 QA 3c489bf0), adopted as written: the reader fails after "queued" and
// "started" only (our message taken in, no event routed yet, so turnChannel is nil); the engine's waiting turn must
// still end through the error path. QA's plant Q1 (waits from turnChannel only) stayed green on the builder's tests.
func TestFlosendo1943_ReaderErrorAfterStartedEndsTheWaitingTurn(t *testing.T) {
	old := flosendoWrapStdout
	flosendoWrapStdout = func(r io.Reader) io.Reader {
		return &failAfterLines{r: r, lines: 2, err: errors.New("planted read error")}
	}
	t.Cleanup(func() { flosendoWrapStdout = old })
	lb := captureLog(t)
	cs := newSession1943(t, fake1943{fixtures: []string{"fresh.jsonl"}, dir: t.TempDir()})
	c := collect(cs)
	if err := cs.Send("first", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cs.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("not closed:\n%s\n%s", dumpRouted(c.snapshot()), notable(lb))
	}
	got := c.snapshot()
	t.Logf("ROUTED:\n%s", dumpRouted(got))
	if e := errorsIn(got, "engine"); len(e) != 1 || e[0] != "read stdout: planted read error; the session was closed" {
		t.Fatalf("want the waiting turn ended by one error, got %q", e)
	}
	if n := noticesIn(got); len(n) != 1 {
		t.Fatalf("want the closed notice once, got %q", n)
	}
}

// flosendoReadLines' edges, as bufio.Scanner read them where the line fits, and a skip where it does not.
func TestFlosendo1943_ReadLinesEdges(t *testing.T) {
	type skip struct {
		head string
		size int
	}
	read := func(t *testing.T, r io.Reader) ([]string, []skip, error) {
		var lines []string
		var skips []skip
		err := flosendoReadLines(r, func(l []byte) { lines = append(lines, string(l)) },
			func(h []byte, n int) { skips = append(skips, skip{string(h), n}) })
		return lines, skips, err
	}
	t.Run("small cap", func(t *testing.T) {
		setLineCap1943(t, 100)
		over := `{"type":"user","x":"` + strings.Repeat("y", 300) + `"}`
		in := "a\r\n\n" + strings.Repeat("x", 100) + "\n" + over + "\nafter\nlast"
		lines, skips, err := read(t, strings.NewReader(in))
		want := []string{"a", "", strings.Repeat("x", 100), "after", "last"}
		if err != nil || strings.Join(lines, "|") != strings.Join(want, "|") {
			t.Fatalf("lines %q, err %v; want %q", lines, err, want)
		}
		if len(skips) != 1 || skips[0].size != len(over) || skips[0].head != over || flosendoLineType([]byte(skips[0].head)) != "user" {
			t.Fatalf("skips %+v, want one of %d bytes", skips, len(over))
		}
	})
	t.Run("a skipped line keeps 4 KB, and one at EOF is still told", func(t *testing.T) {
		setLineCap1943(t, 100)
		lines, skips, err := read(t, strings.NewReader("ok\n"+strings.Repeat("z", 10000)))
		if err != nil || len(lines) != 1 || len(skips) != 1 || len(skips[0].head) != flosendoLineHead || skips[0].size != 10000 {
			t.Fatalf("lines %q, skips %d, err %v", lines, len(skips), err)
		}
	})
	t.Run("a line longer than the read buffer is kept whole", func(t *testing.T) {
		long := strings.Repeat("w", 200000)
		lines, skips, err := read(t, strings.NewReader(long+"\nend\n"))
		if err != nil || len(skips) != 0 || len(lines) != 2 || lines[0] != long || lines[1] != "end" {
			t.Fatalf("%d lines, %d skips, err %v", len(lines), len(skips), err)
		}
	})
	t.Run("a read error drops the partial line and is returned", func(t *testing.T) {
		boom := errors.New("boom")
		lines, _, err := read(t, io.MultiReader(strings.NewReader("one\ntw"), &failAfterLines{lines: 0, err: boom}))
		if !errors.Is(err, boom) || len(lines) != 1 || lines[0] != "one" {
			t.Fatalf("lines %q, err %v", lines, err)
		}
	})
}

func TestFlosendo1943_LineTypeFromHead(t *testing.T) {
	for head, want := range map[string]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"xx`: "assistant",
		`{"session_id":"s","subtype":"success","type":"result","result":"xx`:  "result",
		`{"message":{"type":"inner"},"type":"user"}`:                          "user",
		`{"message":{"content":"xxxxxxxx`:                                     "",
		`not json`:                                                            "",
		`["type","user"]`:                                                     "",
	} {
		if got := flosendoLineType([]byte(head)); got != want {
			t.Fatalf("flosendoLineType(%q) = %q, want %q", head, got, want)
		}
	}
}
