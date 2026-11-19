//go:build !windows

package claudecode

// EDEN-1943, contract 52ea6a01 with the chief of staff's rulings (PLAN v1577). Ivan, 18 Nov: "promote 1943". On 15 Nov
// the chief of staff read the brochure PDF Ivan had attached; the tool result came back as one 13.6 MB line, over the
// read loop's 10 MB cap. The loop stopped ("bufio.Scanner: token too long"), the session was never closed, and every
// reply after it was lost, each new message answered by a false "Not processed" notice (10, 11:42Z to 16:01Z).
//
// The engine-level checks, which compile on the code before the fix (1f27375a) and are red there:
//   1   a CLI line of any size under the cap does not stop the reader: the turn's reply is posted, with no notice, and
//       the next message is answered (OversizedLineKeepsReading)
//   4,5 the reader ends while the CLI runs: the channel is told and the session is closed (the process stopped, the
//       1852 exit line requested=true); no false "Not processed"; the next message gets a fresh session and its reply
//       (ReaderEndStopsTheSession)
//   6   a pin: a message taken into a turn a peer started is that turn's reply, with no notice (1137's move, live 7
//       times 11-17 Nov, 5 in stream-started turns) (PeerStartedTurnTakesSlackMessage)
// The checks that need the new code's names are in flosendo_read_line_unit_test.go.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// TestFlosendo1943Helper is the fake claude (only when EDEN1943_HELPER=1). The k-th process the bridge starts replays
// the k-th fixture of EDEN1943_FIXTURES (comma-separated; the last one again after that), claiming k by creating
// EDEN1943_SIGNAL_DIR/start-<k> and writing its pid to pid-<k>. Rows as EDEN-1137's fake ("in" waits for the bridge's
// next stdin line and touches p<k>-waiting-<n>; "out" is written after its gap times EDEN1943_SCALE), plus:
//
//	big       the line, its "$PAD" string filled with 'x' so the whole line is n bytes (generated, never copied)
//	closeout  it closes its stdout, touches p<k>-closed, and reads stdin until the bridge closes it, then exits 0
//	exit      it exits 0 (after its gap), stdout open to the last byte
//	stderr    it writes the row's line to its stderr, and from then on ends with status 1 (a CLI that fails as it stops)
//
// After the last row it reads stdin until it closes, then exits 0 (1 after a stderr row). Whatever happens, it exits 3
// after 30 s.
func TestFlosendo1943Helper(t *testing.T) {
	if os.Getenv("EDEN1943_HELPER") != "1" {
		return
	}
	go func() {
		time.Sleep(30 * time.Second)
		os.Exit(3)
	}()
	dir := os.Getenv("EDEN1943_SIGNAL_DIR")
	fixtures := strings.Split(os.Getenv("EDEN1943_FIXTURES"), ",")
	k := 1
	for ; ; k++ {
		f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("start-%d", k)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			break
		}
	}
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("pid-%d", k)), []byte(strconv.Itoa(os.Getpid())), 0o644)
	rows := readFixture(t, fixtures[min(k, len(fixtures))-1])
	scale, err := strconv.ParseFloat(os.Getenv("EDEN1943_SCALE"), 64)
	if err != nil {
		scale = 0.01
	}
	in := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var m struct {
				Type string `json:"type"`
				UUID string `json:"uuid"`
			}
			if json.Unmarshal(sc.Bytes(), &m) == nil && m.Type == "user" {
				in <- m.UUID
			}
		}
		close(in)
	}()
	var uuids []string
	prev, code := 0.0, 0
	w := bufio.NewWriter(os.Stdout)
	for _, r := range rows {
		if r.Dir == "in" {
			_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("p%d-waiting-%d", k, r.N)), nil, 0o644)
			u, ok := <-in
			if !ok {
				os.Exit(code)
			}
			uuids = append(uuids, u)
			prev = r.T
			continue
		}
		if d := time.Duration((r.T - prev) * scale * float64(time.Second)); d > 0 {
			time.Sleep(d)
		}
		prev = r.T
		if r.Dir == "stderr" {
			_, _ = fmt.Fprintln(os.Stderr, string(r.Line))
			code = 1
			continue
		}
		if r.Dir == "exit" {
			_ = w.Flush()
			os.Exit(0)
		}
		if r.Dir == "closeout" {
			_ = w.Flush()
			_ = os.Stdout.Close()
			_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("p%d-closed", k)), nil, 0o644)
			for range in {
			}
			os.Exit(0)
		}
		line := string(r.Line)
		for i := len(uuids); i >= 1; i-- {
			line = strings.ReplaceAll(line, fmt.Sprintf(`"$IN%d"`, i), strconv.Quote(uuids[i-1]))
		}
		if r.Dir == "big" {
			pad := r.N - (len(line) - len(`"$PAD"`)) - 2
			line = strings.Replace(line, `"$PAD"`, `"`+strings.Repeat("x", pad)+`"`, 1)
			if len(line) != r.N {
				_, _ = fmt.Fprintf(os.Stderr, "big row: %d bytes, want %d\n", len(line), r.N)
				os.Exit(4)
			}
		}
		_, _ = w.WriteString(line + "\n")
		_ = w.Flush()
	}
	for range in {
	}
	os.Exit(code)
}

