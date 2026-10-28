"""Apply the shipped watchdog patches and exercise only synthetic transcripts."""
import datetime as dt
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import time
from types import SimpleNamespace

import pytest


PACK = Path(__file__).resolve().parents[1]
FIXTURES = PACK / 'tests/fixtures'
PATCHES = PACK / 'assets/patches'


def apply_patch(target, patch, *, dry_run=False):
    result = subprocess.run(
        ['patch', '--batch', *(['--dry-run'] if dry_run else []), str(target), str(patch)],
        capture_output=True, text=True, check=False,
    )
    assert result.returncode == 0, result.stdout + result.stderr
    assert 'offset' not in result.stdout and 'fuzz' not in result.stdout


@pytest.fixture
def patched_path(tmp_path):
    target = tmp_path / 'mayor-model-watchdog'
    target.write_bytes((FIXTURES / 'mayor-model-watchdog-pristine.txt').read_bytes())
    apply_patch(target, PATCHES / 'mayor-model-watchdog-live-source.patch')
    return target


@pytest.fixture
def watchdog(patched_path, tmp_path, monkeypatch):
    namespace = runpy.run_path(str(patched_path))
    # Functions share their defining globals. Replace every host-facing path and
    # action before classification; executing the fixture must never contact gc,
    # Slack, mail, cswap, or the real settings/state files.
    globals_ = namespace['classify'].__globals__
    project = tmp_path / 'project'
    project.mkdir()

    def forbidden(*args, **kwargs):
        pytest.fail('watchdog attempted an external action')

    globals_.update(PROJECT_DIR=str(project), SETTINGS=str(tmp_path / 'settings.json'),
                    STATE=str(tmp_path / 'state.json'), LOG=str(tmp_path / 'watchdog.log'),
                    gc_mayor_session_key=lambda: 'gc-source',
                    slack_post=forbidden, mail_mayor=forbidden, cswap_seat=forbidden,
                    enforce_settings_flags=forbidden)
    monkeypatch.delenv('WATCHDOG_TRANSCRIPT', raising=False)
    return SimpleNamespace(**globals_)


def transcript(path, *, age=30, marker=False, model=True):
    stamp = dt.datetime.fromtimestamp(time.time() - age, dt.timezone.utc).isoformat()
    rows = []
    if marker:
        rows.append({'type': 'user', 'timestamp': stamp,
                     'message': {'content': 'You are the mayor of this Gas City workspace. Synthetic fixture.'}})
    rows.append({'type': 'assistant' if model else 'user', 'timestamp': stamp,
                 'message': {'content': f'Synthetic fixture ({age}).',
                             **({'model': 'claude-fable-synthetic'} if model else {})}})
    path.write_text(''.join(json.dumps(row) + '\n' for row in rows))
    return path


def test_fresh_override_honoured_without_marker(watchdog, tmp_path, monkeypatch):
    override = transcript(tmp_path / 'explicit.jsonl')
    transcript(Path(watchdog.PROJECT_DIR) / 'gc-source.jsonl')
    monkeypatch.setenv('WATCHDOG_TRANSCRIPT', str(override))
    reads = []
    getenv = os.environ.get

    def counted_get(key, *args):
        if key == 'WATCHDOG_TRANSCRIPT':
            reads.append(key)
        return getenv(key, *args)

    monkeypatch.setattr(os.environ, 'get', counted_get)
    result = watchdog.classify()
    assert result['state'] == 'OK'
    assert result['source_path'] == str(override)
    assert result['selector'] == 'explicit' and result['override_used'] is True
    assert result['newest_row_age_seconds'] == pytest.approx(30, abs=2)
    assert reads == ['WATCHDOG_TRANSCRIPT']


def test_stale_override_returns_stale_source(watchdog, tmp_path, monkeypatch):
    override = transcript(tmp_path / 'stale.jsonl', age=7 * 3600)
    transcript(Path(watchdog.PROJECT_DIR) / 'gc-source.jsonl')
    monkeypatch.setenv('WATCHDOG_TRANSCRIPT', str(override))
    result = watchdog.classify()
    assert result['state'] == 'UNKNOWN'
    assert result['reason'] == f'stale_source {override}'
    assert result['source_path'] == str(override) and result['override_used'] is True
    assert result['newest_row_age_seconds'] >= 7 * 3600


def test_missing_override_returns_override_missing(watchdog, tmp_path, monkeypatch):
    missing = tmp_path / 'missing.jsonl'
    transcript(Path(watchdog.PROJECT_DIR) / 'gc-source.jsonl')
    monkeypatch.setenv('WATCHDOG_TRANSCRIPT', str(missing))
    result = watchdog.classify()
    assert result['state'] == 'UNKNOWN'
    assert result['reason'] == f'override_missing {missing}'
    assert result['source_path'] == str(missing) and result['override_used'] is True
    assert result['newest_row_age_seconds'] is None


