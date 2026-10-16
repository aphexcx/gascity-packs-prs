from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

import pytest

ROOT = Path(__file__).resolve().parents[2]
PACK = ROOT / "gascity"
SCRIPT = PACK / "assets/scripts/worker_provenance.py"
RUN = PACK / "commands/provenance/run.sh"
SPEC = importlib.util.spec_from_file_location("worker_provenance", SCRIPT)
module = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(module)
SHA = "a" * 40
OLD_SHA = "b" * 40
PR = "https://github.com/owner/repo/pull/123"


@pytest.fixture
def fixture(tmp_path):
    """Exercise argv and durable append/dedup against a fake bd; no live store."""
    binary = tmp_path / "bin"
    binary.mkdir()
    state = tmp_path / "state.json"
    state.write_text(json.dumps(dict(events=[], metadata={}, calls=[], closed=False)))
    gc = binary / "gc"
    gc.write_text(f"#!{sys.executable}\n" + r'''
import hashlib, json, os, subprocess, sys, uuid
from pathlib import Path
args = sys.argv[1:]
if args == ['import', 'list']:
    print(os.environ.get('IMPORT_LIST', 'custom\t/packs/gascity\t\tlocal'))
    sys.exit(0)
bindings = os.environ.get('HELP_BINDINGS', 'custom').split(',')
if args[0] in bindings and args[1:2] == ['provenance']:
    sys.exit(subprocess.call([os.environ['PROVENANCE_RUN'], *args[2:]]))
if args[1:] == ['provenance', '--help']:
    print('Gas City CLI - generic root help')
    sys.exit(int(os.environ.get('HELP_EXIT', '0')))
p = Path(os.environ['FIXTURE_STATE'])
s = json.loads(p.read_text())
s['calls'].append(args)
p.write_text(json.dumps(s))
read_command = 'show' if args[:2] == ['b' + 'd', 'show'] else (
    'log' if args[:3] == ['b' + 'd', 'provenance', 'log'] else '')
if read_command and os.environ.get('FAIL_READ') == read_command:
    stream = sys.stdout if os.environ.get('DIAGNOSTIC_STREAM') == 'stdout' else sys.stderr
    print('fixture storage unavailable', file=stream)
    sys.exit(6)
if args[:2] == ['b' + 'd', 'show']:
    assert args[3:] == ['--json']
    print(os.environ.get('RESOLVED_JSON', '[{"id":"fixture-1"}]'))
elif args[:3] == ['b' + 'd', 'provenance', 'record']:
    options = dict(zip(args[3::2], args[4::2]))
    if os.environ.get('FAIL_FACT') == json.loads(options['--payload'])['fact']:
        sys.exit(7)
    assert options['--issue'] == 'fixture-1'
    event = {k: options['--' + k.replace('_', '-')] for k in ['kind','ref','ref_kind','source','payload']}
    assert event['kind'] in ['cut','claim','suspend','resume','handoff','commit','land','used']
    assert event['ref_kind'] in ['git-sha','pr','work-id','transcript','branch']
    event['issue_id'] = options['--issue']
    event['occurred_at'] = os.environ.get('FIXTURE_AT', options['--at'].split('.')[0] + 'Z')
    event['created_at'] = event['occurred_at']
    assert len(event['source']) <= 64
    assert len(event['ref']) <= 255
    event['id'] = str(uuid.UUID(bytes=hashlib.sha256(':'.join([event['source'],event['issue_id'],event['kind'],event['ref']]).encode()).digest()[:16]))
    if not any(x['id'] == event['id'] for x in s['events']):
        s['events'].append(event)
elif args[:3] == ['b' + 'd', 'provenance', 'log']:
    assert args[3:] == ['fixture-1', '--json']
    print(json.dumps(s['events']))
elif args[:2] == ['b' + 'd', 'update']:
    if os.environ.get('FAIL_FACT') == 'metadata':
        sys.exit(9)
    assert args[2] == 'fixture-1'
    for i in range(3, len(args), 2):
        if args[i] == '--unset-metadata':
            s['metadata'].pop(args[i+1], None)
        else:
            assert args[i] == '--set-metadata'
            key, value = args[i+1].split('=', 1)
            s['metadata'][key] = value
elif args[:2] == ['b' + 'd', 'close']:
    assert args[2] == 'fixture-1'
    assert args[3] == '--reason'
    s['closed'] = True
else:
    sys.exit('unexpected gc argv: ' + repr(args))
p.write_text(json.dumps(s))
''')
    gc.chmod(0o755)
    git = binary / "git"
    git.write_text(f"#!{sys.executable}\n" + r'''
import os, sys
from pathlib import Path
state = Path(os.environ['FIXTURE_STATE'] + '.detached')
args = sys.argv[1:]
if 'rev-parse' in args:
    print('a' * 40)
elif 'branch' in args:
    print('' if state.exists() else 'fixture-1')
elif 'switch' in args:
    state.touch()
else:
    sys.exit(8)
''')
    git.chmod(0o755)
    reviews = tmp_path / "reviews.json"
    reviews.write_text(json.dumps([
        dict(stage="codex", round=1, verdict="BLOCK", sha=OLD_SHA, ref="/evidence/codex-1"),
        dict(stage="codex", round=2, verdict="CLEAN", sha=SHA, ref="/evidence/codex-2"),
        dict(stage="gate", verdict="CLEAN", sha=SHA, ref="/evidence/gate-1"),
        dict(stage="read", verdict="CLEAN", sha=SHA, ref="/evidence/read-1"),
    ]))
    env = {**os.environ, "PATH": str(binary) + os.pathsep + os.environ['PATH'], "GC_BIN": str(gc),
           "FIXTURE_STATE": str(state), "FIXTURE_AT": "2026-10-16T08:00:00Z", "PROVENANCE_RUN": str(RUN), "GC_PACK_DIR": str(PACK),
           "CLAIMED_BEAD_ID": "fixture-1",
           "GC_DIR": str(tmp_path), "PR_URL": PR, "EVIDENCE_DIR": "/evidence",
           "REVIEW_RECORDS": str(reviews)}
    return state, reviews, env


