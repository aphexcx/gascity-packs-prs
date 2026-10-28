"""Regression flows for the two blocked reviews; all transcript text is synthetic."""
import datetime as dt
import json
import os
from pathlib import Path
import sys
import time

import pytest

from test_shadow_flow import harness, row, conversation, run
import shadow_common as common
import shadow_watch as watch
import shadow_judge as judge


def test_pending_tool_waits_for_result_and_terminal_assistant():
    start = time.time() - 300
    rows = [row('user', 'trigger', 'Synthetic request'), row('assistant', 'call', [
        {'type': 'tool_use', 'id': 'pending', 'name': 'Bash', 'input': {'command': 'gc bd list'}}])]
    for r in rows:
        r['timestamp'] = dt.datetime.fromtimestamp(start, dt.timezone.utc).isoformat()
    assert watch.completed_turns(rows, 240) == []
    rows.append(row('user', 'result', [{'type': 'tool_result', 'tool_use_id': 'pending', 'content': 'success'}]))
    assert watch.completed_turns(rows, 240) == []
    rows.append(row('assistant', 'end', 'Synthetic complete'))
    now = common.timestamp(rows[-1]['timestamp'])
    assert watch.completed_turns(rows, 240, now + 239) == []
    assert watch.completed_turns(rows, 240, now + 240) == [(0, 4)]


def test_max_wait_marks_incomplete_and_judge_refreshes(harness):
    cfg, source, rows = harness
    trigger = row('user', 'pending-trigger', 'Synthetic request')
    trigger['timestamp'] = dt.datetime.fromtimestamp(time.time()-2800, dt.timezone.utc).isoformat()
    pending = row('assistant', 'call', [{'type': 'tool_use', 'id': 'pending', 'name': 'Read', 'input': {'file_path': 'synthetic'}}])
    partial = [trigger, pending]
    assert watch.completed_turns(partial, 240) == [(0, 2)]
    entry = {'trigger_uuid': 'pending-trigger', 'trigger_text': 'Synthetic request',
             'source': str(source), 'fable_incomplete': True, 'fable': watch.blocks(partial), 'shadow': {'blocks': []}}
    partial += [row('user', 'result', [{'type': 'tool_result', 'tool_use_id': 'pending', 'content': 'success'}]),
                row('assistant', 'end', 'Synthetic final evidence')]
    source.write_text(''.join(json.dumps(r)+'\n' for r in partial))
    packet = judge.packet(cfg, entry, synthetic=True)
    assert packet['fable'][-1]['text'] == 'Synthetic final evidence'
    assert packet['fable_tool_results'][0]['excerpt'] == 'success'


@pytest.mark.parametrize('text', ['[Request interrupted by user]', '[Request interrupted by user for tool use]',
                                  '<command-name>/synthetic</command-name>', '<local-command-stdout>synthetic</local-command-stdout>'])
def test_trigger_exclusions(text):
    assert not watch.prompt(row('user', 'excluded', text))


def test_replay_only_hook_emits_captured_context(tmp_path):
    import subprocess
    rows = [row('user', 'trigger', 'Synthetic trigger'),
            row('attachment', 'mail', attachment={'type': 'hook_success', 'content': 'synthetic mail', 'stdout': 'synthetic mail'}),
            row('attachment', 'nudge', attachment={'type': 'hook_additional_context', 'content': ['synthetic nudge']}),
            row('assistant', 'response', 'Synthetic answer')]
    captured = watch.replay_context(rows, 0, len(rows))
    assert captured == 'synthetic mail\nsynthetic nudge'
    file = tmp_path/'context.txt'
    common.write_private(file, captured)
    env = dict(os.environ, SHADOW_WORK_DIR=str(tmp_path), SHADOW_REPLAY_CONTEXT=str(file))
    r = subprocess.run([sys.executable, '-I', str(common.PACK/'scripts/shadow_replay.py')], env=env, capture_output=True, text=True)
    assert r.returncode == 0 and r.stdout == captured
    settings = json.loads((common.PACK/'assets/shadow-settings.json').read_text())
    assert 'gc hook' not in json.dumps(settings)
    assert settings['env']['BASH_MAX_TIMEOUT_MS'] == '60000'


