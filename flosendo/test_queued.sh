#!/usr/bin/env bash
# EDEN-1618: `deploy.sh queued` counts a session's waiting messages from the bridge's log. A queue the bridge took away
# (a reset's "Not processed" notices, an idle kill's replay, a stop's save) must stop counting, or `switch` refuses for
# ever; a drain that leaves messages must keep counting. Run: bash flosendo/test_queued.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/.cc-connect/logs"
now() { date -u -d "$1" +%Y-%m-%dT%H:%M:%S.000Z; }
fail=0
check() { # name, expected output, log lines...
  local name="$1" want="$2"; shift 2
  printf '%s\n' "$@" > "$tmp/.cc-connect/logs/cc-connect.log"
  local got; got="$(HOME="$tmp" bash "$here/deploy.sh" queued - 2>&1 | sed 's/ since .*//')"
  if [ "$got" = "$want" ]; then echo "ok   $name"; else echo "FAIL $name: got [$got], want [$want]"; fail=1; fi
}
q() { echo "time=$(now "$1") level=INFO msg=\"message queued for busy session\" session=slack:C1 user=Ivan queue_depth=$2"; }
check "a queued message waits" "slack:C1 1" "$(q '10 min ago' 1)"
check "a reset's notices end the wait" "no message waiting" "$(q '10 min ago' 1)" \
  "time=$(now '9 min ago') level=WARN msg=\"queued messages dropped\" session=slack:C1 reason=\"session reset\" dropped=1 remaining_queue=0"
check "an idle kill's replay ends the wait" "no message waiting" "$(q '10 min ago' 2)" \
  "time=$(now '9 min ago') level=WARN msg=\"queued messages replayed after idle kill\" session=slack:C1 replayed=2 remaining_queue=0"
check "a stop's save ends the wait" "no message waiting" "$(q '10 min ago' 1)" \
  "time=$(now '9 min ago') level=WARN msg=\"queued messages saved at stop\" session=slack:C1 saved=1 remaining_queue=0"
check "a drain that leaves one keeps it waiting" "slack:C1 1" "$(q '10 min ago' 2)" \
  "time=$(now '9 min ago') level=INFO msg=\"processing queued message\" session=slack:C1 remaining_queue=1"
exit $fail
