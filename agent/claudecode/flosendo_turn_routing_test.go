package claudecode

// EDEN-1137, contract 4c7fee07 with the D0 findings 621cab04 and the chief of staff's calls (95053b0a, 7a599068).
// Ivan: "i want to be able to continue building even if i am not on the office" and "alright, continue all the build
// 24/7". A turn that waits 30 minutes for a reply that already went to the wakeup path, and is then killed, stops that.
//
// The fixtures are the real CLI's lines (2.1.286, haiku, D0), trimmed by minimize.py; a fake claude replays them and
// puts back the uuid the bridge wrote wherever a fixture says $IN<n>. Two are derived and say so in their header.
//
// Items (the contract's failure list): 1 ResumePhantomThenRealReply, 2 BypassEventsFollowOpenTurn, 3
// QuietCLIEndsWaitingTurn (folded in, never read), 4 WakeupThenOwnTurnKeepsBoth, 5 EachLineReachesOneChannel, 6
// TakenReplyHonoursNoReply, 7 (agent half) SettleTurn, 8 CompactionKeepsTurnChannel. Round 2 (the round-1 review's
// notes and a race of my own): QuietCheckPausesWhileATurnRuns, TerminalLifecycleWithoutStartEndsTurnAtOnce, and in the
// unit file NotTakenSignalCurrentUntilSettled and QuietPauseEndsAtEveryResult.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// ---- the fake claude ----------------------------------------------------------------------------------------------

type replayRow struct {
	T    float64         `json:"t"`
	Dir  string          `json:"dir"`
	N    int             `json:"n"`
	Line json.RawMessage `json:"line"`
}

func readFixture(t testing.TB, path string) []replayRow {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	defer f.Close()
	var rows []replayRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		var r replayRow
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("fixture line %q: %v", l, err)
		}
		rows = append(rows, r)
	}
	return rows
}

// TestFlosendo1137Helper is the fake claude (only when EDEN1137_HELPER=1). It replays EDEN1137_FIXTURE: an "in" row
// waits for the bridge's next stdin line (and touches EDEN1137_SIGNAL_DIR/waiting-<n> while it waits); an "out" row
// is written after its recorded gap times EDEN1137_SCALE. EDEN1137_NO_LIFECYCLE=1 drops command_lifecycle rows (a CLI
// that never echoes); EDEN1137_STATE replaces "$STATE". After the last row it reads stdin until it closes.
func TestFlosendo1137Helper(t *testing.T) {
	if os.Getenv("EDEN1137_HELPER") != "1" {
		return
	}
	rows := readFixture(t, os.Getenv("EDEN1137_FIXTURE"))
	scale, err := strconv.ParseFloat(os.Getenv("EDEN1137_SCALE"), 64)
	if err != nil {
		scale = 0.02
	}
	noLifecycle := os.Getenv("EDEN1137_NO_LIFECYCLE") == "1"
	signalDir := os.Getenv("EDEN1137_SIGNAL_DIR")
	state := os.Getenv("EDEN1137_STATE")
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
			if signalDir != "" {
				_ = os.WriteFile(filepath.Join(signalDir, fmt.Sprintf("waiting-%d", r.N)), nil, 0o644)
			}
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
		if noLifecycle && strings.Contains(line, `"type": "command_lifecycle"`) || noLifecycle && strings.Contains(line, `"type":"command_lifecycle"`) {
			continue
		}
		for i := len(uuids); i >= 1; i-- {
			line = strings.ReplaceAll(line, fmt.Sprintf(`"$IN%d"`, i), strconv.Quote(uuids[i-1]))
		}
		if state != "" {
			line = strings.ReplaceAll(line, `"$STATE"`, strconv.Quote(state))
		}
		_, _ = w.WriteString(line + "\n")
		_ = w.Flush()
	}
	for range in {
	}
	os.Exit(0)
}

type fakeCLI struct {
	fixture     string
	scale       float64
	noLifecycle bool
	signalDir   string
	state       string // replaces "$STATE" in the fixture (round 2: not_taken_terminal.jsonl)
}