def run(fixture, *args):
    return subprocess.run([str(RUN), *args], env=fixture[2], text=True, capture_output=True)


def record(fixture, *extra):
    return run(fixture, "record", "fixture-1", "--outcome", "shipped", "--commit", SHA,
               "--pr", PR, "--reviews", str(fixture[1]), "--evidence-dir", "/evidence", *extra)


def data(fixture):
    return json.loads(fixture[0].read_text())


def close_script(*, include_discovery=True):
    text = (PACK / "template-fragments/gc-role-worker.template.md").read_text()
    discovery = 'PROVENANCE_PACK_ALIAS=' + text.split('```bash\nPROVENANCE_PACK_ALIAS=', 1)[1].split('```', 1)[0]
    guard = '[ -n "${PROVENANCE_PACK_ALIAS:-}" ]'
    close = guard + text.split('```bash\n' + guard, 1)[1].split('```', 1)[0]
    return discovery + '\n' + close if include_discovery else close


def test_fixture_close_records_facts_keys_then_releases_and_closes(fixture):
    result = subprocess.run(['sh', '-c', close_script()], env=fixture[2], text=True, capture_output=True)
    assert result.returncode == 0, result.stderr
    assert 'Provenance binding: custom\n' in result.stdout
    s = data(fixture)
    assert s['closed']
    assert len(s['events']) == 8
    assert s['metadata'] == {'gc.outcome': 'pass', 'gc.work_outcome': 'shipped',
                             'gc.work_commit': SHA, 'gc.work_branch': 'fixture-1', 'gc.pr': PR}
    assert run(fixture, 'summary', 'fixture-1').stdout.strip() == f'PR 123 at {SHA}, gate CLEAN, read CLEAN'
    assert all(c[3:5] == ['--issue', 'fixture-1'] for c in s['calls']
               if c[:3] == ['b' + 'd', 'provenance', 'record'])


@pytest.mark.parametrize('binding', [None, ''])
@pytest.mark.parametrize('shell_flags', ['-c', '-euc'])
def test_close_without_discovery_names_missing_binding_before_writes(fixture, binding, shell_flags):
    env = {**fixture[2]}
    env.pop('PROVENANCE_PACK_ALIAS', None)
    if binding is not None:
        env['PROVENANCE_PACK_ALIAS'] = binding
    result = subprocess.run(['sh', shell_flags, close_script(include_discovery=False)],
                            env=env, text=True, capture_output=True)
    assert result.returncode != 0
    assert result.stderr.strip() == 'Run the provenance discovery first (or substitute its printed binding).'
    assert data(fixture) == dict(events=[], metadata={}, calls=[], closed=False)
    assert not Path(fixture[2]['FIXTURE_STATE'] + '.detached').exists()


