#!/usr/bin/env python3
"""gp-g02t RED/GREEN for the box adapter on the gateway's stable conversation_id.

Builds two trees of whatsapp/adapter with `git archive` (never touching the
checkout): the adapter at af7e2d9b (PR 76 before this round) with the round's
test/conversation.test.js and test/fakes.js laid over it, and the adapter at
the head. Runs test/conversation.test.js in each through node's test runner
and writes both transcripts to OUT. The base must fail every case but the
named guards (cases whose outcome the gateway already decides; they must pass
at both), and the head pass every case.

    python3 -I assets/pr-evidence/gp-g02t/make_red.py OUT [HEAD_REV]

Run from the repository root. OUT must not exist yet (nothing is ever deleted).
"""
import pathlib
import re
import subprocess
import sys

BASE = 'af7e2d9b99461481a94f57ab252269a07f364b15'
OVERLAY = ['whatsapp/adapter/test/conversation.test.js', 'whatsapp/adapter/test/fakes.js']
# A PR 73 row (user_id null) is refused by the gateway's own 403 at the base too.
GUARDS = ['an owner PR 73 bound by number alone']


def git(*args, binary=False):
    return subprocess.run(['git', *args], check=True, capture_output=True, text=not binary).stdout


def tree(rev, dest):
    dest.mkdir(parents=True)
    archive = subprocess.run(['git', 'archive', rev, 'whatsapp/adapter'], check=True, capture_output=True).stdout
    subprocess.run(['tar', '-x', '-C', str(dest)], input=archive, check=True)
    return dest / 'whatsapp' / 'adapter'


def run(adapter, out_file):
    proc = subprocess.run(
        ['node', '--test', '--test-reporter=spec', 'test/conversation.test.js'],
        cwd=adapter, capture_output=True, text=True,
    )
    out_file.write_text(proc.stdout + proc.stderr + f'\nexit {proc.returncode}\n')
    text = proc.stdout
    count = lambda key: int((re.search(rf'^ℹ {key} (\d+)', text, re.M) or [0, 0])[1])
    passed = re.findall(r'^✔ (.*?) \(\d', text, re.M)
    failed = re.findall(r'^✖ (.*?) \(\d', text, re.M)
    return count('tests'), count('pass'), count('fail'), passed, sorted(set(failed))


def main():
    out = pathlib.Path(sys.argv[1]).resolve()
    head = git('rev-parse', sys.argv[2] if len(sys.argv) > 2 else 'HEAD').strip()
    out.mkdir(parents=True)
    base_adapter = tree(BASE, out / 'tree-base')
    for path in OVERLAY:
        (out / 'tree-base' / path).write_bytes(git('show', f'{head}:{path}', binary=True))
    head_adapter = tree(head, out / 'tree-head')
    red = run(base_adapter, out / f'red-conversation-at-{BASE[:8]}.txt')
    green = run(head_adapter, out / f'green-conversation-at-{head[:8]}.txt')
    print(f'base {BASE[:8]} with the head {head[:8]} tests: {red[0]} tests, {red[1]} pass, {red[2]} fail')
    print(f'head {head[:8]}: {green[0]} tests, {green[1]} pass, {green[2]} fail')
    guard = lambda name: any(g in name for g in GUARDS)
    red_ok = red[0] > 0 and red[1] == len(GUARDS) and all(guard(n) for n in red[3])
    for name in red[3]:
        print(f'  passes at the base (a guard): {name[:110]}')
    ok = red_ok and green[0] == green[1] and green[0] == red[0]
    print('RED-first holds' if ok else 'RED-first DOES NOT hold')
    sys.exit(0 if ok else 1)


if __name__ == '__main__':
    main()