def test_real_fork_uses_captured_post_prompt_attachments(harness):
    cfg, source, rows = harness
    rows.insert(6, row('attachment', 'injected', attachment={'type': 'hook_additional_context', 'content': ['synthetic queued mail']}))
    source.write_text(''.join(json.dumps(r)+'\n' for r in rows))
    _, entry = watch.run_turn(cfg, source, 'synthetic', rows, (5, 10), 1, model='claude-synthetic')
    assert entry['status'] == 'completed'
    assert Path(entry['shadow']['replay_context_file']).read_text() == 'synthetic queued mail'
    prefix = common.jsonl(Path(entry['shadow']['fork_file']))
    assert prefix[0]['subtype'] == 'compact_boundary'
    assert all(r['uuid'] != 'role' for r in prefix)
    assert entry['shadow']['token_reservation'] == cfg['max_turns'] * (entry['shadow']['context_estimate'] + cfg['max_output_tokens'])
    assert common.totals([(Path('synthetic'), entry)])['reserved_tokens'] == 0


def test_truncated_turn_is_judgeable(harness, monkeypatch):
    real = watch.classify_error
    monkeypatch.setattr(watch, 'classify_error', lambda *args: 'max_turns')
    _, entry = run(harness)
    assert entry['status'] == 'completed_truncated'
    assert entry['diff']['same_action']
    assert real([{'type': 'result', 'is_error': True, 'subtype': 'error_max_turns'}], '', 1, None) == 'max_turns'
    card = judge.scorecard('2026-10-28', 'opus', [(Path('synthetic'), entry)], [], [])
    assert card['same_decision_percent'] == 100


def test_read_denials_and_timers_are_not_actions():
    read = {'type': 'tool_use', 'name': 'Bash', 'input': {'command': 'cat $(date)'}}
    timer = {'type': 'tool_use', 'name': 'Monitor', 'input': {}}
    write = {'type': 'tool_use', 'name': 'Bash', 'input': {'command': 'gc mail inbox | head -2; gc mail send synthetic-recipient -m synthetic'}}
    assert watch.action([read, timer]) is None
    assert watch.action([read, timer, write]) == {'tool': 'Bash', 'target': 'synthetic-recipient', 'command_head': ['gc', 'mail', 'send']}
    assert watch.diff([read, write], [timer, write])['same_action']


def fixture_project(tmp_path, monkeypatch):
    cfg = {'source_config_dir': str(tmp_path/'claude'), 'city_dir': str(tmp_path/'city')}
    project = common.project_dir(Path(cfg['source_config_dir']), Path(cfg['city_dir']))
    project.mkdir(parents=True)
    monkeypatch.setattr(common.subprocess, 'run', lambda *a, **k: type('Result', (), {'stdout': '{"sessions": []}'})())
    return cfg, project


def test_discovery_streams_past_head_and_ignores_tool_quotes(tmp_path, monkeypatch):
    cfg, project = fixture_project(tmp_path, monkeypatch)
    old = project/'old.jsonl'
    stale = row('user', 'stale', '# Synthetic\n\nYou are the mayor of this Gas City workspace.')
    stale['timestamp'] = '2020-01-01T00:00:00Z'
    old.write_text(json.dumps(stale)+'\n')
    live = project/'live.jsonl'
    live.write_text(json.dumps({'type': 'last-prompt', 'synthetic_padding': 'x'*210000})+'\n'+
                    json.dumps(row('attachment', 'role', attachment={'type': 'instructions', 'content': '# Synthetic\n\nYou are the mayor of this Gas City workspace.'}))+'\n'+
                    json.dumps(row('user', 'current', 'Synthetic current trigger'))+'\n')
    quoted = project/'quoted.jsonl'
    quoted.write_text(json.dumps(row('user', 'quote', [{'type': 'tool_result', 'content': '\nYou are the mayor of this Gas City workspace.'}]))+'\n')
    for p, age in ((old, 300), (live, 100), (quoted, 0)):
        os.utime(p, (time.time()-age, time.time()-age))
    source, by = common.discover(cfg)
    assert source == live and by == 'paragraph-start-full-scan'
    assert not common.source_info(live)['stale']
    assert common.source_info(old)['stale']
    assert not common.source_info(quoted)['marker']


