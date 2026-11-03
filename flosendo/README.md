# Flosendo's cc-connect: keeping our own bridge, taking upstream only when it is worth it

Our Slack agents run on this fork of [chenhg5/cc-connect](https://github.com/chenhg5/cc-connect)
(`Flosendo-One/cc-connect`, EDEN-1313). Ivan, 9 Oct 2026: keep our own bridge, and take upstream
changes "only if it makes sense, and added value by scoring and looking at the cons and pros and
consequences". This folder is how.

## Our own changes (keep them through any upstream merge)

`docs/fork-features.md` lists the Unimount-era features (Jim, April to September 2026). Since Flosendo
took the fork over, each with its test:

| Commit | What | Test |
|---|---|---|
| `d288f868` | Telemetry off unless a deployment opts in with its own key (EDEN-1313) | `core/telemetry*_test.go` |
| `27251c9a` | A silent idle turn posts nothing: NO_REPLY honoured on the wakeup path | `TestBuildOrphanRender_HeaderShape` |
| `a054387f` | The same on the engine's unsolicited relay | `TestUnsolicitedOutbound_HonoursNoReply` |
| `6ed03c23` | A nudge that meets a busy session waits (up to 3 h) and runs; "⏰" posts only when it starts | `core/flosendo_scheduled_test.go` |
| `79cfad75` | A nudge cancelled or switched off while it waits does not run | `core/flosendo_scheduled_test.go` |
| EDEN-1618 | A message queued behind a busy session is never lost unseen: an idle kill hands it to a fresh session (and posts to `alert_session_key`); a reset says "Not processed" in its thread, quoting it; a restart saves the queue and says the same after; `deploy.sh queued` reads the drops | `core/flosendo_queue_test.go`, `platform/slack/flosendo_reply_target_test.go`, `flosendo/test_queued.sh` |
| EDEN-1789 | A channel bound to a person's Claude seat: `[[projects.session_env]]` (a session key, its env) adds that env to every agent start of the key, its threads (`key:...`) and its cron runs (`key#cron:...`), never to another key; hot-reloaded by `/config reload`, validated at load (a malformed entry fails the load, so a reload keeps the old rules); one `flosendo: agent start` log line per start names the key, the env names and the seat folder, never another value (the per-session account record EDEN-1782 reads); `/usage` probes the asking channel's seat, not the last start's. Code in `core/flosendo_session_env.go`, `config/flosendo_session_env.go` and `agent/claudecode/flosendo_usage_seat.go`; engine.go has one call at each of the two start sites and one in `/usage`, `claude_usage.go` one | `core/`, `config/`, `cmd/cc-connect/flosendo_session_env_test.go`, `agent/claudecode/flosendo_usage_seat_test.go` |
| EDEN-1759 | One session's effort: `/new effort=<low|medium|high|xhigh|max> [name]` stores the effort on the new session (`flosendo_effort` in the session store, so a resume after a restart keeps it), never in its name; before its first start, an effort below the default (low, medium, high) is asked of the project's `effort_guard` script (JSON on stdin: project, session key, effort, first message; exit 0 allows; 10 s; stderr never read), and any refusal, error, timeout or missing guard starts at the default with a channel reply saying why. The effort reaches the agent in `StartSession`'s context for that one start (`WithSessionEffort`), so the agent's own setting never changes and no other session inherits it; a plain `/new`, threads, relays and a `new_per_run` cron or timer run pass none. A cron or timer run in the default mode (`reuse`) is a turn of the channel's active session, so inside a session started with `effort=high` it runs at high, from the stored decision (QA round 1 finding; the chief of staff kept the behaviour, reversal word "1759 cron at default"). The start line gains `effort=<v or default> effort_from=new\|refused\|none`. `normalizeEffort` keeps `xhigh` (it returned "" before, so a config or preset at xhigh meant no flag); `/reasoning`'s numbered list is unchanged (adding xhigh would renumber max). Code in `core/flosendo_session_effort.go`; engine.go has one call in `/new`, one before the interactive start, the effort context at its two `StartSession` calls and the session at both log calls; session.go the field and its copy in the store snapshot; `claudecode.go` two lines; config `effort_guard` | `core/flosendo_session_effort_test.go`, `agent/claudecode/flosendo_session_effort_test.go`, `config/` |
| EDEN-1137 | A turn's reply reaches the turn that waits for it, routed by the CLI's own command lifecycle (`Send` puts a uuid on each message; the CLI echoes it as `command_uuid`: queued, started, completed). Once echoed, a turn goes to the engine exactly when one of our messages has started, else to the wakeup path; a message taken into a turn the CLI started itself moves the rest of that turn to the engine (the part before posts as a wakeup, text only, at once). A message the CLI never takes in ends its turn after 30 s of quiet with EDEN-1618's "Not processed" notice (the quiet is not counted from a turn's system init to its result: the CLI is busy then, however slow its first model call), and at once when the CLI ends it without starting it (`cancelled`, `discarded`, or `completed` with no `started`); the signal names the messages it gives up on, and the engine acts on it only while the agent says it is current (`NotTakenCurrent`: the latest one, until the engine's turn settles), so one left in the channel by a turn that has ended cannot end the next. Two declared limits (the round 2 review's notes): a not-taken signal that arrives with no turn of ours waiting goes to the reader between turns, which has no case for it and counts it as turn activity (it posts nothing); and a message `started` and then `cancelled` with no result still waits for the idle kill (not seen: the bridge sends no interrupt). With it: upstream 80bdf304's claudecode half (the resume's empty task-notification result dropped), system lines and tool results following the open turn, compaction keeping the turn, only model output disarming the phantom grace, `SettleTurn` at every turn end, and the nil-agentSession guard below. One log line per session names the mode (`flosendo: turn routing by command lifecycle` or `... by counter`); a CLI that never echoes keeps the counter, with a declared limit, also in that log line: there, a message folded into a turn the CLI started itself (22 Oct's shape) still waits for the idle kill. The real CLI's lines (2.1.286, D0) are the fixtures. Code in `agent/claudecode/flosendo_turn_routing.go` and `core/flosendo_turn_routing.go`; session.go has the uuid in `Send`, the `command_lifecycle` case, `emitEvent`'s choice, the two bypasses, the drop and the system init's pause; engine.go the head of `processInteractiveEvents`' loop and two lines in it; sdk_orphan.go the move | `agent/claudecode/flosendo_turn_routing*_test.go` (`testdata/eden1137/`), `core/flosendo_turn_routing_test.go` |

## Upstream defects we know of (watch for an upstream fix)

- `agent/claudecode/claude_usage.go` `runClaudeUsageProbe`: when the probed `claude` exits early, the pty read fails
  with EIO; if the loop takes that from `readDone`, the deferred `<-readDone` then waits for ever, and `/usage` never
  answers (found under EDEN-1789, 2 Nov 2026; confirmed by its review round 2).
- `core/engine.go` `processInteractiveEvents`: it reads `state.agentSession.Events()` with no nil check, and
  `cleanupInteractiveState` (what `/new` runs on the channel's old session) sets `state.agentSession` to nil. A `/new`
  that lands after a turn has started but before it reaches its event loop makes the turn dereference nil; the turn's
  goroutine (`go e.processInteractiveMessageWith`) has no `recover`, so the whole process stops (systemd's
  `Restart=on-failure` brings it back, without the turns that were running). Found under EDEN-1759's QA, 2 Nov 2026,
  in the fork's tests about 1 run in a few hundred (B under `-count=200 -cpu 1,2,8`; A's re-QA, 8 of 11 batches of up
  to 300 runs); the tests now wait for the turn to end before a `/new`. Not seen live. **Patched in our fork by EDEN-1137** (the turn
  takes the session under its lock once; nil ends the turn): `TestFlosendo1137_SessionResetBeforeEventLoopDoesNotPanic`
  lands the `/new` in that window every time. Not reachable through the CLI (the window has no CLI or network call);
  still open upstream.

## Taking an upstream change: score, decide, then build

1. **Report.** `python3 flosendo/upstream_report.py` fetches upstream and lists every commit not yet in
   `upstream-decisions.json`, tagged by what it touches. Tags: `uses` (Slack, Claude Code, core,
   config, cmd), `not-used` (other platforms and agents, macOS launchd) or `neutral` (docs, tests).
   It also gives each commit's size and how many files it shares with our patches (the conflict risk).
2. **Score each `uses` commit** on four questions, in this order:
   - Do we run the code it changes? Check `~/.cc-connect/config.toml`: a fix to a feature we have
     switched off is worth nothing.
   - Have we seen the problem it fixes? Check the logs (`~/.cc-connect/logs`).
   - What does it risk? Size, overlap with our patches, and whether it changes behaviour our agents rely on.
   - What would change for us? Example: a question that waits for Ivan stalls an overnight run.
3. **Record the verdict** in `upstream-decisions.json` with the reason: `adopt`, `watch` (adopt if a
   named symptom appears), `declined`, `feature-off`, `have-equivalent`, `not-used` or `low-value`.
   Anything that changes how the agents behave goes to Ivan first, as a short card.
4. **Adopt** on a branch: `git cherry-pick <sha>`, or merge `upstream/main` when most of it is
   adopted. Then `go vet ./core/ && nice go test -p 1 ./core/`. Our tests above must still pass.
5. **Build and switch on** with `flosendo/deploy.sh`:
   - Before `switch`, every building session gives "restart ok" and has set itself a resume timer.
   - `switch` and `rollback` refuse while a user's message is waiting in a busy session (`deploy.sh queued -` lists them): the bridge keeps that queue in memory, so a restart drops it (EDEN-1137, 9 Oct: Ivan's 12:20 message was lost to the 12:37 restart). Wait for it to be processed; `FORCE=1` only when the queue was emptied another way (a "stop" leaves no log line).
   - `build <version>` builds the version.
   - `switch <version>` points the systemd drop-in at it and restarts the bridge in 90 s, outside the
     service.
   - `verify <version>` checks the service is active, on that binary, with its schedulers started.
   - `rollback <previous>` goes back if verify fails.

