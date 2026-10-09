#!/usr/bin/env bash
# Flosendo: build, switch on, verify and roll back our own cc-connect build (EDEN-1313).
#
#   flosendo/deploy.sh build    <version>   build ~/.local/opt/cc-connect/<version>/cc-connect from HEAD (no_web)
#   flosendo/deploy.sh switch   <version>   point the systemd drop-in at it and restart the bridge in 90 s
#   flosendo/deploy.sh verify   <version>   service active, on that binary, schedulers started, no fatal error
#   flosendo/deploy.sh rollback <version>   the same as switch, to the version named (normally the previous one)
#   flosendo/deploy.sh queued   -           messages waiting in a busy session (switch and rollback refuse while any)
#
# A restart stops every agent turn running under the bridge (gates included). Before `switch`:
# list the live sessions (ListAgents) and get "restart ok" from each building session, each having set
# itself a resume timer a few minutes out. `switch` schedules the restart outside the service
# (systemd-run), so the session running this script can finish its reply first. See flosendo/README.md.
#
# EDEN-1137: the bridge keeps waiting messages in memory, so a restart drops them (9 Oct 12:37: Ivan's
# 12:20 message was taken off the queue 11 ms after shutdown began). `switch` and `rollback` refuse while
# the log shows a message still waiting; FORCE=1 overrides (e.g. after a "stop", which empties a queue
# without a log line).
set -euo pipefail
cmd="${1:-}"; ver="${2:-}"
[ -n "$cmd" ] && [ -n "$ver" ] || { sed -n '2,8p' "$0"; exit 2; }
GO="${GO:-$HOME/.local/go1.25.14/go/bin/go}"
OPT="$HOME/.local/opt/cc-connect"
DROPIN="$HOME/.config/systemd/user/cc-connect.service.d/eden-1313-own-build.conf"
BIN="$OPT/$ver/cc-connect"
cd "$(dirname "$0")/.."

# Sessions with a message still waiting, read from the bridge log since its last start (24 h at most):
# depth from "message queued for busy session" (queue_depth=N) and "processing queued message"
# (remaining_queue=N). Prints "<session> <waiting> since <time>" per session; prints nothing when none.
queued_messages() {
  local logs="$HOME/.cc-connect/logs" cutoff
  cutoff="$(date -u -d '24 hours ago' +%Y-%m-%dT%H:%M:%S)"
  # Each file only if present: under pipefail a missing cc-connect.log.1 (no rotation yet) would end the script silently.
  { for f in "$logs/cc-connect.log.1" "$logs/cc-connect.log"; do [ -f "$f" ] && cat "$f"; done; true; } | awk -v cutoff="$cutoff" '
    /msg="cc-connect is running"/ { delete depth; delete since; next }
    /msg="message queued for busy session"/ || /msg="processing queued message"/ {
      t = $1; sub(/^time=/, "", t); if (substr(t, 1, 19) < cutoff) next
      match($0, /session=[^ ]+/); k = substr($0, RSTART + 8, RLENGTH - 8)
      if ($0 ~ /queue_depth=/) { match($0, /queue_depth=[0-9]+/); depth[k] = substr($0, RSTART + 12, RLENGTH - 12) + 0; if (!(k in since)) since[k] = t }
      else { match($0, /remaining_queue=[0-9]+/); depth[k] = substr($0, RSTART + 16, RLENGTH - 16) + 0; if (depth[k] == 0) delete since[k] }
    }
    END { for (k in depth) if (depth[k] > 0) print k, depth[k], "since", since[k] }'
}

case "$cmd" in
  queued)
    q="$(queued_messages)"; [ -n "$q" ] && echo "$q" || echo "no message waiting"
    ;;
  build)
    [ -z "$(git status --porcelain --untracked-files=no)" ] || { echo "refusing: uncommitted changes (the binary must match a commit)"; exit 1; }
    mkdir -p "$OPT/$ver"
    nice "$GO" build -tags no_web \
      -ldflags "-s -w -X main.version=$ver -X main.commit=$(git rev-parse --short HEAD) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o "$BIN" ./cmd/cc-connect
    "$BIN" --version 2>&1 | grep -v '^log:'
    ;;
  switch|rollback)
    [ -x "$BIN" ] || { echo "no binary at $BIN (build it first)"; exit 1; }
    q="$(queued_messages)"
    if [ -n "$q" ] && [ "${FORCE:-0}" != 1 ]; then
      echo "refusing: a restart would drop these waiting messages (EDEN-1137):"; echo "$q"
      echo "wait for them to be processed, or FORCE=1 if the queue was emptied another way (a \"stop\")"; exit 1
    fi
    cp "$DROPIN" "/tmp/eden-1313-own-build.conf.bak-$(date -u +%Y%m%dT%H%M%SZ)"
    cat > "$DROPIN" <<CONF
# EDEN-1313: run our own build (Flosendo-One/cc-connect). Delete this file + daemon-reload + restart to go back to upstream.
# $ver ($(cd "$(dirname "$BIN")" && "$BIN" --version 2>&1 | awk '/commit:/{print $2}')), switched on $(date -u +%Y-%m-%dT%H:%MZ) by flosendo/deploy.sh $cmd.
# Back to an earlier build: flosendo/deploy.sh rollback <version> (versions in $OPT).
[Service]
ExecStart=
ExecStart=$BIN
CONF
    systemctl --user daemon-reload
    systemd-run --user --on-active=90 --unit="cc-connect-restart-$(date -u +%H%M%S)" systemctl --user restart cc-connect >/dev/null
    echo "drop-in -> $BIN; restart scheduled in 90 s. Then: flosendo/deploy.sh verify $ver"
    ;;
  verify)
    ok=1
    [ "$(systemctl --user is-active cc-connect)" = active ] || { echo "FAIL service not active"; ok=0; }
    systemctl --user show cc-connect -p ExecStart | grep -q "$BIN" || { echo "FAIL ExecStart is not $BIN"; ok=0; }
    pgrep -f "^$BIN" >/dev/null || { echo "FAIL no running process from $BIN"; ok=0; }
    since="$(systemctl --user show cc-connect -p ActiveEnterTimestamp --value)"
    logs="$HOME/.cc-connect/logs"
    start_iso="$(date -u -d "$since" +%Y-%m-%dT%H:%M:%S)"
    # the bridge rotates its log at 10 MB, so the restart's lines may already be in cc-connect.log.1
    cat "$logs/cc-connect.log.1" "$logs/cc-connect.log" 2>/dev/null \
      | awk -v s="$start_iso" -F'time=' '{ if (substr($2,1,19) >= s) print }' > /tmp/cc-connect-since-restart.log
    grep -q 'timer: scheduler started' /tmp/cc-connect-since-restart.log || { echo "FAIL timer scheduler did not start"; ok=0; }
    if grep -qE 'level=ERROR.*(panic|fatal)' /tmp/cc-connect-since-restart.log; then echo "FAIL fatal error since restart"; ok=0; fi
    [ $ok = 1 ] && echo "OK $ver running since $since" || { echo "verify FAILED: roll back with flosendo/deploy.sh rollback <previous>"; exit 1; }
    ;;
  *) sed -n '2,8p' "$0"; exit 2 ;;
esac