def test_stale_gc_session_falls_back(tmp_path, monkeypatch):
    cfg, project = fixture_project(tmp_path, monkeypatch)
    key = '00000000-0000-0000-0000-000000000000'
    stale = row('user', 'old', 'Synthetic old')
    stale['timestamp'] = '2020-01-01T00:00:00Z'
    (project/(key+'.jsonl')).write_text(json.dumps(stale)+'\n')
    live = project/'live.jsonl'
    live.write_text(json.dumps(row('user', 'role', '\nYou are the mayor of this Gas City workspace.'))+'\n')
    monkeypatch.setattr(common.subprocess, 'run', lambda *a, **k: type('Result', (), {'stdout': json.dumps({'sessions': [{'alias': 'mayor', 'session_key': key}]})})())
    assert common.discover(cfg)[0] == live


def test_stale_source_health_prevents_any_ledger(harness):
    cfg, source, rows = harness
    for r in rows:
        r['timestamp'] = '2020-01-01T00:00:00Z'
    source.write_text(''.join(json.dumps(r)+'\n' for r in rows))
    args = type('Args', (), {'transcript': str(source), 'once': True, 'backfill': 0, 'model': 'claude-synthetic'})()
    assert watch.tick(cfg, args) == 1
    root = Path(cfg['work_dir'])
    assert common.ledgers(root) == []
    assert json.loads((root/'watch-health.json').read_text())['last_error'] == 'stale_source'


@pytest.mark.parametrize('value', [None, '', 'off', 'OFF', 'garbage'])
def test_switch_off_does_not_create_ledger_and_only_changes_health_once(harness, value):
    cfg, source, rows = harness
    if value is None:
        cfg['kill_switch'] += '-absent'
    else:
        Path(cfg['kill_switch']).write_text(value)
    args = type('Args', (), {'transcript': str(source), 'once': True, 'backfill': 0, 'model': 'claude-synthetic'})()
    assert watch.tick(cfg, args) == 0
    file = Path(cfg['work_dir'])/'watch-health.json'
    modified = file.stat().st_mtime_ns
    assert watch.tick(cfg, args) == 0
    assert file.stat().st_mtime_ns == modified
    assert common.ledgers(Path(cfg['work_dir'])) == []
    assert run(harness)[0] is None


def test_incremental_tail_survives_restart_partial_append_and_regression(tmp_path):
    source, work = tmp_path/'source', tmp_path/'work'
    first = json.dumps(row('user', 'first', 'Synthetic first'))+'\n'
    source.write_text(first+'{"type":')
    assert [r['uuid'] for r in common.tail_rows(source, work)] == ['first']
    with source.open('a') as f:
        f.write('"system","uuid":"second"}\n')
    assert [r['uuid'] for r in common.tail_rows(source, work)] == ['first', 'second']
    common._TAILS.clear()
    assert [r['uuid'] for r in common.tail_rows(source, work)] == ['first', 'second']
    source.write_text('{"type":"user","uuid":"replacement"}\n')
    assert [r['uuid'] for r in common.tail_rows(source, work)] == ['replacement']
    assert len(list((work/'runtime/tail-cache').glob('*.jsonl'))) == 2


def test_source_metadata_scans_only_appended_bytes(tmp_path, monkeypatch):
    source = tmp_path/'source'
    source.write_text(json.dumps(row('user', 'first', '\nYou are the mayor of this Gas City workspace.'))+'\n')
    assert common.source_info(source)['marker']
    offset = source.stat().st_size
    with source.open('a') as f:
        f.write(json.dumps(row('user', 'second', 'Synthetic second'))+'\n')
    assert common.source_info(source)['marker']
    state = next(v for k, v in common._SOURCE_PROGRESS.items() if k[0] == str(source))
    assert state['offset'] == source.stat().st_size > offset