func (f fakeCLI) cmd() string {
	return os.Args[0] + " -test.run=^TestFlosendo1137Helper$ --"
}

func (f fakeCLI) env() map[string]string {
	env := map[string]string{
		"EDEN1137_HELPER":     "1",
		"EDEN1137_FIXTURE":    f.fixture,
		"EDEN1137_SCALE":      strconv.FormatFloat(f.scale, 'f', -1, 64),
		"EDEN1137_SIGNAL_DIR": f.signalDir,
	}
	if f.noLifecycle {
		env["EDEN1137_NO_LIFECYCLE"] = "1"
	}
	if f.state != "" {
		env["EDEN1137_STATE"] = f.state
	}
	return env
}

func fixturePath(name string) string {
	p, _ := filepath.Abs(filepath.Join("testdata", "eden1137", name))
	return p
}

// newReplaySession starts a real claudeSession over the fake claude.
func newReplaySession(t *testing.T, f fakeCLI) *claudeSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var extraEnv []string
	for k, v := range f.env() {
		extraEnv = append(extraEnv, k+"="+v)
	}
	cs, err := newClaudeSession(ctx, t.TempDir(), os.Args[0], []string{"-test.run=^TestFlosendo1137Helper$", "--"}, "",
		"", "", "", "bypassPermissions", "", "", nil, nil, nil, extraEnv, "", false, core.SpawnOptions{}, 0, t.TempDir(), "")
	if err != nil {
		t.Fatalf("newClaudeSession: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// collector reads both channels of a session and keeps every event with its channel, in order.
type collector struct {
	mu   sync.Mutex
	evts []routed
}

type routed struct {
	ch  string // "engine" or "wakeup"
	evt core.Event
}

func collect(cs *claudeSession) *collector {
	c := &collector{}
	read := func(name string, ch <-chan core.Event) {
		for e := range ch {
			c.mu.Lock()
			c.evts = append(c.evts, routed{name, e})
			c.mu.Unlock()
		}
	}
	go read("engine", cs.events)
	go read("wakeup", cs.orphanEvents)
	return c
}

func (c *collector) snapshot() []routed {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]routed(nil), c.evts...)
}

func (c *collector) waitFor(t *testing.T, what string, d time.Duration, ok func([]routed) bool) []routed {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s := c.snapshot(); ok(s) {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	s := c.snapshot()
	t.Fatalf("timed out waiting for %s; events so far:\n%s", what, dumpRouted(s))
	return nil
}

func dumpRouted(rs []routed) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "  %-6s %-20s %q done=%v\n", r.ch, r.evt.Type, r.evt.Content, r.evt.Done)
	}
	return b.String()
}

func results(rs []routed, ch string) []core.Event {
	var out []core.Event
	for _, r := range rs {
		if r.ch == ch && r.evt.Type == core.EventResult {
			out = append(out, r.evt)
		}
	}
	return out
}

func waitFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the fake claude never reached %s", filepath.Base(path))
}

// ---- item 1 ------------------------------------------------------------------------------------------------------

// Item 1 (shape A, 2 Nov 09:18-09:51Z, #eden-dev): the first prompt into a resumed session. The CLI closes a synthetic
// task-notification turn first (origin.kind task-notification, num_turns 0, empty, zero usage); it must reach no
// channel, and the real reply must reach the engine's turn. Both with the lifecycle echoed and without it (counter).
func TestFlosendo1137_ResumePhantomThenRealReply(t *testing.T) {
	for _, noLC := range []bool{false, true} {
		t.Run(fmt.Sprintf("noLifecycle=%v", noLC), func(t *testing.T) {
			cs := newReplaySession(t, fakeCLI{fixture: fixturePath("s1b_resumed.jsonl"), scale: 0.05, noLifecycle: noLC})
			c := collect(cs)
			if err := cs.Send("Reply with the single word PONG.", "m1", nil, nil); err != nil {
				t.Fatal(err)
			}
			rs := c.waitFor(t, "the engine's result", 10*time.Second, func(rs []routed) bool { return len(results(rs, "engine")) > 0 })
			time.Sleep(100 * time.Millisecond)
			rs = c.snapshot()
			for _, r := range rs {
				if r.evt.Type == core.EventResult && r.evt.Content == "" {
					t.Fatalf("the synthetic empty result reached the %s channel:\n%s", r.ch, dumpRouted(rs))
				}
			}
			if got := results(rs, "engine"); len(got) != 1 || got[0].Content != "PONG" {
				t.Fatalf("engine results = %+v, want one PONG:\n%s", got, dumpRouted(rs))
			}
			if got := results(rs, "wakeup"); len(got) != 0 {
				t.Fatalf("a result went to the wakeup path:\n%s", dumpRouted(rs))
			}
		})
	}
}

