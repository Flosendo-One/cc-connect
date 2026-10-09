#!/usr/bin/env python3
"""Flosendo: what upstream cc-connect has that we have not decided on, scored so a person can decide.

  python3 flosendo/upstream_report.py            # fetch upstream, list undecided commits, scored
  python3 flosendo/upstream_report.py --quiet    # print nothing (exit 0) when no undecided commit touches what we use

Each upstream commit not in flosendo/upstream-decisions.json is scored on what it touches:
  uses      Slack, Claude Code, the engine/session/scheduler core, config, cmd: the parts we run
  not-used  another chat platform or agent (Feishu, DingTalk, Codex, Kimi...), or macOS launchd
  neutral   docs, tests, build files
plus its size and how many files it shares with our own patches (conflict risk). The verdict
(adopt | watch | declined | feature-off | have-equivalent | not-used | low-value) is a person's,
written back to the decisions file with a reason; adopted commits are cherry-picked on a branch,
tested, built and deployed with flosendo/deploy.sh. See flosendo/README.md.
"""
import json, re, subprocess, sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DECISIONS = ROOT / 'flosendo' / 'upstream-decisions.json'
USES = ('platform/slack/', 'agent/claudecode/', 'core/', 'cmd/', 'config/')
NOT_USED = ('platform/', 'agent/', 'daemon/launchd')


def sh(*a):
    return subprocess.run(a, capture_output=True, text=True, cwd=ROOT, check=True).stdout


def main():
    quiet = '--quiet' in sys.argv
    sh('git', 'fetch', '-q', 'upstream', '--tags')
    decided = json.loads(DECISIONS.read_text())['commits']
    base = sh('git', 'merge-base', 'HEAD', 'upstream/main').strip()
    ours = set(sh('git', 'diff', '--name-only', base, 'HEAD').split())
    rows = []
    for line in sh('git', 'log', '--no-merges', '--format=%h\t%ad\t%s', '--date=short', 'HEAD..upstream/main').splitlines():
        h, date, subject = line.split('\t', 2)
        if h in decided:
            continue
        files = [f for f in sh('git', 'show', '--name-only', '--format=', h).splitlines() if f]
        stat = sh('git', 'show', '--shortstat', '--format=', h)
        size = sum(int(x) for x in re.findall(r'(\d+) (?:insertion|deletion)', stat))
        if any(f.startswith(USES) for f in files):
            area = 'uses'
        elif any(f.startswith(NOT_USED) for f in files):
            area = 'not-used'
        else:
            area = 'neutral'
        overlap = sum(1 for f in files if f in ours)
        rows.append((area, h, date, size, overlap, subject))
    using = [r for r in rows if r[0] == 'uses']
    if quiet and not using:
        return 0
    print(f'{len(rows)} upstream commit(s) not yet decided; {len(using)} touch what we use.')
    for area, h, date, size, overlap, subject in sorted(rows, key=lambda r: (r[0] != 'uses', -r[3])):
        print(f'  [{area:8}] {h} {date} size {size:5} shares {overlap} file(s) with our patches  {subject[:90]}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