def test_sampling_spreads_admission_across_cst_hours():
    cfg = {'target_turns_per_day': 40, 'sample_every': 0}
    state = {}
    counts = []
    for hour in range(24):
        now = dt.datetime(2026, 10, 28, hour, tzinfo=common.CST).timestamp()
        counts.append(sum(watch.sample_turn(cfg, state, {'uuid': f'{hour}-{i}'}, now) for i in range(20)))
    assert sum(counts) == 40
    assert min(counts) >= 1 and max(counts) <= 2
    assert state['sampling']['k'] > 1
    assert 0 < state['sampling']['expected_coverage'] < 1


def test_sample_every_override_and_duplicate_poll():
    cfg = {'target_turns_per_day': 40, 'sample_every': 3}
    state = {}
    now = dt.datetime(2026, 10, 28, 1, tzinfo=common.CST).timestamp()
    assert sum(watch.sample_turn(cfg, state, {'uuid': str(i)}, now) for i in range(10)) == 4
    assert not watch.sample_turn(cfg, state, {'uuid': '0'}, now)


def test_init_fidelity_uses_actual_event_not_copied_snapshot(tmp_path):
    source = [row('attachment', 'snapshot', attachment={'type': 'prompt_snapshot', 'systemPrompt': ['Synthetic']})]
    seed = tmp_path/'seed.jsonl'
    common.write_private(seed, ''.join(json.dumps(r)+'\n' for r in watch.fork_rows(source, 1, 'seed')))
    proof = watch.fork_evidence(source, seed, seed, {'type': 'system', 'subtype': 'init', 'tools': ['Read'], 'skills': ['synthetic']})
    assert proof['runtime_init_sha256']
    assert proof['runtime_init_tools'] == ['Read']
    assert proof['runtime_init_skills'] == ['synthetic']
    assert proof['runtime_system_prompt_verified'] is False


def test_judge_results_bounded_and_deidentified():
    result = judge.result_excerpts([{'tool_use_id': 'customer-name', 'content': 'Alice Example at https://private.example/a says success ' + 'x'*1000}])
    assert len(result[0]['excerpt']) <= 600
    assert 'Alice' not in result[0]['excerpt'] and 'private.example' not in result[0]['excerpt']
    assert 'tool_use_id' not in result[0]


def test_judge_reason_word_limit():
    value = {'scores': dict.fromkeys(judge.CRITERIA, 2), 'human_intervention_needed': False,
             'shadow_better': False, 'reason': 'The proposed reply used verified evidence and waited for approval.', 'severity': 'NOTE', 'violations': []}
    assert judge.validate(value) == value
    with pytest.raises(ValueError):
        judge.validate({**value, 'reason': 'word '*21})


def test_sampling_does_not_requeue_unsampled_turns_on_next_day():
    cfg = {'target_turns_per_day': 40, 'sample_every': 2}
    state = {}
    now = dt.datetime(2026, 10, 28, 1, tzinfo=common.CST).timestamp()
    assert watch.sample_turn(cfg, state, {'uuid': 'first'}, now)
    assert not watch.sample_turn(cfg, state, {'uuid': 'unsampled'}, now)
    assert not watch.sample_turn(cfg, state, {'uuid': 'unsampled'}, now+86400)
    assert state['sampling']['eligible'] == 0


def test_secret_export_cached_once_even_when_key_is_absent(monkeypatch):
    cfg = {'secret_name': 'SYNTHETIC_MOONSHOT_KEY', 'infisical_project': 'synthetic-cache-test'}
    calls = []
    monkeypatch.delenv(cfg['secret_name'], raising=False)
    def export(*args, **kwargs):
        calls.append(1)
        return type('Result', (), {'returncode': 0, 'stdout': '{}'})()
    monkeypatch.setattr(common.subprocess, 'run', export)
    for _ in range(2):
        with pytest.raises(RuntimeError):
            common.moonshot_key(cfg)
    assert len(calls) == 1


