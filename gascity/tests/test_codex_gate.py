"""Gate contract checks with a fake CLI; no network or model usage."""
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import textwrap

import pytest

SCRIPT = Path(__file__).resolve().parents[1] / 'assets/scripts/codex-gate.sh'
DOC = Path(__file__).resolve().parents[2] / 'docs/ops/citadel-codex-gates.md'
SHIM = SCRIPT.parent / 'codex-infisical-shim.sh'
README = Path(__file__).resolve().parents[1] / 'README.md'
FRAGMENT = Path(__file__).resolve().parents[1] / 'template-fragments/gc-role-worker.template.md'


def gate_default_model():
    models = re.findall(r'^model=(\S+)$', SCRIPT.read_text(encoding='utf-8'), re.M)
    assert len(models) == 1, f'{SCRIPT.name}: expected one model= default'
    return models[0]


def fence_text(path, heading):
    section = path.read_text(encoding='utf-8').partition(f'\n{heading}\n')[2].partition('\n## ')[0]
    block = re.search(r'^( *)```bash\n(.*?)^\1```$', section, re.M | re.S)
    assert block, f'{path.name}: expected a bash block under {heading}'
    return textwrap.dedent(block[2])


def documented_argv(text, env):
    lines = text.replace('\\\n', '').splitlines()
    return [shlex.split(line, comments=True)[1:] for line in lines if line.strip()]


def assert_passes_model(args, model, message):
    assert args.count('--model') == 1, message
    index = args.index('--model') + 1
    assert args[index:index + 1] == [model], message


def test_doc_examples_and_cli_pins_follow_the_gate_default(command_env):
    model = gate_default_model()
    examples = documented_argv(fence_text(DOC, '## Gate helper'), command_env)
    assert [args[:1] for args in examples] == [['review'], ['exec']], (
        f'{DOC.name}: expected the review and exec examples under Gate helper'
    )
    for args in examples:
        assert_passes_model(args, model, f'{DOC.name}: the {args[0]} example must pass --model {model}')

    pin = r'@openai/codex@([^\s`\'"]+)'
    versions = re.findall(pin, README.read_text(encoding='utf-8'))
    assert len(versions) == 1, f'{README.name}: expected one Codex CLI install pin'
    cli = versions[0]
    usage = re.search(r"<<'USAGE'\n(.*?)^USAGE$", SCRIPT.read_text(encoding='utf-8'), re.M | re.S)
    assert usage and f'on Codex {cli})' in usage[1], f'{SCRIPT.name}: usage must name Codex {cli}'
    assert re.findall(pin, SHIM.read_text(encoding='utf-8')) == [cli, cli], (
        f'{SHIM.name}: both example pins must match Codex {cli}'
    )


def test_worker_prompt_gate_command_follows_the_gate_default(command_env):
    model = gate_default_model()
    commands = documented_argv(fence_text(FRAGMENT, '## Codex gates'), command_env)
    assert len(commands) == 1 and commands[0][:1] == ['review'], (
        f'{FRAGMENT.name}: expected one review command under Codex gates'
    )
    assert_passes_model(commands[0], model, f'{FRAGMENT.name}: the review command must pass --model {model}')


@pytest.mark.parametrize('tail', ['--model {model}#old', '# --model {model}'])
def test_pin_reader_sees_what_bash_passes(command_env, tail):
    model = gate_default_model()
    command = 'codex-gate.sh exec "$PROMPT_FILE" ' + tail.format(model=model)
    args, = documented_argv(command, command_env)
    with pytest.raises(AssertionError, match='expected the exact model'):
        assert_passes_model(args, model, 'expected the exact model')