// ---- item 2 ------------------------------------------------------------------------------------------------------

// Item 2 (shape A's second half): system lines and tool results used to skip the router and always reach the
// engine's channel, so a turn going to the wakeup path disarmed the engine's phantom grace and fed its idle timer for
// 30 minutes. They must follow the open turn's channel; with no turn open they go to the engine as before (counter),
// or to the wakeup path when the lifecycle shows no turn of ours is running.
func TestFlosendo1137_BypassEventsFollowOpenTurn(t *testing.T) {
	newCS := func() *claudeSession {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		cs := &claudeSession{events: make(chan core.Event, 16), orphanEvents: make(chan core.Event, 16), ctx: ctx}
		cs.alive.Store(true)
		return cs
	}
	sys := map[string]any{"type": "system", "subtype": "task_notification", "session_id": "s"}
	tool := map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "content": "ok"}}}}
	text := func(s string) map[string]any {
		return map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}}
	}

	t.Run("a turn open on the wakeup path takes its system lines and tool results", func(t *testing.T) {
		cs := newCS()
		cs.handleAssistant(text("woke up")) // inTurn 0: a self-fired turn, wakeup path
		<-cs.orphanEvents
		cs.inTurn.Store(1) // a Send arrives while it runs
		cs.handleSystem(sys)
		cs.handleUser(tool)
		if e := drainOne(cs.events); e != nil {
			t.Fatalf("a bypass line of the wakeup turn reached the engine's turn: %+v", e)
		}
		if a, b := drainOne(cs.orphanEvents), drainOne(cs.orphanEvents); a == nil || b == nil {
			t.Fatalf("the wakeup path did not get both bypass lines: %v %v", a, b)
		}
	})
	t.Run("no turn open, counter: the engine as before", func(t *testing.T) {
		cs := newCS()
		cs.handleSystem(sys)
		if e := drainOne(cs.events); e == nil || e.SessionID != "s" {
			t.Fatalf("the start-up system line did not reach the engine: %+v", e)
		}
	})
}

// ---- items 5 and 8 -----------------------------------------------------------------------------------------------

// Item 5: every line reaches exactly one channel, so a reply can be posted neither twice nor never. Replays the folded-in
// shape (s5) and checks each model line and result once, under -race -count=50 in the fork's gate.
func TestFlosendo1137_EachLineReachesOneChannel(t *testing.T) {
	sig := t.TempDir()
	cs := newReplaySession(t, fakeCLI{fixture: fixturePath("s5_midturn_self_tool.jsonl"), scale: 0.01, signalDir: sig})
	c := collect(cs)
	if err := cs.Send("start the background job", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(sig, "waiting-2"), 10*time.Second)
	if err := cs.Send("Reply with the single word THIRD.", "m2", nil, nil); err != nil {
		t.Fatal(err)
	}
	rs := c.waitFor(t, "the folded-in result", 10*time.Second, func(rs []routed) bool { return len(results(rs, "engine")) == 2 })
	time.Sleep(50 * time.Millisecond)
	rs = c.snapshot()
	seen := map[string]int{}
	for _, r := range rs {
		if r.evt.Type == core.EventText && r.evt.Content != "" || r.evt.Type == core.EventResult {
			seen[string(r.evt.Type)+":"+r.evt.Content]++
		}
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("%q reached the channels %d times:\n%s", k, n, dumpRouted(rs))
		}
	}
	eng := results(rs, "engine")
	if eng[0].Content != "STARTED" || eng[1].Content != "HANDLED\n\nTHIRD" {
		t.Fatalf("engine results = %q, %q:\n%s", eng[0].Content, eng[1].Content, dumpRouted(rs))
	}
	for _, r := range rs {
		if r.ch == "wakeup" && r.evt.Type == core.EventResult {
			t.Fatalf("the folded-in turn's result went to the wakeup path:\n%s", dumpRouted(rs))
		}
	}
}

