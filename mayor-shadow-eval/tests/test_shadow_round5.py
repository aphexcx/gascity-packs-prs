"""Real hook and operating-system regressions for the two isolation layers."""
import json
import subprocess
import sys
from pathlib import Path

import pytest

from test_shadow_guard import guard, hook
from test_shadow_round3 import read_city
from test_shadow_flow import common, watch, harness, run


SAFE = "~/city/work/stone-videos-1027"


@pytest.mark.parametrize("command,expected", [
    ("du file --exclude .", "deny"),
    ("ls -R file --ignore .", "deny"),
    (f"du -sh {SAFE} --exclude .", "deny"),
    ("wc -l file -- .", "deny"),
    ("ls file -R", "allow"),
    ("tail -n 5 file --follow .", "deny"),
    (f"du -sh {SAFE}", "allow"),
    ("ls ~/city/work", "allow"),
    ("ls file --ignore .", "deny"),
    ("ls ~/city/work -R", "deny"),
    ("du file --exclude ..", "deny"),
    ("du file --exclude ~", "deny"),
    ("du file --exclude '*'", "deny"),
    ("grep pattern --exclude . file", "deny"),
    ("rg pattern --glob . file", "deny"),
    ("grep . --exclude . file", "deny"),
    ("rg . --glob . file", "deny"),
    (f"grep -e -- -r {SAFE}", "allow"),
    (f"rg -e -- {SAFE}", "allow"),
    ("unknown-reader file --exclude .", "deny"),
    ("file file --exclude .", "deny"),
    ("ffprobe file --exclude .", "deny"),
] + [(f"{exe} file --format .", "deny")
     for exe in ("wc", "cat", "head", "tail", "stat", "md5", "shasum")])
def test_bsd_and_ambiguous_operands_through_hook(tmp_path, read_city, command, expected):
    (read_city / "file").write_text("synthetic public fixture\n")
    result = hook(tmp_path, {"cwd": str(read_city), "tool_name": "Bash",
                             "tool_input": {"command": command}})
    assert result.returncode == 0
    output = json.loads(result.stdout)["hookSpecificOutput"]
    assert output["permissionDecision"] == expected
    if expected == "deny":
        assert output["permissionDecisionReason"] == guard.SHAPE_REASON


@pytest.mark.skipif(sys.platform != "darwin", reason="BSD utility behavior")
@pytest.mark.parametrize("argv", [["du", "file", "--exclude", "."],
                                 ["ls", "-R", "file", "--ignore", "."]])
def test_bsd_utilities_really_traverse_and_hook_denies(tmp_path, read_city, argv):
    (read_city / "file").write_text("synthetic public fixture\n")
    (read_city / "assets/private/synthetic-marker").write_text("synthetic fixture\n")
    raw = subprocess.run(argv, cwd=read_city, capture_output=True, text=True, timeout=5)
    assert "assets/private" in raw.stdout
    result = hook(tmp_path, {"cwd": str(read_city), "tool_name": "Bash",
                             "tool_input": {"command": " ".join(argv)}})
    assert json.loads(result.stdout)["hookSpecificOutput"]["permissionDecision"] == "deny"


@pytest.fixture
def seatbelt(tmp_path, monkeypatch):
    if not Path('/usr/bin/sandbox-exec').is_file():
        pytest.skip('macOS sandbox-exec required for OS denial tests')
    home = tmp_path / 'home'
    city = home / 'city'
    source = home / '.claude'
    work = city / 'work/eval'
    runtime = work / 'runtime/current'
    temporary = runtime / 'tmp'
    decisions = work / 'turns/current/decisions.jsonl'
    temporary.mkdir(parents=True)
    decisions.parent.mkdir(parents=True)
    source.mkdir()
    monkeypatch.setenv('HOME', str(home))
    config = city / 'ops/shadow-eval.toml'
    config.parent.mkdir()
    config.write_text('synthetic operator config')
    cfg = {'city_dir': str(city), 'work_dir': str(work), 'source_config_dir': str(source),
           'config_path': str(config), 'sandbox': True, '_shadow_runtime': str(runtime),
           '_shadow_decisions': str(decisions), 'sandbox_excluded_paths': ['extra-private']}
    profile = runtime / 'shadow.sb'
    profile.write_text(common.shadow_sandbox_profile(cfg, runtime, temporary, decisions))
    return cfg, profile


def sandbox_run(profile, argv, cwd):
    return subprocess.run(['/usr/bin/sandbox-exec', '-f', str(profile), *map(str, argv)],
                          cwd=cwd, capture_output=True, text=True, timeout=10)


@pytest.mark.parametrize('relative', [
    'city/assets/private/fixture', 'city/.gc/keys/fixture',
    'city/.gc/services/new-service/secrets/fixture', 'city/ops/shadow-eval.toml',
    'city/extra-private/fixture', 'city/work/eval/ledger/day/fixture',
    'city/work/eval-kimi-k31/ledger/day/fixture',
    'city/work/eval/runtime/previous/session.jsonl', '.config/infisical-agent/fixture',
    '.claude/.credentials.json', '.claude/settings.json',
    '.claude/projects/live/session.jsonl',
])
def test_os_denies_excluded_reads_even_created_after_profile(seatbelt, relative):
    cfg, profile = seatbelt
    home = Path(cfg['city_dir']).parent
    target = home / relative
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text('synthetic excluded fixture\n')
    result = sandbox_run(profile, ['/bin/cat', target], cfg['city_dir'])
    assert result.returncode != 0
    assert 'Operation not permitted' in result.stderr
    assert 'synthetic excluded fixture' not in result.stdout