def test_close_in_new_shell_uses_printed_discovery_binding(fixture):
    discovery = close_script().split('[ -n "${PROVENANCE_PACK_ALIAS:-}" ]', 1)[0]
    result = subprocess.run(['sh', '-c', discovery], env=fixture[2], text=True, capture_output=True)
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == 'Provenance binding: custom'
    env = {**fixture[2], 'PROVENANCE_PACK_ALIAS': result.stdout.strip().split(': ', 1)[1]}
    result = subprocess.run(['sh', '-euc', close_script(include_discovery=False)],
                            env=env, text=True, capture_output=True)
    assert result.returncode == 0, result.stderr
    assert data(fixture)['closed']
    assert len(data(fixture)['events']) == 8


@pytest.mark.parametrize('source', [
    'https://github.com/owner/packs//gascity#main',
    'https://github.com/owner/packs//gascity/#sha:abc',
])
def test_direct_binding_with_source_ref_closes(fixture, source):
    env = {**fixture[2], 'IMPORT_LIST': f'custom\t{source}\t\tsha:abc'}
    result = subprocess.run(['sh', '-c', close_script()], env=env, text=True, capture_output=True)
    assert result.returncode == 0, result.stderr
    assert 'Provenance binding: custom\n' in result.stdout
    assert data(fixture)['closed']
    assert len(data(fixture)['events']) == 8


@pytest.mark.parametrize('help_exit', ['0', '3'])
def test_no_provenance_binding_stops_close_before_writes(fixture, help_exit):
    env = {**fixture[2], 'HELP_BINDINGS': '', 'HELP_EXIT': help_exit}
    result = subprocess.run(['sh', '-c', close_script()], env=env, text=True, capture_output=True)
    assert result.returncode != 0
    assert 'No gc binding serves provenance: check gc import list and the Gas City pack pin.' in result.stderr
    assert data(fixture) == dict(events=[], metadata={}, calls=[], closed=False)
    assert not Path(fixture[2]['FIXTURE_STATE'] + '.detached').exists()


@pytest.mark.parametrize('imports', [
    'stale\t/packs/gascity\t\told',
    '(transitive)\thttps://github.com/owner/packs//gascity\t\tsha:abc',
    'roles\t/packs/gascity/roles\t\tlocal',
    '',
])
def test_nested_gc_binding_closes_when_direct_binding_is_unavailable(fixture, imports):
    env = {**fixture[2], 'IMPORT_LIST': imports, 'HELP_BINDINGS': 'gc'}
    result = subprocess.run(['sh', '-c', close_script()], env=env, text=True, capture_output=True)
    assert result.returncode == 0, result.stderr
    assert data(fixture)['closed']
    assert len(data(fixture)['events']) == 8


@pytest.mark.parametrize('command', ['record', 'summary'])
def test_abbreviated_id_is_rejected_before_trail_access_or_mutation(fixture, command):
    args = ['--outcome', 'no-op'] if command == 'record' else []
    result = run(fixture, command, 'fixture', *args)
    assert result.returncode != 0
    assert 'expected exact bead id' in result.stderr
    assert data(fixture) == dict(events=[], metadata={}, closed=False,
                                calls=[['b' + 'd', 'show', 'fixture', '--json']])


@pytest.mark.parametrize('resolved', ['null', '{}', '[]', '[null]',
                                     '[{"id":"fixture-1"},{"id":"fixture-2"}]'])
def test_invalid_id_resolution_is_rejected_before_writes(fixture, resolved):
    fixture[2]['RESOLVED_JSON'] = resolved
    result = record(fixture)
    assert result.returncode != 0
    assert 'expected exact bead id' in result.stderr
    assert data(fixture) == dict(events=[], metadata={}, closed=False,
                                calls=[['b' + 'd', 'show', 'fixture-1', '--json']])


@pytest.mark.parametrize('command', ['show', 'log'])
@pytest.mark.parametrize('stream', ['stdout', 'stderr'])
def test_read_failure_preserves_diagnostic_without_writes(fixture, command, stream):
    fixture[2].update(FAIL_READ=command, DIAGNOSTIC_STREAM=stream)
    result = record(fixture)
    assert result.returncode != 0
    assert 'fixture storage unavailable' in result.stderr
    assert not data(fixture)['events']
    assert not data(fixture)['metadata']
    assert not data(fixture)['closed']


