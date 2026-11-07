"""Optional acknowledgement credentials are checked without sourcing secrets."""
import pytest

from test_doctor_env import KEYS, run_check


def test_ack_key_optional_and_readable(tmp_path):
    base = {name: 'fixture' for name in KEYS}
    assert run_check(**base).returncode == 0
    assert run_check(ACK_MODEL_API_KEY_FILE='', **base).returncode == 0
    key = tmp_path / 'model key'
    key.write_text('private-test-content')
    result = run_check(ACK_MODEL_API_KEY_FILE=str(key), **base)
    assert result.returncode == 0
    assert 'private-test-content' not in result.stdout + result.stderr


@pytest.mark.parametrize('kind', ['missing', 'directory', 'unreadable'])
def test_ack_key_invalid(tmp_path, kind):
    key = tmp_path / 'key'
    if kind == 'directory':
        key.mkdir()
    elif kind == 'unreadable':
        key.write_text('private-test-content')
        key.chmod(0)
    result = run_check(ACK_MODEL_API_KEY_FILE=str(key), **{name: 'fixture' for name in KEYS})
    assert result.returncode == 2
    assert 'ACK_MODEL_API_KEY_FILE' in result.stdout
    assert 'private-test-content' not in result.stdout + result.stderr


@pytest.mark.parametrize('quote', ['', "'", '"'])
def test_ack_key_in_env_file_is_not_executed(tmp_path, quote):
    key = tmp_path / 'model key'
    key.write_text('private-test-content')
    marker = tmp_path / 'must-not-exist'
    env_file = tmp_path / 'env'
    env_file.write_text(''.join(f'{name}=fixture\n' for name in KEYS)
                        + f'export ACK_MODEL_API_KEY_FILE={quote}{key}{quote}\n'
                        + f'touch "{marker}"\n')
    result = run_check(GC_SLACK_ADAPTER_ENV=str(env_file))
    assert result.returncode == 0, result.stdout + result.stderr
    assert not marker.exists()
    assert 'private-test-content' not in result.stdout + result.stderr
    key.unlink()
    assert run_check(GC_SLACK_ADAPTER_ENV=str(env_file)).returncode == 2
    # An explicitly empty ambient setting overrides the env-file path.
    assert run_check(GC_SLACK_ADAPTER_ENV=str(env_file), ACK_MODEL_API_KEY_FILE='').returncode == 0