@pytest.fixture
def command_env(tmp_path):
    bindir, city, scratch = (tmp_path / name for name in ('bin', 'city', 'scratch'))
    bindir.mkdir()
    scratch.mkdir()
    installed = city / 'assets/ops/mayor-tools/codex-gate.sh'
    installed.parent.mkdir(parents=True)
    installed.symlink_to(bindir / 'codex-gate.sh')
    fallback = bindir / 'codex'
    fallback.write_text('#!/bin/sh\necho fallback >> "$UNEXPECTED_CALLS"\nexit 97\n')
    fallback.chmod(0o755)
    env = {
        'HOME': str(tmp_path), 'TMPDIR': str(scratch), 'GC_CITY_PATH': str(city),
        'PATH': str(bindir) + os.pathsep + os.environ['PATH'],
        'UNEXPECTED_CALLS': str(tmp_path / 'unexpected-calls'),
        'STUB_DIR': str(tmp_path), 'NEW_MODEL': gate_default_model(),
        'PROMPT_FILE': str(tmp_path / 'prompt file.txt'),
        'REPO': str(tmp_path / 'repo'), 'REPO_DIR': str(tmp_path / 'repo'),
        'REVIEW_OUTPUT': str(tmp_path / 'real record'), 'REVIEW_BASE': 'review-base',
        'REVIEW_CLASS': 'STANDARD',
    }
    assert Path(env['GC_CITY_PATH']).resolve().parent == tmp_path.resolve()
    return env


@pytest.fixture
def run_probe(tmp_path, command_env):
    (tmp_path / 'bin/codex-gate.sh').symlink_to(SCRIPT)
    record = Path(command_env['REVIEW_OUTPUT'])
    record.write_text('VERDICT: BLOCK\n')
    Path(f'{record}.log').write_text('the real transcript\n')
    fence = fence_text(DOC, '## Moving a city to a new gate model')

    def run(*, prelude='', tail=''):
        script = '\n'.join(part for part in (prelude, fence, tail) if part)
        script += '\nstatus=$?\nprintf "PATH-AFTER=%s\\n" "$PATH"\nexit "$status"'
        return subprocess.run(['bash', '-c', script], env=command_env, cwd=tmp_path,
                              capture_output=True, text=True, timeout=10)
    return run


def assert_original_record(env):
    record = Path(env['REVIEW_OUTPUT'])
    assert record.read_text() == 'VERDICT: BLOCK\n', 'the probe replaced the real verdict'
    assert Path(f'{record}.log').read_text() == 'the real transcript\n'


def probe_records(result, env, models):
    records = [Path(p) for p in re.findall(r"^.*the stub's record is (.+)$", result.stdout, re.M)]
    assert len(records) == len(models), f'expected {len(models)} named records: {result.stderr}'
    assert len(set(records)) == len(records)
    calls = []
    for path, model in zip(records, models):
        assert path.resolve().parent == Path(env['TMPDIR']).resolve()
        args = json.loads(path.read_text())['args']
        assert args[args.index('-m') + 1] == model
        assert [a for a in args if a.startswith('review_model=')] == [f'review_model="{model}"']
        calls.append(args)
    return calls


@pytest.mark.usefixtures('gate')
def test_gate_probe_reports_the_model_each_caller_sends(command_env, run_probe):
    model = command_env['NEW_MODEL']
    worker = fence_text(FRAGMENT, '## Codex gates')
    assert worker.count(f'--model {model}') == 1
    scratch = Path(command_env['TMPDIR'])
    for worker_model in (model, 'previous-gate-model'):
        existing = set(scratch.iterdir())
        caller = worker.replace(f'--model {model}', f'--model {worker_model}')
        result = run_probe(tail='gate_probe ' + caller)
        assert_original_record(command_env)
        assert result.stdout.splitlines()[-1] == 'PATH-AFTER=' + command_env['PATH']
        reports = [line for line in result.stdout.splitlines() if line.startswith('gate_probe: -m ')]
        expected = [f'gate_probe: -m {m}, review_model {m}: ' + ('OK' if m == model else 'STALE')
                    for m in (model, worker_model)]
        assert reports == expected, f'expected reports for default and worker: {result.stderr}'
        assert result.returncode == (0 if worker_model == model else 1)
        default_args, worker_args = probe_records(result, command_env, [model, worker_model])
        assert Path(command_env['PROMPT_FILE']).read_text().strip() in default_args[-1]
        assert 'Inspect git diff ' in worker_args[-1]
        fresh = set(scratch.iterdir()) - existing
        assert sum(p.read_text() == 'VERDICT: CLEAN\n' for p in fresh if p.suffix != '.log') == 2
        assert not Path(command_env['UNEXPECTED_CALLS']).exists()