def test_explicit_gc_binary_handles_every_subcommand(fixture):
    default = Path(fixture[2]['GC_BIN'])
    selected = default.with_name('selected-gc')
    default.rename(selected)
    default.write_text('#!/bin/sh\nexit 99\n')
    default.chmod(0o755)
    fixture[2]['GC_BIN'] = str(selected)
    assert record(fixture).returncode == 0
    assert run(fixture, 'summary', 'fixture-1').stdout.strip() == f'PR 123 at {SHA}, gate CLEAN, read CLEAN'
    assert {c[1] for c in data(fixture)['calls']} == {'show', 'provenance', 'update'}


def test_gc_binary_defaults_to_path(fixture):
    fixture[2].pop('GC_BIN')
    assert record(fixture).returncode == 0
    assert run(fixture, 'summary', 'fixture-1').stdout.strip() == f'PR 123 at {SHA}, gate CLEAN, read CLEAN'


def test_empty_review_array_file_records_no_review_facts(fixture):
    fixture[1].write_text('[]')
    assert record(fixture).returncode == 0
    assert len(data(fixture)['events']) == 4
    assert run(fixture, 'summary', 'fixture-1').stdout.strip() == f'PR 123 at {SHA}, gate unknown, read unknown'


def test_literal_review_array_is_not_a_file_and_cannot_write(fixture):
    assert record(fixture, '--reviews', '[]').returncode != 0
    assert data(fixture)['calls'] == []


def test_new_commit_without_pr_clears_old_compatibility_pr(fixture):
    assert record(fixture).returncode == 0
    assert run(fixture, 'record', 'fixture-1', '--outcome', 'shipped', '--commit', OLD_SHA).returncode == 0
    assert data(fixture)['metadata'] == {'gc.work_outcome': 'shipped', 'gc.work_commit': OLD_SHA}
    assert run(fixture, 'summary', 'fixture-1').stdout.strip() == f'PR unknown at {OLD_SHA}, gate unknown, read unknown'


def test_blocked_without_commit_clears_old_compatibility_facts(fixture):
    assert record(fixture).returncode == 0
    assert run(fixture, 'record', 'fixture-1', '--outcome', 'blocked').returncode == 0
    assert data(fixture)['metadata'] == {'gc.work_outcome': 'blocked'}
    assert run(fixture, 'summary', 'fixture-1').stdout.strip() == 'blocked; PR unknown at unknown, gate unknown, read unknown'
    # Unsetting already absent keys must also succeed on an immediate retry.
    assert run(fixture, 'record', 'fixture-1', '--outcome', 'blocked').returncode == 0


def test_retry_is_idempotent_and_changed_verdict_is_preserved(fixture):
    assert record(fixture).returncode == 0
    assert record(fixture).returncode == 0
    assert len(data(fixture)['events']) == 8
    reviews = json.loads(fixture[1].read_text())
    reviews[-1].update(verdict='CHANGES_REQUIRED', round=2, ref='/evidence/read-2')
    fixture[1].write_text(json.dumps(reviews))
    assert record(fixture).returncode == 0
    assert len(data(fixture)['events']) == 9
    assert run(fixture, 'summary', 'fixture-1').stdout.strip().endswith('read CHANGES_REQUIRED')


def test_previous_head_reviews_are_not_clean_for_new_head(fixture):
    assert record(fixture).returncode == 0
    assert run(fixture, 'record', 'fixture-1', '--outcome', 'shipped', '--commit', OLD_SHA, '--pr', PR).returncode == 0
    assert run(fixture, 'summary', 'fixture-1').stdout.strip() == f'PR 123 at {OLD_SHA}, gate unknown, read unknown'


@pytest.mark.parametrize('fail', ['review', 'outcome'])
def test_record_failure_prevents_close_and_can_be_retried(fixture, fail):
    env = {**fixture[2], 'FAIL_FACT': fail}
    result = subprocess.run(['sh', '-c', close_script()], env=env, text=True, capture_output=True)
    assert result.returncode != 0
    assert not data(fixture)['closed']
    assert not data(fixture)['metadata']
    assert not Path(fixture[2]['FIXTURE_STATE'] + '.detached').exists()
    assert record(fixture).returncode == 0
    assert len(data(fixture)['events']) == 8


@pytest.mark.parametrize('extra', [('--commit', 'abc'), ('--pr', '123'), ('--evidence-dir', 'relative')])
def test_invalid_input_causes_no_partial_writes(fixture, extra):
    assert record(fixture, *extra).returncode != 0
    assert data(fixture)['calls'] == []


def test_invalid_late_review_causes_no_writes(fixture):
    fixture[1].write_text('[{"stage":"gate","sha":"short"}]')
    assert record(fixture).returncode != 0
    assert data(fixture)['calls'] == []


