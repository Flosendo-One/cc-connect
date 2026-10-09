#!/usr/bin/env bash
# Flosendo: build, switch on, verify and roll back our own cc-connect build (EDEN-1313).
#
#   flosendo/deploy.sh build    <version>   build ~/.local/opt/cc-connect/<version>/cc-connect from HEAD (no_web)
#   flosendo/deploy.sh switch   <version>   point the systemd drop-in at it and restart the bridge in 90 s
#   flosendo/deploy.sh verify   <version>   service active, on that binary, schedulers started, no fatal error
#   flosendo/deploy.sh rollback <version>   the same as switch, to the version named (normally the previous one)
#
# A restart stops every agent turn running under the bridge (gates included). Before `switch`:
# list the live sessions (ListAgents) and get "restart ok" from each building session, each having set
# itself a resume timer a few minutes out. `switch` schedules the restart outside the service
# (systemd-run), so the session running this script can finish its reply first. See flosendo/README.md.
set -euo pipefail
cmd="${1:-}"; ver="${2:-}"
[ -n "$cmd" ] && [ -n "$ver" ] || { sed -n '2,8p' "$0"; exit 2; }
GO="${GO:-$HOME/.local/go1.25.14/go/bin/go}"
OPT="$HOME/.local/opt/cc-connect"
DROPIN="$HOME/.config/systemd/user/cc-connect.service.d/eden-1313-own-build.conf"
BIN="$OPT/$ver/cc-connect"
cd "$(dirname "$0")/.."

case "$cmd" in
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
