"""The doctor must inspect the same explicit env file as adapter/run.sh."""
import os
from pathlib import Path
import subprocess

CHECK = Path(__file__).resolve().parents[1] / 'doctor/check-env.sh'
KEYS = ('SLACK_WORKSPACE_ID', 'SLACK_BOT_TOKEN', 'SLACK_SIGNING_SECRET', 'GC_CITY_NAME')


def run_check(**env):
    clean = {'PATH': os.environ['PATH']}
    clean.update(env)
    return subprocess.run([str(CHECK)], env=clean, text=True, capture_output=True)


def test_explicit_file_with_no_home(tmp_path):
    file = tmp_path / 'adapter-env'
    file.write_text(''.join(f'{name}=fixture-private-value\n' for name in KEYS))
    result = run_check(GC_SLACK_ADAPTER_ENV=str(file))
    assert result.returncode == 0, result.stdout + result.stderr
    assert 'fixture-private-value' not in result.stdout + result.stderr


def test_missing_explicit_file_rejects_ambient_credentials(tmp_path):
    result = run_check(GC_SLACK_ADAPTER_ENV=str(tmp_path / 'missing'),
                       **{name: 'fixture-private-value' for name in KEYS})
    assert result.returncode == 2
    assert 'GC_SLACK_ADAPTER_ENV' in result.stdout


def test_missing_home_reports_names_without_shell_error():
    result = run_check()
    assert result.returncode == 2
    assert all(name in result.stdout for name in KEYS)
    assert result.stderr == ''
    assert '/.config/' not in result.stdout