@pytest.mark.parametrize('bead', ['', '-x', 'fixture-1 fixture-2'])
def test_requires_one_explicit_id(fixture, bead):
    assert run(fixture, 'record', bead, '--outcome', 'no-op').returncode != 0
    assert data(fixture)['calls'] == []


def test_legacy_and_noop_do_not_invent_facts(fixture):
    assert run(fixture, 'summary', 'fixture-1').stdout.strip() == 'PR unknown at unknown, gate unknown, read unknown'
    assert run(fixture, 'record', 'fixture-1', '--outcome', 'no-op').returncode == 0
    assert data(fixture)['metadata'] == {'gc.work_outcome': 'no-op'}
    assert len(data(fixture)['events']) == 1
    assert run(fixture, 'summary', 'fixture-1').stdout.startswith('no-op; PR unknown')


def test_reader_ignores_other_producers_and_wrong_types(fixture):
    assert record(fixture).returncode == 0
    events = data(fixture)['events']
    for event in events:
        payload = json.loads(event['payload'])
        if payload['fact'] == 'review':
            event['kind'] = 'land'
    assert module.summary(events).endswith('gate unknown, read unknown')
    assert module.summary([{**e, 'source': 'ingest-backfill'} for e in events]) == 'PR unknown at unknown, gate unknown, read unknown'


def test_review_round_order_wins_when_recorded_together_or_replayed(fixture):
    fixture[1].write_text(json.dumps([
        dict(stage='gate', round=3, verdict='CLEAN', sha=SHA, ref='/evidence/gate-3'),
        dict(stage='gate', round=1, verdict='BLOCK', sha=SHA, ref='/evidence/gate-1'),
        dict(stage='gate', round=2, verdict='BLOCK', sha=SHA, ref='/evidence/gate-2'),
    ]))
    assert record(fixture).returncode == 0
    assert run(fixture, 'summary', 'fixture-1').stdout.strip().endswith('gate CLEAN, read unknown')
    fixture[1].write_text(json.dumps([
        dict(stage='gate', round=1, verdict='BLOCK', sha=SHA, ref='/evidence/late-old-round'),
    ]))
    assert record(fixture).returncode == 0
    assert run(fixture, 'summary', 'fixture-1').stdout.strip().endswith('gate CLEAN, read unknown')


@pytest.mark.parametrize('middle', ['blocked', 'new-head'])
def test_repeated_outcome_is_a_new_transition_but_immediate_retry_is_not(fixture, middle):
    assert record(fixture).returncode == 0
    args = ['--outcome', 'blocked'] if middle == 'blocked' else ['--outcome', 'shipped', '--commit', OLD_SHA, '--pr', PR]
    assert run(fixture, 'record', 'fixture-1', *args).returncode == 0
    assert record(fixture).returncode == 0
    assert run(fixture, 'summary', 'fixture-1').stdout.strip() == f'PR 123 at {SHA}, gate CLEAN, read CLEAN'
    outcomes = [e for e in data(fixture)['events'] if json.loads(e['payload'])['fact'] == 'outcome']
    assert len(outcomes) == 3
    assert json.loads(outcomes[-1]['payload'])['previous'] == outcomes[-2]['id']
    count = len(data(fixture)['events'])
    assert record(fixture).returncode == 0
    assert len(data(fixture)['events']) == count


def test_metadata_failure_after_outcome_append_is_retryable(fixture):
    result = subprocess.run(['sh', '-c', close_script()], env={**fixture[2], 'FAIL_FACT': 'metadata'},
                            text=True, capture_output=True)
    assert result.returncode != 0
    assert not data(fixture)['closed']
    assert len(data(fixture)['events']) == 8
    assert record(fixture).returncode == 0
    assert len(data(fixture)['events']) == 8
    assert data(fixture)['metadata']['gc.work_outcome'] == 'shipped'


def test_reader_rejects_forked_outcomes_and_conflicting_same_round_reviews(fixture):
    assert record(fixture).returncode == 0
    reviews = json.loads(fixture[1].read_text())
    reviews[-1].update(verdict='BLOCK', ref='/evidence/conflicting-read')
    fixture[1].write_text(json.dumps(reviews))
    assert record(fixture).returncode == 0
    assert run(fixture, 'summary', 'fixture-1').stdout.strip().endswith('read conflicting')
    events = data(fixture)['events']
    outcome = next(e for e in events if json.loads(e['payload'])['fact'] == 'outcome')
    with pytest.raises(ValueError, match='ambiguous'):
        module.summary(events + [{**outcome, 'id': 'a-different-root'}])