@pytest.mark.usefixtures('gate')
@pytest.mark.parametrize('stub', ['empty', 'non-executable', 'function'])
def test_gate_probe_refuses_an_unusable_stub(tmp_path, command_env, run_probe, stub):
    prelude = ''
    if stub == 'function':
        prelude = 'codex() { echo function >> "$UNEXPECTED_CALLS"; return 97; }; export -f codex'
    else:
        directory = tmp_path / stub
        directory.mkdir()
        command_env['STUB_DIR'] = str(directory)
        if stub == 'non-executable':
            binary = directory / 'codex'
            binary.write_bytes((tmp_path / 'codex').read_bytes())
            binary.chmod(0o644)
    result = run_probe(prelude=prelude)
    calls = Path(command_env['UNEXPECTED_CALLS'])
    assert not calls.exists(), f'unexpected codex call: {calls.read_text().strip()}'
    assert result.returncode != 0
    assert len(result.stderr.splitlines()) == 1
    assert_original_record(command_env)
    assert not list(Path(command_env['TMPDIR']).iterdir())


@pytest.mark.usefixtures('gate')
def test_gate_probe_names_its_own_record(command_env, run_probe):
    assert 'CAPTURE' not in command_env
    result = run_probe()
    probe_records(result, command_env, [command_env['NEW_MODEL']])
    assert result.returncode == 0


@pytest.fixture
def gate(tmp_path):
    binary = tmp_path / 'codex'
    binary.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