def test_quoted_mutation_words_in_read_are_not_outward_actions():
    assert watch.action([{'type': 'tool_use', 'name': 'Bash', 'input': {'command': "grep 'gc mail send' ~/city/docs/guide.md"}}]) is None
    out = watch.action([{'type': 'tool_use', 'name': 'Bash', 'input': {'command': 'gc slack publish --session=synthetic-channel --body synthetic'}}])
    assert out['target'] == 'synthetic-channel'


def test_tool_result_redaction_covers_lowercase_names_and_dictionary_keys():
    result = judge.result_excerpts([{'content': {'alice': 'bob', 'status': 'success', 'records': 2}},
                                   {'content': 'alice and bob returned success; records=2'}])
    text = json.dumps(result)
    assert 'alice' not in text and 'bob' not in text
    assert 'success' in text and 'records' in text
    assert all(len(r['excerpt']) <= 600 for r in result)


def test_private_storage_modes_include_created_parents(tmp_path):
    target = tmp_path/'private'/'ledger'/'day'/'entry.json'
    common.save(target, {'synthetic': True})
    for p in (target.parent, target.parent.parent, target.parent.parent.parent):
        assert p.stat().st_mode & 0o777 == 0o700
    assert target.stat().st_mode & 0o777 == 0o600


def test_child_process_inherits_private_umask(tmp_path):
    argv = [sys.executable, '-I', '-c', "from pathlib import Path; Path('child').mkdir(); Path('child/file').write_text('synthetic')"]
    code, _, error = common.run_process(argv, os.environ.copy(), str(tmp_path), tmp_path/'out', tmp_path/'err', 10)
    assert code == 0 and error is None
    assert (tmp_path/'child').stat().st_mode & 0o777 == 0o700
    assert (tmp_path/'child/file').stat().st_mode & 0o777 == 0o600


@pytest.mark.parametrize('tool,mutates', [('CronCreate', True), ('CronDelete', True), ('SendMessage', True),
                                        ('Monitor', False), ('ScheduleWakeup', False), ('CronList', False)])
def test_cron_mutations_are_distinct_from_timers(tool, mutates):
    action = watch.action([{'type': 'tool_use', 'name': tool, 'input': {'recipient': 'synthetic'}}])
    assert (action is not None) == mutates


def test_redirection_mutation_retains_target_even_when_read_parser_rejects_shape():
    call = lambda target: {'type': 'tool_use', 'name': 'Bash', 'input': {'command': "printf 'synthetic\\n' >> " + target}}
    first, second = call('~/city/synthetic-a'), call('~/city/synthetic-b')
    assert watch.action([first]) == {'tool': 'Bash', 'target': '~/city/synthetic-a', 'command_head': ['printf']}
    assert not watch.diff([first], [second])['same_action']
    assert watch.action([{'type': 'tool_use', 'name': 'Bash', 'input': {'command': "printf '>> synthetic'"}}]) is None


def test_discovery_accepts_rendered_attachment_marker_but_not_embedded_tool_result():
    r = row('attachment', 'rendered', attachment={'type': 'instructions'},
            rendered=[{'type': 'text', 'text': '\nYou are the mayor of this Gas City workspace.'}])
    assert common.marker_in_row(r)
    r['rendered'] = [{'type': 'tool_result', 'content': '\nYou are the mayor of this Gas City workspace.'}]
    assert not common.marker_in_row(r)


def test_worker_alias_and_store_overrides_cannot_shadow_mayor_identity(monkeypatch):
    worker_keys = ('GC_ALIAS', 'GC_AGENT', 'GC_BEADS', 'GC_BEADS_SCOPE_ROOT', 'GC_SESSION_ORIGIN',
                   'GC_TRIGGER_BEAD_ID', 'GC_WORKTREE_BEAD_ID', 'GC_INSTANCE_TOKEN')
    for key in worker_keys:
        monkeypatch.setenv(key, 'synthetic-worker-value')
    env = common.normal_env()
    assert not any(key in env for key in worker_keys)
    env.update(GC_AGENT='mayor')
    identity = env.get('GC_SESSION_ID') or env.get('GC_ALIAS') or env['GC_AGENT']
    assert identity == 'mayor'
