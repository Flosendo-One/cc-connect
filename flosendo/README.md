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
   - `build <version>` builds the version.
   - `switch <version>` points the systemd drop-in at it and restarts the bridge in 90 s, outside the
     service.
   - `verify <version>` checks the service is active, on that binary, with its schedulers started.
   - `rollback <previous>` goes back if verify fails.

The weekly check (a cc-connect cron in #eden-two, Mondays 08:00 UTC) runs `upstream_report.py
--quiet`. It writes to Ivan only when an undecided upstream commit touches what we use.

## Nudges: what makes them work

- A nudge that meets a busy session waits for the session to free, then runs.
- A nudge cancelled or switched off while it waits does not run.
- A nudge that finds nothing to do answers NO_REPLY, and nothing posts.
- `--mute` silences the nudge's own `cc-connect send` too. Mute only nudges that never need to report.
- The scheduler's timeout (default 30 min) only stops waiting for the result: it does not stop the
  agent's work. Overnight builds may set `--timeout-mins 0`.