def test_os_denies_du_option_bypass_without_the_hook(seatbelt):
    cfg, profile = seatbelt
    city = Path(cfg['city_dir'])
    safe = city / 'safe'
    safe.write_text('public fixture')
    excluded = city / 'assets/private'
    excluded.mkdir(parents=True)
    (excluded / 'fixture').write_text('synthetic excluded fixture')
    result = sandbox_run(profile, ['/usr/bin/du', safe, '--exclude', excluded], city)
    assert result.returncode != 0
    assert 'Operation not permitted' in result.stderr
    assert str(excluded) not in result.stdout


def test_os_allows_context_and_current_runtime_but_confines_writes(seatbelt):
    cfg, profile = seatbelt
    city, source, runtime = (Path(cfg[k]) for k in ('city_dir', 'source_config_dir', '_shadow_runtime'))
    memory = common.project_dir(source, city) / 'memory/MEMORY.md'
    context = [city / 'CLAUDE.md', source / 'CLAUDE.md', source / 'skills/example/SKILL.md', memory]
    for target in context:
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text('synthetic context\n')
        result = sandbox_run(profile, ['/bin/cat', target], city)
        assert result.returncode == 0, result.stderr
    for target, allowed in [(runtime / 'fork.jsonl', True), (runtime / 'tmp/scratch', True),
                            (Path(cfg['_shadow_decisions']), True), (city / 'must-not-write', False),
                            (source / 'must-not-write', False), (Path(cfg['work_dir']) / 'ledger-new', False)]:
        result = sandbox_run(profile, ['/usr/bin/touch', target], city)
        assert (result.returncode == 0) == allowed, result.stderr
        assert target.exists() == allowed
    # The hook can append decisions, but the shadow cannot read them back.
    result = sandbox_run(profile, ['/bin/cat', cfg['_shadow_decisions']], city)
    assert result.returncode != 0 and 'Operation not permitted' in result.stderr


def test_os_exclusions_resolve_symlink_targets(seatbelt):
    cfg, profile = seatbelt
    city = Path(cfg['city_dir'])
    target = city / 'public-looking/fixture'
    target.parent.mkdir()
    target.write_text('synthetic excluded fixture')
    link = city / 'assets/private'
    link.parent.mkdir()
    link.symlink_to(target.parent, target_is_directory=True)
    argv = common.shadow_sandbox_argv(cfg, ['/bin/cat', str(target)])
    result = subprocess.run(argv, capture_output=True, text=True, timeout=5)
    assert result.returncode != 0 and 'Operation not permitted' in result.stderr


def test_sandbox_off_is_explicit_in_ledger_and_status(harness, monkeypatch):
    import shadow_status
    cfg, _, _ = harness
    cfg['sandbox'] = False
    _, entry = run(harness)
    assert entry['sandbox'] == 'off'
    assert common.shadow_sandbox_argv(cfg, ['synthetic']) == ['synthetic']
    monkeypatch.setattr(shadow_status, 'discover', lambda cfg: (_ for _ in ()).throw(RuntimeError()))
    status = shadow_status.status(cfg, common.cst_day())
    assert status['sandbox'] == 'off' and 'WARNING' in status['sandbox_warning']


@pytest.mark.parametrize('bundled_config', [False, True])
def test_replay_under_real_os_sandbox_records_hooks_and_fork(harness, monkeypatch, bundled_config):
    if not Path('/usr/bin/sandbox-exec').is_file():
        pytest.skip('macOS sandbox-exec required for OS integration')
    cfg, source, rows = harness
    if bundled_config:
        cfg['config_path'] = str(common.PACK / 'assets/shadow-eval.toml')
    monkeypatch.setattr(watch, 'sandbox_argv', common.shadow_sandbox_argv)
    _, entry = run(harness)
    assert entry['status'] == 'completed', entry['shadow']['errors']
    assert entry['sandbox'] == 'on'
    assert [d['decision'] for d in entry['shadow']['decisions']] == ['allow', 'deny']
    assert entry['shadow']['source_integrity']['prefix_unchanged']


def test_explicit_sandbox_request_does_not_fall_back_when_missing(tmp_path, monkeypatch):
    cfg = {'sandbox': True}
    monkeypatch.setattr(Path, 'is_file', lambda self: False)
    with pytest.raises(RuntimeError, match='unavailable'):
        common.shadow_sandbox_argv(cfg, ['synthetic'])


def test_config_rejects_non_boolean_sandbox(tmp_path):
    config = tmp_path / 'invalid.toml'
    config.write_text('sandbox = "false"\n')
    with pytest.raises(ValueError, match='boolean'):
        common.load_config(config)