// Item 8 (shape C, latent): a mid-turn compaction (a result with Done=false) used to release the turn's channel and
// spend the count, so the rest of the turn went to the wakeup path.
func TestFlosendo1137_CompactionKeepsTurnChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := &claudeSession{events: make(chan core.Event, 16), orphanEvents: make(chan core.Event, 16), ctx: ctx}
	cs.alive.Store(true)
	cs.inTurn.Store(1)
	cs.handleResult(map[string]any{"type": "result", "subtype": "compact", "result": ""})
	cs.handleAssistant(map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "after compaction"}}}})
	cs.handleResult(map[string]any{"type": "result", "subtype": "success", "result": "after compaction",
		"usage": map[string]any{"input_tokens": float64(5), "output_tokens": float64(3)}})
	var got []core.Event
	for e := drainOne(cs.events); e != nil; e = drainOne(cs.events) {
		got = append(got, *e)
	}
	if len(got) != 3 || got[1].Content != "after compaction" || !got[2].Done {
		t.Fatalf("engine channel got %+v, want compaction, text, result", got)
	}
	if e := drainOne(cs.orphanEvents); e != nil {
		t.Fatalf("part of the turn went to the wakeup path after the compaction: %+v", e)
	}
	if n := cs.inTurn.Load(); n != 0 {
		t.Fatalf("count after the turn = %d, want 0", n)
	}
}

// ---- the engine with a real session: items 1, 3, 4, 6 ------------------------------------------------------------

type capPlatform struct {
	mu      sync.Mutex
	posts   []string
	handler core.MessageHandler
	started chan struct{}
}

func newCapPlatform() *capPlatform { return &capPlatform{started: make(chan struct{})} }

func (p *capPlatform) Name() string { return "cap" }
func (p *capPlatform) Start(h core.MessageHandler) error {
	p.mu.Lock()
	p.handler = h
	p.mu.Unlock()
	close(p.started)
	return nil
}
func (p *capPlatform) Reply(_ context.Context, _ any, c string) error { return p.Send(nil, nil, c) }
func (p *capPlatform) Send(_ context.Context, _ any, c string) error {
	p.mu.Lock()
	p.posts = append(p.posts, c)
	p.mu.Unlock()
	return nil
}
func (p *capPlatform) Stop() error { return nil }
func (p *capPlatform) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.posts...)
}
func (p *capPlatform) send(n int, text string) {
	p.mu.Lock()
	h := p.handler
	p.mu.Unlock()
	go h(p, &core.Message{SessionKey: "cap:ch1:u1", Platform: "cap", MessageID: fmt.Sprintf("m%d", n), UserID: "u1",
		UserName: "ivan", Content: text, ReplyCtx: "rc"})
}

func (p *capPlatform) waitPost(t *testing.T, what string, d time.Duration, ok func([]string) bool) []string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s := p.all(); ok(s) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; posts: %q", what, p.all())
	return nil
}