type fake1943 struct {
	fixtures []string // file names in testdata/eden1943, one per process start
	dir      string   // the signal dir
}

func (f fake1943) env() map[string]string {
	var paths []string
	for _, n := range f.fixtures {
		p, _ := filepath.Abs(filepath.Join("testdata", "eden1943", n))
		paths = append(paths, p)
	}
	return map[string]string{"EDEN1943_HELPER": "1", "EDEN1943_FIXTURES": strings.Join(paths, ","),
		"EDEN1943_SIGNAL_DIR": f.dir, "EDEN1943_SCALE": "0.01"}
}

// newEngine1943 wires a real Engine, the real claudecode agent and the fake, as EDEN-1137's newReplayEngine does.
func newEngine1943(t *testing.T, f fake1943) *capPlatform {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ag, err := New(map[string]any{"work_dir": t.TempDir(), "cmd": os.Args[0] + " -test.run=^TestFlosendo1943Helper$ --",
		"mode": "bypassPermissions", "env": f.env(), "cc_data_dir": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p := newCapPlatform()
	e := core.NewEngine("eden1943", ag, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	<-p.started
	return p
}

func fakePid1943(t *testing.T, dir string, k int) int {
	t.Helper()
	waitFile(t, filepath.Join(dir, fmt.Sprintf("pid-%d", k)), 10*time.Second)
	b, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("pid-%d", k)))
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatalf("pid-%d: %q", k, b)
	}
	return pid
}

const (
	notProcessed1943 = "Not processed"
	skipNotice1943   = "Not read (too long"
	closedNotice1943 = "Session closed ("
)

// Item 1: 15 Nov's line, 13,599,627 bytes, is read like any other: the reply after it is posted once, nothing says a
// line was skipped or a message not processed, and the next message is answered by the same process.
func TestFlosendo1943_OversizedLineKeepsReading(t *testing.T) {
	old := notTakenQuiet
	notTakenQuiet = 400 * time.Millisecond
	t.Cleanup(func() { notTakenQuiet = old })
	dir := t.TempDir()
	p := newEngine1943(t, fake1943{fixtures: []string{"big_tool_result.jsonl"}, dir: dir})
	p.send(1, "read the brochure")
	// 60 s: under -race on a loaded 2-vCPU box, parsing the 13.6 MB line took over 15 s (19 Nov 03:58Z, load 6.8).
	p.waitPost(t, "the reply after the 13.6 MB line", 60*time.Second, func(s []string) bool {
		return countWith(s, "AFTER-BIG") > 0
	})
	waitFile(t, filepath.Join(dir, "p1-waiting-2"), 10*time.Second)
	p.send(2, "and the next one")
	p.waitPost(t, "the next message's reply", 10*time.Second, func(s []string) bool { return countWith(s, "SECOND-OK") > 0 })
	time.Sleep(notTakenQuiet + 300*time.Millisecond)
	posts := p.all()
	if n := countWith(posts, "AFTER-BIG"); n != 1 {
		t.Fatalf("the reply after the big line was posted %d times: %q", n, posts)
	}
	for _, bad := range []string{notProcessed1943, skipNotice1943, closedNotice1943, "token too long", "read stdout"} {
		if n := countWith(posts, bad); n != 0 {
			t.Fatalf("%q was posted for a line under the cap: %q", bad, posts)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "start-2")); err == nil {
		t.Fatalf("a second process was started: the first one's session did not carry the next message")
	}
}

