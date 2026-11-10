//go:build !windows

package claudecode

// EDEN-1852 slice 2: the bridge logs every agent exit. Ivan, 7 Nov: "promote 1852". On 6 Nov one session's runaway
// test ended all three agent sessions at 19:41Z and the bridge wrote no line at all until 22:15Z: an idle session's
// process that dies says nothing (finishReadLoop logs only when stderr has text), and A, in a turn the CLI had started
// itself after a background task's notification, said nothing either (the engine logs "agent process exited" only
// from its own turn loop, which had ended at 19:22Z). The contract's checks, one test each:
//   12 a process killed by a signal with nothing on stderr logs one INFO line, signal=killed requested=false
//   13 an idle session's process that dies logs the same line, in_turn=false
//   14 6 Nov's A: a turn the CLI started itself, killed at its tool call, logs in_turn=true turn=wakeup, and the turn
//      ends (the partial turn is posted as a wakeup); an engine turn killed the same way logs turn=engine, and the
//      engine's own "agent process exited" follows
//   15 a requested close (stdin close, SIGTERM, force kill) logs requested=true; an exit 0 nobody asked for, false
//   and a bridge stop (the engine's context cancelled, which kills every session) is requested: KillMode=control-group
//   signals the processes with the bridge, and S1's guard reads this line first, so a stop read as "on its own" would
//   post a false alarm at every restart
// The line is read with the guard's own field rule (scripts/ops/dev-box-guard/dev_box_guard.py how_it_ended, via
// handover_check.py field): msg "flosendo: agent exit", pid=, requested=true|false, signal= or status=.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const exitMsg = "flosendo: agent exit"

// TestFlosendo1852Helper is the fake claude (only when EDEN1852_HELPER=1). It writes its own CC_SESSION_KEY to
// EDEN1852_KEY_FILE, replays EDEN1852_FIXTURE as EDEN-1137's fake does (an "in" row waits for the bridge's next stdin
// line; an "out" row is written after its recorded gap times EDEN1852_SCALE), then ends as EDEN1852_END says:
//
//	die:KILL, die:TERM  it kills itself with that signal, writing nothing to stderr
//	exit:<n>            it exits with that status on its own
//	wait                it reads stdin until the bridge closes it, then exits 0
//	hold, hold-term     it ignores the stdin close (hold-term: and SIGTERM) until the bridge signals it
//	trap-term           as hold, but a SIGTERM, SIGINT or SIGHUP makes it exit 0, as the real CLI does (2.1.286)
//	stderr-exit:<n>     it writes an error to stderr, then exits with that status
//
// EDEN1852_LINGER_MS makes it wait that long after the last row before its ending.
//
// Whatever the ending, it exits 3 after 20 s, so no fake outlives its test. It starts no process.
func TestFlosendo1852Helper(t *testing.T) {
	if os.Getenv("EDEN1852_HELPER") != "1" {
		return
	}
	go func() {
		time.Sleep(20 * time.Second)
		os.Exit(3)
	}()
	end := os.Getenv("EDEN1852_END")
	if end == "hold-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	if end == "trap-term" {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		go func() {
			<-c
			os.Exit(0)
		}()
	}
	if f := os.Getenv("EDEN1852_KEY_FILE"); f != "" {
		_ = os.WriteFile(f, []byte(os.Getenv("CC_SESSION_KEY")), 0o644)
	}
	rows := readFixture(t, os.Getenv("EDEN1852_FIXTURE"))
	scale, err := strconv.ParseFloat(os.Getenv("EDEN1852_SCALE"), 64)
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
	prev := 0.0
	w := bufio.NewWriter(os.Stdout)
	for _, r := range rows {
		if r.Dir == "in" {
			u, ok := <-in
			if !ok {
				os.Exit(0)
			}
			uuids = append(uuids, u)
			prev = r.T
			continue
		}
		if d := time.Duration((r.T - prev) * scale * float64(time.Second)); d > 0 {
			time.Sleep(d)
		}
		prev = r.T
		line := string(r.Line)
		for i := len(uuids); i >= 1; i-- {
			line = strings.ReplaceAll(line, fmt.Sprintf(`"$IN%d"`, i), strconv.Quote(uuids[i-1]))
		}
		_, _ = w.WriteString(line + "\n")
		_ = w.Flush()
	}
	if ms, err := strconv.Atoi(os.Getenv("EDEN1852_LINGER_MS")); err == nil {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	switch {
	case end == "die:KILL":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	case end == "die:TERM":
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	case strings.HasPrefix(end, "stderr-exit:"):
		_, _ = os.Stderr.WriteString("API Error: 529 overloaded_error\n")
		n, _ := strconv.Atoi(strings.TrimPrefix(end, "stderr-exit:"))
		os.Exit(n)
	case strings.HasPrefix(end, "exit:"):
		n, _ := strconv.Atoi(strings.TrimPrefix(end, "exit:"))
		os.Exit(n)
	case end == "wait":
		for range in {
		}
		os.Exit(0)
	default: // hold, hold-term, trap-term
		for range in {
		}
	}
	select {}
}