def test_unset_preserves_gc_then_marker_then_stale_rule(watchdog, monkeypatch):
    monkeypatch.setattr(time, 'time', lambda: 1800000000)
    project = Path(watchdog.PROJECT_DIR)
    gc_source = transcript(project / 'gc-source.jsonl', age=5 * 3600)
    fallback = transcript(project / 'fallback.jsonl', marker=True)
    result = watchdog.classify()
    assert result['source_path'] == str(gc_source) and result['selector'] == 'gc-session'
    assert result['override_used'] is False
    monkeypatch.setattr(time, 'time', lambda: 1800000000 + 2 * 3600)
    result = watchdog.classify()
    assert result['source_path'] == str(fallback)
    assert result['selector'] == 'paragraph-start-full-scan'
    assert result['override_used'] is False
    monkeypatch.setattr(time, 'time', lambda: 1800000000 + 8 * 3600)
    result = watchdog.classify()
    assert result['state'] == 'UNKNOWN'
    assert result['reason'] == f'stale_source {fallback}'


def test_override_is_reread_and_selection_reset_each_tick(watchdog, tmp_path, monkeypatch):
    first = transcript(tmp_path / 'first.jsonl')
    second = transcript(tmp_path / 'second.jsonl')
    for path in (first, second, tmp_path / 'missing.jsonl'):
        monkeypatch.setenv('WATCHDOG_TRANSCRIPT', str(path))
        assert watchdog.classify()['source_path'] == str(path)
    monkeypatch.delenv('WATCHDOG_TRANSCRIPT')
    result = watchdog.classify()
    assert result['reason'] == 'no mayor transcript found'
    assert result['source_path'] is None and result['override_used'] is False
    assert result['newest_row_age_seconds'] is None


@pytest.mark.parametrize('age,state', [(21599, 'OK'), (21600, 'UNKNOWN'), (21601, 'UNKNOWN')])
def test_six_hour_freshness_boundary(watchdog, tmp_path, monkeypatch, age, state):
    monkeypatch.setattr(time, 'time', lambda: 1800000000)
    source = transcript(tmp_path / 'boundary.jsonl', age=age)
    monkeypatch.setenv('WATCHDOG_TRANSCRIPT', str(source))
    assert watchdog.classify()['state'] == state


@pytest.mark.parametrize('kind', ['fresh', 'stale', 'missing', 'no-model'])
def test_status_cli_reports_source_row_age_and_override(patched_path, tmp_path, kind):
    source = tmp_path / f'{kind}.jsonl'
    if kind != 'missing':
        transcript(source, age=7 * 3600 if kind == 'stale' else 30, model=kind != 'no-model')
    home = tmp_path / 'isolated-home'
    home.mkdir()
    result = subprocess.run(
        [sys.executable, '-I', str(patched_path), '--status'],
        env={'HOME': str(home), 'PATH': os.defpath, 'WATCHDOG_TRANSCRIPT': str(source)},
        capture_output=True, text=True, timeout=10, check=True,
    )
    status = json.loads(result.stdout)
    assert status['source_path'] == str(source) and status['override_used'] is True
    assert status['state'] == ('OK' if kind == 'fresh' else 'UNKNOWN')
    if kind == 'missing':
        assert status['newest_row_age_seconds'] is None
    else:
        assert status['newest_row_age_seconds'] == pytest.approx(7 * 3600 if kind == 'stale' else 30, abs=2)
    assert not result.stderr
    assert list(home.iterdir()) == []


def test_both_patch_routes_apply_exactly_and_produce_identical_v2(tmp_path):
    direct = tmp_path / 'direct'
    upgrade = tmp_path / 'upgrade'
    pristine = (FIXTURES / 'mayor-model-watchdog-pristine.txt').read_bytes()
    direct.write_bytes(pristine)
    upgrade.write_bytes(pristine)
    full_patch = PATCHES / 'mayor-model-watchdog-live-source.patch'
    incremental = PATCHES / 'mayor-model-watchdog-live-source-v1-to-v2.patch'
    apply_patch(direct, full_patch, dry_run=True)
    assert direct.read_bytes() == pristine
    apply_patch(direct, full_patch)
    apply_patch(upgrade, FIXTURES / 'mayor-model-watchdog-v1.patch')
    v1 = upgrade.read_bytes()
    apply_patch(upgrade, incremental, dry_run=True)
    assert upgrade.read_bytes() == v1
    apply_patch(upgrade, incremental)
    assert direct.read_bytes() == upgrade.read_bytes()


def test_relative_override_fails_closed(watchdog, monkeypatch):
    monkeypatch.setenv('WATCHDOG_TRANSCRIPT', 'relative.jsonl')
    result = watchdog.classify()
    assert result['state'] == 'UNKNOWN'
    assert result['reason'] == 'override_not_absolute relative.jsonl'
