"""Synthetic-only RED, parity and diversity probes; no Slack access."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile

EVIDENCE = Path(__file__).resolve().parent
ROOT = EVIDENCE.parents[3]
BASE = '8e6d75e51b5d9749e5a14d558bc19a466a289599'
ROUND1 = '6d977249977ddbdf4be5ec9c4c4631d13183552c'
CASE_PATH = 'slack-full/tests/fixtures/redaction/cases.json'
PATHS = ['scripts/slack_redact.py', 'adapter/redact/redact.go',
         'adapter/redact/patterns.json', 'adapter/go.mod']


def at(revision, path):
    return subprocess.check_output(['git', 'show', revision + ':' + path], cwd=ROOT)


def probe(revision, cases, floor=None):
    with tempfile.TemporaryDirectory(prefix='gp-zbq3-probe-') as directory:
        pack = Path(directory) / 'slack-full'
        for path in PATHS:
            target = pack / path
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(at(revision, 'slack-full/' + path) if revision else
                               (ROOT / 'slack-full' / path).read_bytes())
        if floor is not None:
            path = pack / 'adapter/redact/patterns.json'
            rules = json.loads(path.read_text())
            next(r for r in rules if r['kind'] == 'base64')['min_distinct'] = floor
            path.write_text(json.dumps(rules))
        (pack / 'adapter/probe.go').write_bytes((EVIDENCE / 'probe.go').read_bytes())
        spec = importlib.util.spec_from_file_location('scratch_redact', pack / 'scripts/slack_redact.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        texts = [c.get('text', ''.join(c.get('text_parts', []))) for c in cases]
        # Include the expected output in each engine's input to prove idempotence.
        inputs = texts + [c['want'] for c in cases]
        python = [module.redact_text(text) for text in inputs]
        result = subprocess.run(['go', 'run', '.'], cwd=pack / 'adapter',
                                env={**os.environ, 'GOPROXY': 'off', 'GOFLAGS': '-mod=readonly'},
                                input=json.dumps(inputs), capture_output=True, text=True, check=True)
        go = json.loads(result.stdout)
        count = len(cases)
        failures = {engine: [case['name'] for case, output in zip(cases, outputs)
                             if case['want'] != output]
                    for engine, outputs in [('python', python), ('go', go)]}
        rows = [{'name': case['name'], 'python': py, 'go': g,
                 'byte_identical': py.encode('utf-8') == g.encode('utf-8')}
                for case, py, g in zip(cases, python, go)]
        return {'failures': failures, 'rows': rows,
                'idempotent': {'python': python[count:] == [c['want'] for c in cases],
                               'go': go[count:] == [c['want'] for c in cases]}}


def main():
    cases = json.loads((ROOT / CASE_PATH).read_text())
    old = json.loads(at(ROUND1, CASE_PATH))
    original = json.loads(at(BASE, CASE_PATH))
    assert cases[:len(original)] == original
    current = probe(None, cases)
    assert not any(current['failures'].values()), current['failures']
    assert all(row['byte_identical'] for row in current['rows'])
    assert all(current['idempotent'].values())
    previous = probe(ROUND1, cases)
    assert all(previous['failures'].values()), 'new regressions must fail at round 1'
    first_red = probe(BASE, old)
    assert all(len(failures) == 9 for failures in first_red['failures'].values())
    floor6 = probe(None, original, floor=6)
    expected_floor = {'hex-prefix-base64', 'slack-permalink-prepended-base64'}
    assert all(set(failures) == expected_floor for failures in floor6['failures'].values())
    changed = {c['name'] for c in cases if c not in old}
    table = [row for row in current['rows'] if row['name'] in changed]
    (EVIDENCE / 'parity.json').write_text(json.dumps(table, indent=2, ensure_ascii=True) + '\n')
    summary = {'round1_head': ROUND1, 'round2_fixture_count': len(cases),
               'new_fixtures': len(cases) - len(old), 'revised_fixtures': len(changed) - (len(cases) - len(old)),
               'original_fixtures_unchanged': len(original),
               'round1_red': previous['failures'], 'base_round1_red': first_red['failures'],
               'floor6_red': floor6['failures'], 'current_failures': current['failures'],
               'current_parity_count': len(current['rows']), 'current_idempotent': current['idempotent']}
    (EVIDENCE / 'regression-summary.json').write_text(json.dumps(summary, indent=2) + '\n')
    print(json.dumps(summary, indent=2))


if __name__ == '__main__':
    main()