func countWith(posts []string, sub string) int {
	n := 0
	for _, s := range posts {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func postWith(posts []string, sub string) string {
	for _, s := range posts {
		if strings.Contains(s, sub) {
			return s
		}
	}
	return ""
}

// newReplayEngine wires a real Engine, the real claudecode agent and the fake claude.
func newReplayEngine(t *testing.T, f fakeCLI) *capPlatform {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ag, err := New(map[string]any{"work_dir": t.TempDir(), "cmd": f.cmd(), "mode": "bypassPermissions", "env": f.env(),
		"cc_data_dir": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p := newCapPlatform()
	e := core.NewEngine("eden1137", ag, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	<-p.started
	return p
}

const wakeupHeader = "Scheduled wakeup"

// Item 3, folded in (22 Oct's shape, reproduced by D0 s5): a message written while a self-fired turn is at a tool
// call is answered inside that turn. The answer must be the turn's reply (posted once, no wakeup header), and the turn
// must end at that turn's result, not at the idle kill.
func TestFlosendo1137_QuietCLIEndsWaitingTurn_FoldedIn(t *testing.T) {
	sig := t.TempDir()
	p := newReplayEngine(t, fakeCLI{fixture: fixturePath("s5_midturn_self_tool.jsonl"), scale: 0.01, signalDir: sig})
	p.send(1, "start the background job")
	p.waitPost(t, "the first reply", 10*time.Second, func(s []string) bool { return countWith(s, "STARTED") == 1 })
	waitFile(t, filepath.Join(sig, "waiting-2"), 10*time.Second)
	p.send(2, "Reply with the single word THIRD.")
	posts := p.waitPost(t, "the folded-in reply", 8*time.Second, func(s []string) bool { return countWith(s, "THIRD") > 0 })
	time.Sleep(300 * time.Millisecond)
	posts = p.all()
	if n := countWith(posts, "THIRD"); n != 1 {
		t.Fatalf("the folded-in answer was posted %d times: %q", n, posts)
	}
	if r := postWith(posts, "THIRD"); strings.Contains(r, wakeupHeader) {
		t.Fatalf("the folded-in answer was posted as a wakeup, not as the reply: %q", r)
	}
	if n := countWith(posts, wakeupHeader); n != 0 {
		t.Fatalf("a tool-only self-fired part was posted as a wakeup: %q", posts)
	}
	// The turn is over: a third message gets a turn of its own straight away (the fixture has none, so it is the
	// queue's state that is checked: the message reaches the fake's stdin, which means the lock was released).
}

// Item 3, never read: the CLI never takes the message in. After the self-fired result and Q of quiet, the waiting
// turn ends with EDEN-1618's notice (posted once), the self-fired text is a wakeup, and a message queued behind the
// turn is processed.
func TestFlosendo1137_QuietCLIEndsWaitingTurn_NeverRead(t *testing.T) {
	old := notTakenQuiet
	notTakenQuiet = 400 * time.Millisecond
	t.Cleanup(func() { notTakenQuiet = old })
	sig := t.TempDir()
	p := newReplayEngine(t, fakeCLI{fixture: fixturePath("never_read.jsonl"), scale: 0.01, signalDir: sig})
	p.send(1, "start the background job")
	p.waitPost(t, "the first reply", 10*time.Second, func(s []string) bool { return countWith(s, "STARTED") == 1 })
	waitFile(t, filepath.Join(sig, "waiting-2"), 10*time.Second)
	p.send(2, "Reply with the single word THIRD.")
	time.Sleep(50 * time.Millisecond)
	p.send(3, "and this one, queued")
	// The wakeup waits for the session lock (up to 30 s), which the engine holds until the queued turn is done.
	posts := p.waitPost(t, "the queued message's reply and the wakeup", 15*time.Second, func(s []string) bool {
		return countWith(s, "QUEUED-OK") > 0 && countWith(s, "background task") > 0
	})
	time.Sleep(300 * time.Millisecond)
	posts = p.all()
	if n := countWith(posts, "Not processed (the agent did not take it in)"); n != 1 {
		t.Fatalf("the not-processed notice was posted %d times: %q", n, posts)
	}
	if !strings.Contains(postWith(posts, "Not processed"), "Reply with the single word THIRD.") {
		t.Fatalf("the notice does not quote the message: %q", postWith(posts, "Not processed"))
	}
	if w := postWith(posts, "background task"); !strings.Contains(w, wakeupHeader) {
		t.Fatalf("the self-fired text was not posted as a wakeup: %q", posts)
	}
	if n := countWith(posts, "background task"); n != 1 {
		t.Fatalf("the self-fired text was posted %d times: %q", n, posts)
	}
}

// Item 4 (D0 s3): a message written while a self-fired turn has no tool call gets its own turn 5 ms after the
// self-fired result. The self-fired text must post as a wakeup and our answer as the reply, each once; the quiet
// backstop must not fire in that gap.
func TestFlosendo1137_WakeupThenOwnTurnKeepsBoth(t *testing.T) {
	old := notTakenQuiet
	notTakenQuiet = 400 * time.Millisecond
	t.Cleanup(func() { notTakenQuiet = old })
	sig := t.TempDir()
	p := newReplayEngine(t, fakeCLI{fixture: fixturePath("s3_midturn_self.jsonl"), scale: 0.01, signalDir: sig})
	p.send(1, "start the background job")
	p.waitPost(t, "the first reply", 10*time.Second, func(s []string) bool { return countWith(s, "STARTED") == 1 })
	waitFile(t, filepath.Join(sig, "waiting-2"), 10*time.Second)
	p.send(2, "Reply with the single word THIRD.")
	posts := p.waitPost(t, "both posts", 8*time.Second, func(s []string) bool {
		return countWith(s, "THIRD") > 0 && countWith(s, "background task") > 0
	})
	time.Sleep(notTakenQuiet + 300*time.Millisecond)
	posts = p.all()
	if r := postWith(posts, "THIRD"); strings.Contains(r, wakeupHeader) || countWith(posts, "THIRD") != 1 {
		t.Fatalf("our answer was not posted once as the reply: %q", posts)
	}
	if w := postWith(posts, "background task"); !strings.Contains(w, wakeupHeader) || countWith(posts, "background task") != 1 {
		t.Fatalf("the self-fired text was not posted once as a wakeup: %q", posts)
	}
	if n := countWith(posts, "Not processed"); n != 0 {
		t.Fatalf("the quiet backstop fired although the CLI took the message in: %q", posts)
	}
}

// Item 6: NO_REPLY on the folded-in path. The self-fired part before the move says NO_REPLY and so does the folded
// reply: nothing is posted for them, and the turn ends at its result, so a third message is answered at once.
func TestFlosendo1137_TakenReplyHonoursNoReply(t *testing.T) {
	sig := t.TempDir()
	p := newReplayEngine(t, fakeCLI{fixture: fixturePath("s5_no_reply.jsonl"), scale: 0.01, signalDir: sig})
	p.send(1, "start the background job")
	p.waitPost(t, "the first reply", 10*time.Second, func(s []string) bool { return countWith(s, "STARTED") == 1 })
	waitFile(t, filepath.Join(sig, "waiting-2"), 10*time.Second)
	before := len(p.all())
	p.send(2, "Reply with the single word THIRD.")
	waitFile(t, filepath.Join(sig, "waiting-3"), 10*time.Second)
	time.Sleep(100 * time.Millisecond)
	p.send(3, "and then this")
	posts := p.waitPost(t, "the third message's reply (the NO_REPLY turn has ended)", 8*time.Second, func(s []string) bool {
		return countWith(s, "AFTER-OK") > 0
	})
	time.Sleep(300 * time.Millisecond)
	posts = p.all()
	for _, s := range posts[before:] {
		if strings.Contains(s, "NO_REPLY") || strings.Contains(s, wakeupHeader) {
			t.Fatalf("the NO_REPLY turn posted something (only its progress lines may show): %q", posts[before:])
		}
	}
}

// ---- round 2 ------------------------------------------------------------------------------------------------------

// Round 2 (b), the round-1 review's note 2: a turn the CLI started itself can write nothing for longer than the quiet
// window before its first model output (a slow model call; derived from D0 s3). From a turn's system init to its
// result the CLI is busy, not ignoring our message: the backstop must not fire, and our message, taken in right after
// that turn, is answered as the reply, once.
func TestFlosendo1137_QuietCheckPausesWhileATurnRuns(t *testing.T) {
	old := notTakenQuiet
	notTakenQuiet = 400 * time.Millisecond
	t.Cleanup(func() { notTakenQuiet = old })
	sig := t.TempDir()
	p := newReplayEngine(t, fakeCLI{fixture: fixturePath("s3_slow_first_call.jsonl"), scale: 0.01, signalDir: sig})
	p.send(1, "start the background job")
	p.waitPost(t, "the first reply", 10*time.Second, func(s []string) bool { return countWith(s, "STARTED") == 1 })
	waitFile(t, filepath.Join(sig, "waiting-2"), 10*time.Second)
	p.send(2, "Reply with the single word THIRD.")
	p.waitPost(t, "both posts", 10*time.Second, func(s []string) bool {
		return countWith(s, "THIRD") > 0 && countWith(s, "background task") > 0
	})
	time.Sleep(notTakenQuiet + 300*time.Millisecond)
	posts := p.all()
	if n := countWith(posts, "Not processed"); n != 0 {
		t.Fatalf("the quiet backstop fired while the CLI was running a turn: %q", posts)
	}
	if r := postWith(posts, "THIRD"); strings.Contains(r, wakeupHeader) || countWith(posts, "THIRD") != 1 {
		t.Fatalf("our answer was not posted once as the reply: %q", posts)
	}
	if w := postWith(posts, "background task"); !strings.Contains(w, wakeupHeader) || countWith(posts, "background task") != 1 {
		t.Fatalf("the self-fired text was not posted once as a wakeup: %q", posts)
	}
}

// Round 2 (c), the round-1 review's note 1: the CLI ends a message of ours without ever starting it (cancelled,
// discarded, or completed with no started; derived, since D0 saw none of these for ours). It will never answer it, so
// the turn waiting for it ends at once with EDEN-1618's notice, posted once (the quiet window stays at its 30 s here,
// so only "at once" passes), and the message queued behind it is answered.
func TestFlosendo1137_TerminalLifecycleWithoutStartEndsTurnAtOnce(t *testing.T) {
	for _, st := range []string{"cancelled", "discarded", "completed"} {
		t.Run(st, func(t *testing.T) {
			sig := t.TempDir()
			p := newReplayEngine(t, fakeCLI{fixture: fixturePath("not_taken_terminal.jsonl"), scale: 0.01, signalDir: sig, state: st})
			p.send(1, "start the background job")
			p.waitPost(t, "the first reply", 10*time.Second, func(s []string) bool { return countWith(s, "STARTED") == 1 })
			waitFile(t, filepath.Join(sig, "waiting-2"), 10*time.Second)
			p.send(2, "Reply with the single word THIRD.")
			time.Sleep(50 * time.Millisecond)
			p.send(3, "and this one, queued")
			p.waitPost(t, "the queued message's reply and the wakeup", 10*time.Second, func(s []string) bool {
				return countWith(s, "QUEUED-OK") > 0 && countWith(s, "background task") > 0
			})
			time.Sleep(300 * time.Millisecond)
			posts := p.all()
			if n := countWith(posts, "Not processed (the agent did not take it in)"); n != 1 {
				t.Fatalf("the not-processed notice was posted %d times: %q", n, posts)
			}
			if !strings.Contains(postWith(posts, "Not processed"), "Reply with the single word THIRD.") {
				t.Fatalf("the notice does not quote the message: %q", postWith(posts, "Not processed"))
			}
			if n := countWith(posts, "QUEUED-OK"); n != 1 {
				t.Fatalf("the queued message's reply was posted %d times: %q", n, posts)
			}
			if w := postWith(posts, "background task"); !strings.Contains(w, wakeupHeader) || countWith(posts, "background task") != 1 {
				t.Fatalf("the self-fired text was not posted once as a wakeup: %q", posts)
			}
		})
	}
}