pathlib.Path(os.environ['CAPTURE']).write_text(json.dumps({'args': args, 'stdin': sys.stdin.read()}))
pathlib.Path(args[args.index('--output-last-message')+1]).write_text(os.environ.get('ANSWER', 'VERDICT: CLEAN\\n'))
print('VERDICT: CLEAN')  # A transcript match must never pass the gate.
sys.exit(int(os.environ.get('CODEX_EXIT', '0')))
''')
    binary.chmod(0o755)
    repo = tmp_path / 'repo'
    subprocess.run(['git', 'init', '-q', str(repo)], check=True)
    source = repo / 'source.txt'
    source.write_text('base\n')
    subprocess.run(['git', '-C', str(repo), 'add', 'source.txt'], check=True)
    subprocess.run(['git', '-C', str(repo), '-c', 'user.name=Gate Test',
                    '-c', 'user.email=gate@example.invalid', 'commit',
                    '-qm', 'base'], check=True)
    subprocess.run(['git', '-C', str(repo), 'branch', 'review-base'], check=True)
    source.write_text('delta\n')
    subprocess.run(['git', '-C', str(repo), '-c', 'user.name=Gate Test',
                    '-c', 'user.email=gate@example.invalid', 'commit',
                    '-qam', 'delta'], check=True)
    prompt = tmp_path / 'prompt file.txt'
    prompt.write_text('Review a literal `command` and $(touch nope).\nSecond line.')
    output = tmp_path / 'answer file.txt'
    capture = tmp_path / 'capture.json'

    def run(mode='exec', *, answer='VERDICT: CLEAN\n', exit_code=0, extra=(), dirty=None):
        if dirty == 'untracked':
            (repo / 'new.txt').write_text('unreviewed\n')
        elif dirty:
            source.write_text('unreviewed\n')
            if dirty == 'staged':
                subprocess.run(['git', '-C', str(repo), 'add', 'source.txt'], check=True)
        args = ['exec', str(prompt)] if mode == 'exec' else ['review', '--base', 'review-base']
        result = subprocess.run(
            ['bash', str(SCRIPT), *args, '-C', str(repo), '--output', str(output), *extra],
            input='inherited stdin must not reach Codex', text=True, capture_output=True,
            env={**os.environ, 'PATH': str(tmp_path) + os.pathsep + os.environ['PATH'],
                 'CAPTURE': str(capture), 'ANSWER': answer, 'CODEX_EXIT': str(exit_code)},
            timeout=10,
        )
        return result, json.loads(capture.read_text()) if capture.exists() else None, output
    return run


@pytest.mark.parametrize('mode', ['exec', 'review'])
def test_city_astra_invocation_and_stdin(gate, mode):
    result, call, output = gate(mode)
    assert result.returncode == 0, result.stderr
    args = call['args']
    assert args[args.index('-p') + 1] == 'city'
    assert args[args.index('-m') + 1] == 'gpt-6.2-astra'
    assert 'review_model="gpt-6.2-astra"' in args
    assert args[args.index('-s') + 1] == 'read-only'
    assert args[args.index('-a') + 1] == 'never'
    assert any(a.startswith('developer_instructions=') for a in args)
    assert '--skip-git-repo-check' in args and '-C' in args
    assert call['stdin'] == ''
    assert output.read_text() == 'VERDICT: CLEAN\n'
    if mode == 'review':
        assert 'QUICK code review of committed changes' in args[-1]
        repo = args[args.index('-C') + 1]
        base = subprocess.check_output(['git', '-C', repo, 'rev-parse', 'review-base'], text=True).strip()
        head = subprocess.check_output(['git', '-C', repo, 'rev-parse', 'HEAD'], text=True).strip()
        assert base != head
        assert f'git diff {base} {head}' in args[-1]
        assert 'review-base' not in args[-1]
    else:
        assert '$(touch nope).\nSecond line.' in args[-1]
        assert not (output.parent / 'nope').exists()


@pytest.mark.parametrize(('answer', 'expected'), [
    ('VERDICT: BLOCK\n', 1),
    ('No verdict\n', 2),
    ('quoted VERDICT: CLEAN\n', 2),
    ('VERDICT: CLEAN\nVERDICT: BLOCK\n', 2),
    ('VERDICT: CLEANISH\n', 2),
    ('VERDICT: CLEAN\nMore findings\n', 2),
    ('```\nVERDICT: CLEAN\n```\n', 2),
    ('```\nVERDICT: CLEAN\n', 2),
    ('   ~~~text\nVERDICT: CLEAN\n', 2),
    ('````\n```\nVERDICT: CLEAN\n', 2),
    ('```\n~~~\nVERDICT: CLEAN\n', 2),
    ('```text\nExample code\n```\nVERDICT: CLEAN\n', 0),
])
def test_only_one_exact_anchored_final_verdict_passes(gate, answer, expected):
    result, _, _ = gate(answer=answer)
    assert result.returncode == expected


def test_model_override_pins_review_model_too(gate):
    result, call, _ = gate('review', extra=('--model', 'test-model'))
    assert result.returncode == 0
    assert call['args'][call['args'].index('-m') + 1] == 'test-model'
    assert 'review_model="test-model"' in call['args']


def test_cli_failure_cannot_pass_or_reuse_stale_verdict(gate):
    result, _, output = gate()
    assert result.returncode == 0
    result, _, _ = gate(answer='VERDICT: CLEAN\n', exit_code=7)
    assert result.returncode == 7
    assert output.read_text() == ''


def test_review_rejects_empty_delta_before_calling_codex(gate):
    result, call, _ = gate('review', extra=('--base', 'HEAD'))
    assert result.returncode == 2
    assert 'nonempty committed delta' in result.stderr
    assert call is None


@pytest.mark.parametrize('dirty', ['untracked', 'modified', 'staged'])
def test_review_rejects_uncommitted_work_before_calling_codex(gate, dirty):
    result, call, _ = gate('review', dirty=dirty)
    assert result.returncode == 2
    assert 'uncommitted or untracked changes' in result.stderr
    assert call is None
