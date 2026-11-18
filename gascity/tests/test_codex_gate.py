"""Gate contract checks with a fake CLI; no network or model usage."""
import json
import os
from pathlib import Path
import re
import shlex
import subprocess

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


def bash_commands(path, heading):
    """The first bash fence under HEADING in PATH, one string per command as bash joins it."""
    section = path.read_text(encoding='utf-8').partition(f'\n{heading}\n')[2].partition('\n## ')[0]
    block = re.search(r'^( *)```bash\n(.*?)^\1```$', section, re.M | re.S)
    assert block, f'{path.name}: expected a bash block under {heading}'
    return [line.strip() for line in block[2].replace('\\\n', '').splitlines() if line.strip()]


def assert_passes_model(args, model, message):
    assert args.count('--model') == 1, message
    index = args.index('--model') + 1
    assert args[index:index + 1] == [model], message


def test_doc_examples_and_cli_pins_follow_the_gate_default():
    model = gate_default_model()
    examples = [shlex.split(line, comments=True) for line in bash_commands(DOC, '## Gate helper')]
    assert [args[:2] for args in examples] == [
        ['codex-gate.sh', 'review'], ['codex-gate.sh', 'exec'],
    ], f'{DOC.name}: expected the review and exec examples under Gate helper'
    for args in examples:
        assert_passes_model(args, model, f'{DOC.name}: the {args[1]} example must pass --model {model}')

    pin = r'@openai/codex@([^\s`\'"]+)'
    versions = re.findall(pin, README.read_text(encoding='utf-8'))
    assert len(versions) == 1, f'{README.name}: expected one Codex CLI install pin'
    cli = versions[0]
    usage = re.search(r"<<'USAGE'\n(.*?)^USAGE$", SCRIPT.read_text(encoding='utf-8'), re.M | re.S)
    assert usage and f'on Codex {cli})' in usage[1], f'{SCRIPT.name}: usage must name Codex {cli}'
    assert re.findall(pin, SHIM.read_text(encoding='utf-8')) == [cli, cli], (
        f'{SHIM.name}: both example pins must match Codex {cli}'
    )


def test_worker_prompt_gate_command_follows_the_gate_default():
    model = gate_default_model()
    commands = [shlex.split(line, comments=True) for line in bash_commands(FRAGMENT, '## Codex gates')]
    assert len(commands) == 1 and commands[0][1:2] == ['review'], (
        f'{FRAGMENT.name}: expected one review command under Codex gates'
    )
    assert_passes_model(commands[0], model, f'{FRAGMENT.name}: the review command must pass --model {model}')


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