**First scoring, 9 Oct 2026:** upstream had 63 commits since our last merge (`b39c11f4`), 59 of them
non-merge, and all 59 have a verdict:
- 0 adopt;
- 1 watch (`781ead2b`, the busy-lock self-heal: scored under EDEN-1137, which found the two freezes were a live turn waiting for a reply routed elsewhere, not a lock held by a dead process, and fixed that with the CLI's command lifecycle; stays watch for a session busy with its agent process gone. The first verdict said we had never seen the symptom, and that was wrong);
- 1 declined (`48f8a399`, which would stall unattended runs on a question);
- 7 feature-off;
- 2 have-equivalent;
- 33 not-used;
- 15 low-value.

(Commit `3dd5139d`'s message says "63" and "19 low-value". These counts are the correct ones.)

The weekly check (a cc-connect cron in #eden-two, Mondays 08:00 UTC) runs `upstream_report.py
--quiet`. It writes to Ivan only when an undecided upstream commit touches what we use.

## Nudges: what makes them work

- A nudge that meets a busy session waits for the session to free, then runs.
- A nudge cancelled or switched off while it waits does not run.
- A nudge that finds nothing to do answers NO_REPLY, and nothing posts.
- `--mute` silences the nudge's own `cc-connect send` too. Mute only nudges that never need to report.
- The scheduler's timeout (default 30 min) only stops waiting for the result: it does not stop the
  agent's work. Overnight builds may set `--timeout-mins 0`.