// Items 4 and 5 (the chief of staff's condition on C-3): the CLI closes its stdout and lives on, so the bridge's
// reader ends while the CLI runs (15 Nov's state, reached here without the 10 MB cap). The channel is told once, the
// process is stopped by the bridge (the 1852 line: requested=true), no "Not processed" notice is posted, and the next
// message is answered by a fresh session.
func TestFlosendo1943_ReaderEndStopsTheSession(t *testing.T) {
	old := notTakenQuiet
	notTakenQuiet = 400 * time.Millisecond
	t.Cleanup(func() { notTakenQuiet = old })
	lb := captureLog(t)
	dir := t.TempDir()
	p := newEngine1943(t, fake1943{fixtures: []string{"closeout.jsonl", "fresh.jsonl"}, dir: dir})
	p.send(1, "first")
	p.waitPost(t, "the first reply", 10*time.Second, func(s []string) bool { return countWith(s, "FIRST-OK") > 0 })
	pid := fakePid1943(t, dir, 1)
	waitFile(t, filepath.Join(dir, "p1-closed"), 10*time.Second)
	p.waitPost(t, "the notice that the session was closed", 15*time.Second, func(s []string) bool {
		return countWith(s, closedNotice1943) > 0
	})
	deadline := time.Now().Add(15 * time.Second)
	var exits []string
	for time.Now().Before(deadline) {
		if exits = exitLines(lb, pid); len(exits) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(exits) != 1 {
		t.Fatalf("want one %q line for pid %d after the reader ended, got %d; INFO and above:\n%s", exitMsg, pid, len(exits), notable(lb))
	}
	if v, _ := field(exits[0], "requested"); v != "true" {
		t.Fatalf("the process's end was not the bridge's doing: %s", exits[0])
	}
	p.send(2, "second")
	p.waitPost(t, "the fresh session's reply", 15*time.Second, func(s []string) bool { return countWith(s, "SECOND-OK") > 0 })
	time.Sleep(notTakenQuiet + 300*time.Millisecond)
	posts := p.all()
	if n := countWith(posts, closedNotice1943); n != 1 {
		t.Fatalf("the closed notice was posted %d times: %q", n, posts)
	}
	if n := countWith(posts, notProcessed1943); n != 0 {
		t.Fatalf("a false %q notice: %q", notProcessed1943, posts)
	}
	if _, err := os.Stat(filepath.Join(dir, "start-2")); err != nil {
		t.Fatalf("the next message did not get a fresh process: %v", err)
	}
}

// Item 6, a pin: a turn a peer's message started (no task notification, no lifecycle of its own) takes our message in
// at a tool call; our answer is that turn's reply, posted once without the wakeup header, and nothing says "Not
// processed" (1137's move; 17 Nov 11:12:05Z live).
func TestFlosendo1943_PeerStartedTurnTakesSlackMessage(t *testing.T) {
	old := notTakenQuiet
	notTakenQuiet = 400 * time.Millisecond
	t.Cleanup(func() { notTakenQuiet = old })
	sig := t.TempDir()
	fixture, _ := filepath.Abs(filepath.Join("testdata", "eden1943", "peer_started.jsonl"))
	p := newReplayEngine(t, fakeCLI{fixture: fixture, scale: 0.01, signalDir: sig})
	p.send(1, "first")
	p.waitPost(t, "the first reply", 10*time.Second, func(s []string) bool { return countWith(s, "STARTED") == 1 })
	waitFile(t, filepath.Join(sig, "waiting-2"), 10*time.Second)
	p.send(2, "Reply with the single word THIRD.")
	p.waitPost(t, "the answer inside the peer's turn", 10*time.Second, func(s []string) bool { return countWith(s, "THIRD") > 0 })
	time.Sleep(notTakenQuiet + 300*time.Millisecond)
	posts := p.all()
	if r := postWith(posts, "THIRD"); strings.Contains(r, wakeupHeader) || countWith(posts, "THIRD") != 1 {
		t.Fatalf("our answer was not posted once as the reply: %q", posts)
	}
	if n := countWith(posts, notProcessed1943); n != 0 {
		t.Fatalf("a %q notice for a message the CLI took in: %q", notProcessed1943, posts)
	}
}