type exitFake struct {
	fixture string
	end     string
	keyFile string
	linger  time.Duration
}

func (f exitFake) env() map[string]string {
	p, _ := filepath.Abs(filepath.Join("testdata", "eden1852", f.fixture))
	return map[string]string{"EDEN1852_HELPER": "1", "EDEN1852_FIXTURE": p, "EDEN1852_SCALE": "0.01",
		"EDEN1852_END": f.end, "EDEN1852_KEY_FILE": f.keyFile, "EDEN1852_LINGER_MS": strconv.Itoa(int(f.linger.Milliseconds()))}
}

// newExitSession starts a real claudeSession over the fake, as the engine would with CC_SESSION_KEY=key. The bridge's
// own environment carries a different key, so a line that read the bridge's instead of the process's would show it.
func newExitSession(t *testing.T, f exitFake, key string) (*claudeSession, context.CancelFunc) {
	t.Helper()
	t.Setenv("CC_SESSION_KEY", "the-bridge-own-env")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	extraEnv := []string{"CC_SESSION_KEY=" + key}
	for k, v := range f.env() {
		extraEnv = append(extraEnv, k+"="+v)
	}
	cs, err := newClaudeSession(ctx, t.TempDir(), os.Args[0], []string{"-test.run=^TestFlosendo1852Helper$", "--"}, "",
		"", "", "", "bypassPermissions", "", "", nil, nil, nil, extraEnv, "", false, core.SpawnOptions{}, 0, t.TempDir(), "")
	if err != nil {
		t.Fatalf("newClaudeSession: %v", err)
	}
	cs.gracefulStopTimeout = 200 * time.Millisecond
	t.Cleanup(func() { _ = cs.Close() })
	return cs, cancel
}

// newExitEngine wires a real Engine, the real claudecode agent and the fake; the engine sets CC_SESSION_KEY itself.
func newExitEngine(t *testing.T, f exitFake) (*core.Engine, *capPlatform) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CC_SESSION_KEY", "the-bridge-own-env")
	ag, err := New(map[string]any{"work_dir": t.TempDir(), "cmd": os.Args[0] + " -test.run=^TestFlosendo1852Helper$ --",
		"mode": "bypassPermissions", "env": f.env(), "cc_data_dir": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p := newCapPlatform()
	e := core.NewEngine("eden1852", ag, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	<-p.started
	return e, p
}

// logBuf captures the bridge's log as the bridge writes it (slog's text handler, every level).
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Split(strings.TrimRight(l.b.String(), "\n"), "\n")
}

func captureLog(t *testing.T) *logBuf {
	t.Helper()
	old := slog.Default()
	lb := &logBuf{}
	slog.SetDefault(slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return lb
}

// field is the guard's rule (handover_check.py field): name=value, a value quoted the way slog quotes it.
func field(line, name string) (string, bool) {
	m := regexp.MustCompile(`(?:^| )` + regexp.QuoteMeta(name) + `=("(?:[^"\\]|\\.)*"|\S+)`).FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	if v, err := strconv.Unquote(m[1]); err == nil && strings.HasPrefix(m[1], `"`) {
		return v, true
	}
	return m[1], true
}

func msgIs(line, msg string) bool {
	v, ok := field(line, "msg")
	return ok && v == msg
}

// exitLines are the exit lines naming pid.
func exitLines(lb *logBuf, pid int) []string {
	var out []string
	for _, l := range lb.lines() {
		if v, _ := field(l, "pid"); msgIs(l, exitMsg) && v == strconv.Itoa(pid) {
			out = append(out, l)
		}
	}
	return out
}

// notable is what the bridge logged at INFO and above: on today's code, the failure message shows that nothing at
// all was logged for the process's end (the reproduction of 6 Nov).
func notable(lb *logBuf) string {
	var b strings.Builder
	for _, l := range lb.lines() {
		if lv, _ := field(l, "level"); lv != "DEBUG" {
			b.WriteString("  " + l + "\n")
		}
	}
	return b.String()
}

func waitExitLine(t *testing.T, lb *logBuf, pid int, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ls := exitLines(lb, pid); len(ls) > 0 {
			time.Sleep(100 * time.Millisecond) // a second line would show here
			if ls = exitLines(lb, pid); len(ls) != 1 {
				t.Fatalf("process %d's end was logged %d times:\n%s", pid, len(ls), strings.Join(ls, "\n"))
			}
			return ls[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %q line for process %d within %s; what the bridge logged at INFO and above:\n%s", exitMsg, pid, d, notable(lb))
	return ""
}

// linesOut appends each checked line to EDEN1852_LINES_OUT when it is set, so the landed guard's own reader can read
// what the bridge writes (the QA's cross-check, outside this repository).
func linesOut(line string) {
	if p := os.Getenv("EDEN1852_LINES_OUT"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.WriteString(line + "\n")
			_ = f.Close()
		}
	}
}

func want(t *testing.T, line string, kv ...string) {
	t.Helper()
	linesOut(line)
	for i := 0; i < len(kv); i += 2 {
		got, ok := field(line, kv[i])
		if kv[i+1] == "-" {
			if ok {
				t.Errorf("%s=%s should not be in the line: %s", kv[i], got, line)
			}
			continue
		}
		if !ok || got != kv[i+1] {
			t.Errorf("%s: got %q (present %v), want %q in: %s", kv[i], got, ok, kv[i+1], line)
		}
	}
}

func wantRan(t *testing.T, line string) {
	t.Helper()
	v, _ := field(line, "ran")
	if d, err := time.ParseDuration(v); err != nil || d < 0 || d > time.Minute {
		t.Errorf("ran=%q is not how long the process ran: %s", v, line)
	}
}

func pidOf(cs *claudeSession) int { return cs.cmd.Process.Pid }

func turnDone(c *collector) func([]routed) bool {
	return func(s []routed) bool { return len(results(s, "engine")) == 1 }
}

func waitClosed(t *testing.T, cs *claudeSession) {
	t.Helper()
	select {
	case <-cs.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the session's channels never closed")
	}
}

// Items 12 and 13 (6 Nov's B and the chief of staff): an idle session's process killed by a signal, nothing on stderr.
func TestFlosendo1852_IdleSessionKilledBySignalIsLogged(t *testing.T) {
	lb := captureLog(t)
	cs, _ := newExitSession(t, exitFake{fixture: "idle_after_turn.jsonl", end: "die:KILL"}, "slack:C0ASQ5LPS4W")
	c := collect(cs)
	if err := cs.Send("start the background job", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "the turn's result", 10*time.Second, turnDone(c))
	line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
	want(t, line, "level", "INFO", "session_key", "slack:C0ASQ5LPS4W", "signal", "killed", "status", "-",
		"requested", "false", "in_turn", "false", "turn", "-")
	wantRan(t, line)
	waitClosed(t, cs)
}

// Item 14, 6 Nov's A, through the real engine: its engine turn had ended; a background task's notification made the
// CLI start a turn itself, and the process died at that turn's tool call. Today nothing at all is logged.
func TestFlosendo1852_SixNovember_CLIStartedTurnKilledIsLogged(t *testing.T) {
	lb := captureLog(t)
	keyFile := filepath.Join(t.TempDir(), "key")
	_, p := newExitEngine(t, exitFake{fixture: "wakeup_midtool.jsonl", end: "die:KILL", keyFile: keyFile})
	p.send(1, "start the background job")
	p.waitPost(t, "the engine turn's reply", 10*time.Second, func(s []string) bool { return countWith(s, "STARTED") == 1 })
	// The turn ends: the part the CLI started is posted as a wakeup once its process is gone.
	p.waitPost(t, "the CLI-started turn, posted as a wakeup", 10*time.Second, func(s []string) bool { return countWith(s, wakeupHeader) == 1 })
	key, err := os.ReadFile(keyFile)
	if err != nil || len(key) == 0 {
		t.Fatalf("the fake never wrote its session key: %v", err)
	}
	var line string
	deadline := time.Now().Add(5 * time.Second)
	for line == "" && time.Now().Before(deadline) {
		for _, l := range lb.lines() {
			if msgIs(l, exitMsg) {
				line = l
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if line == "" {
		t.Fatalf("no %q line after the CLI-started turn's process died; what the bridge logged at INFO and above:\n%s", exitMsg, notable(lb))
	}
	want(t, line, "session_key", string(key), "signal", "killed", "requested", "false", "in_turn", "true", "turn", "wakeup")
	wantRan(t, line)
}

// Item 14, an engine turn: the line says turn=engine, and the engine's own exit line follows (the turn ends).
func TestFlosendo1852_EngineTurnKilledIsLogged(t *testing.T) {
	lb := captureLog(t)
	keyFile := filepath.Join(t.TempDir(), "key")
	_, p := newExitEngine(t, exitFake{fixture: "engine_midtool.jsonl", end: "die:KILL", keyFile: keyFile})
	p.send(1, "start the background job")
	deadline := time.Now().Add(10 * time.Second)
	var mine, engines int
	var line string
	for time.Now().Before(deadline) {
		mine, engines = 0, 0
		for _, l := range lb.lines() {
			if msgIs(l, exitMsg) {
				mine++
				line = l
			}
			if msgIs(l, "agent process exited") {
				engines++
			}
		}
		if mine > 0 && engines > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mine != 1 || engines != 1 {
		t.Fatalf("want one %q and one engine \"agent process exited\", got %d and %d; INFO and above:\n%s", exitMsg, mine, engines, notable(lb))
	}
	key, _ := os.ReadFile(keyFile)
	want(t, line, "session_key", string(key), "signal", "killed", "requested", "false", "in_turn", "true", "turn", "engine")
}

// Item 15: an exit 0 nobody asked for is not requested.
func TestFlosendo1852_ExitZeroNobodyAskedFor(t *testing.T) {
	lb := captureLog(t)
	cs, _ := newExitSession(t, exitFake{fixture: "idle_after_turn.jsonl", end: "exit:0"}, "slack:C0ADAUV6666")
	c := collect(cs)
	if err := cs.Send("start the background job", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "the turn's result", 10*time.Second, turnDone(c))
	line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
	want(t, line, "status", "0", "signal", "-", "requested", "false", "in_turn", "false")
}

// Item 15: each step of a requested close (stdin close, SIGTERM, force kill) is requested.
func TestFlosendo1852_RequestedClosesAreRequested(t *testing.T) {
	for _, tc := range []struct{ name, end, kv string }{
		{"stdin close", "wait", "status=0"},
		{"SIGTERM", "hold", "signal=terminated"},
		{"force kill", "hold-term", "signal=killed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lb := captureLog(t)
			cs, _ := newExitSession(t, exitFake{fixture: "idle_after_turn.jsonl", end: tc.end}, "slack:C0B1FCC755W")
			c := collect(cs)
			if err := cs.Send("start the background job", "", nil, nil); err != nil {
				t.Fatal(err)
			}
			c.waitFor(t, "the turn's result", 10*time.Second, turnDone(c))
			time.Sleep(100 * time.Millisecond) // the fake reaches its ending
			if err := cs.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			line := waitExitLine(t, lb, pidOf(cs), 15*time.Second)
			kv := strings.SplitN(tc.kv, "=", 2)
			want(t, line, "requested", "true", kv[0], kv[1], "session_key", "slack:C0B1FCC755W")
		})
	}
}

// The bridge stopping: Engine.Stop cancels the engine's context, which kills every session's process before the
// engine closes it (CommandContext). That end is the bridge's own doing.
func TestFlosendo1852_ContextCancelIsRequested(t *testing.T) {
	lb := captureLog(t)
	cs, cancel := newExitSession(t, exitFake{fixture: "idle_after_turn.jsonl", end: "hold"}, "slack:C0ASQ5LPS4W")
	c := collect(cs)
	if err := cs.Send("start the background job", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "the turn's result", 10*time.Second, turnDone(c))
	cancel()
	line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
	want(t, line, "requested", "true", "signal", "killed")
}

// The same through the real engine: a stop logs every session's end as requested.
func TestFlosendo1852_EngineStopIsRequested(t *testing.T) {
	lb := captureLog(t)
	e, p := newExitEngine(t, exitFake{fixture: "idle_after_turn.jsonl", end: "hold"})
	p.send(1, "start the background job")
	p.waitPost(t, "the turn's reply", 10*time.Second, func(s []string) bool { return countWith(s, "STARTED") == 1 })
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	var lines []string
	deadline := time.Now().Add(10 * time.Second)
	for len(lines) == 0 && time.Now().Before(deadline) {
		for _, l := range lb.lines() {
			if msgIs(l, exitMsg) {
				lines = append(lines, l)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(lines) != 1 {
		t.Fatalf("want one %q line at the stop, got %d; INFO and above:\n%s", exitMsg, len(lines), notable(lb))
	}
	want(t, lines[0], "requested", "true")
}

// A SIGTERM from someone other than the bridge (a person's kill, with no stop) is not requested.
func TestFlosendo1852_TermNobodyAskedFor(t *testing.T) {
	lb := captureLog(t)
	cs, _ := newExitSession(t, exitFake{fixture: "idle_after_turn.jsonl", end: "die:TERM"}, "slack:C0ASQ5LPS4W")
	c := collect(cs)
	if err := cs.Send("start the background job", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "the turn's result", 10*time.Second, turnDone(c))
	line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
	want(t, line, "requested", "false", "signal", "terminated")
}

// Review round 1, note 1: the CLI opened a turn (its system init) and died before any event was routed: still mid-turn.
func TestFlosendo1852_DeathAtTheTurnsSystemInit(t *testing.T) {
	for _, tc := range []struct{ fixture, turn string }{{"engine_init.jsonl", "engine"}, {"wakeup_init.jsonl", "wakeup"}} {
		t.Run(tc.turn, func(t *testing.T) {
			lb := captureLog(t)
			cs, _ := newExitSession(t, exitFake{fixture: tc.fixture, end: "die:KILL"}, "slack:C0ADAUV6666")
			collect(cs)
			if err := cs.Send("start the background job", "", nil, nil); err != nil {
				t.Fatal(err)
			}
			line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
			want(t, line, "in_turn", "true", "turn", tc.turn, "signal", "killed", "requested", "false")
		})
	}
}

// Review round 1, note 2: a Close() that begins after the end was seen (a message meeting the dead process) did not
// cause it, even while the line waits to learn whether the bridge is stopping.
func TestFlosendo1852_CloseAfterTheEndIsNotItsCause(t *testing.T) {
	for _, end := range []string{"die:TERM", "exit:0"} {
		t.Run(end, func(t *testing.T) {
			lb := captureLog(t)
			cs, _ := newExitSession(t, exitFake{fixture: "idle_after_turn.jsonl", end: end}, "slack:C0ASQ5LPS4W")
			c := collect(cs)
			if err := cs.Send("start the background job", "", nil, nil); err != nil {
				t.Fatal(err)
			}
			c.waitFor(t, "the turn's result", 10*time.Second, turnDone(c))
			time.Sleep(300 * time.Millisecond) // the process has ended; its line may still be waiting for a stop
			go func() { _ = cs.Close() }()
			line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
			want(t, line, "requested", "false")
		})
	}
}

// QA round 1, finding 4: our message is in (the CLI took it, or, from a CLI that echoes no lifecycle, it is written and
// unanswered) and the process dies before the CLI's system init: the engine's turn is waiting, so a turn is open.
func TestFlosendo1852_DeathBeforeTheCLIsSystemInit(t *testing.T) {
	for _, fixture := range []string{"engine_started.jsonl", "engine_sent_nolc.jsonl"} {
		t.Run(fixture, func(t *testing.T) {
			lb := captureLog(t)
			cs, _ := newExitSession(t, exitFake{fixture: fixture, end: "die:KILL"}, "slack:C0ADAUV6666")
			collect(cs)
			if err := cs.Send("start the background job", "", nil, nil); err != nil {
				t.Fatal(err)
			}
			line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
			want(t, line, "in_turn", "true", "turn", "engine", "signal", "killed", "requested", "false")
		})
	}
}

// QA round 1, finding 2: a process that writes to stderr and exits mid-turn. The line is written before the stderr
// branch's error event ends the turn, so it still says the turn was open; the branch's own line is kept.
func TestFlosendo1852_StderrDeathMidTurn(t *testing.T) {
	lb := captureLog(t)
	cs, _ := newExitSession(t, exitFake{fixture: "engine_midtool.jsonl", end: "stderr-exit:1"}, "slack:C0ASQ5LPS4W")
	collect(cs)
	if err := cs.Send("start the background job", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
	want(t, line, "status", "1", "requested", "false", "in_turn", "true", "turn", "engine")
	failed := 0
	for _, l := range lb.lines() {
		if msgIs(l, "claudeSession: process failed") {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("the stderr branch logged %d \"process failed\" lines, want 1:\n%s", failed, notable(lb))
	}
}

// QA round 1, finding 3: ran is the process's life, measured when its end is seen, not after the wait for a stop
// (up to 2 s for an exit by status). The fake lives about 1.3 s.
func TestFlosendo1852_RanIsTheProcesssLife(t *testing.T) {
	lb := captureLog(t)
	cs, _ := newExitSession(t, exitFake{fixture: "idle_after_turn.jsonl", end: "exit:0", linger: 1200 * time.Millisecond}, "slack:C0ASQ5LPS4W")
	c := collect(cs)
	if err := cs.Send("start the background job", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "the turn's result", 10*time.Second, turnDone(c))
	line := waitExitLine(t, lb, pidOf(cs), 10*time.Second)
	v, _ := field(line, "ran")
	if d, err := time.ParseDuration(v); err != nil || d < time.Second || d > 2*time.Second {
		t.Fatalf("ran=%q, want the process's life (1s or 2s), not that plus the wait: %s", v, line)
	}
}
