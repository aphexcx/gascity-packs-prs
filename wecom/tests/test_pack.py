"""Pack discovery and doctor contracts, including failure cases."""
import hashlib
import http.server
import os
from pathlib import Path
import shutil
import socketserver
import subprocess
import tempfile
import threading
import tomllib

import pytest

PACK = Path(__file__).resolve().parents[1]


def doctor(name, **env):
    # Never inherit bot credentials or a live city's service paths.
    clean = {key: os.environ[key] for key in ('PATH', 'HOME') if key in os.environ}
    clean.update(env)
    return subprocess.run([str(PACK / 'doctor' / f'check-{name}.sh')],
                          env=clean, text=True, capture_output=True, timeout=10)


def test_service_and_doctor_entries_resolve():
    pack = tomllib.loads((PACK / 'pack.toml').read_text())
    service, = pack['service']
    assert service['kind'] == 'proxy_process'
    for command in service['process']['command']:
        assert os.access(PACK / command, os.X_OK)
    assert service['process']['health_path'] == '/healthz'
    entries = list((PACK / 'doctor').glob('*/doctor.toml'))
    assert {entry.parent.name for entry in entries} == {'runtime', 'dependencies', 'env', 'socket'}
    for entry in entries:
        config = tomllib.loads(entry.read_text())
        assert config['description']
        assert os.access(entry.parent / config['run'], os.X_OK)
    route, = tomllib.loads((PACK / 'city-fragment.toml').read_text())['extmsg']['default_route']
    assert route == {'provider': 'wecom', 'agent': 'mayor'}


def test_runtime_available():
    result = doctor('runtime')
    assert result.returncode == 0, result.stdout + result.stderr


def test_missing_node_is_actionable(tmp_path):
    result = doctor('runtime', PATH=str(tmp_path))
    assert result.returncode == 2
    assert 'Node.js 20' in result.stdout


def test_old_node_is_rejected(tmp_path):
    node = tmp_path / 'node'
    node.write_text('#!/bin/sh\nexit 1\n')
    node.chmod(0o755)
    result = doctor('runtime', PATH=str(tmp_path))
    assert result.returncode == 2
    assert '20 or newer' in result.stdout


@pytest.fixture
def env_file(tmp_path):
    file = tmp_path / 'env'
    file.write_text("WECOM_BOT_ID='fixture-bot'\nWECOM_BOT_SECRET='fixture-secret'\n"
                    'GC_CITY_NAME=fixture-city\nGC_API_BASE_URL=http://127.0.0.1:9443\n')
    return file


def test_credentials_report_names_only(env_file):
    result = doctor('env', GC_WECOM_ADAPTER_ENV=str(env_file))
    assert result.returncode == 0
    assert 'WECOM_BOT_SECRET' in result.stdout
    assert 'fixture-' not in result.stdout + result.stderr


def test_empty_file_value_overrides_ambient(env_file):
    with env_file.open('a') as output:
        output.write('WECOM_BOT_SECRET="" # deliberately empty\n')
    result = doctor('env', GC_WECOM_ADAPTER_ENV=str(env_file), WECOM_BOT_SECRET='ambient-secret')
    assert result.returncode == 2
    assert 'WECOM_BOT_SECRET' in result.stdout
    assert 'ambient-secret' not in result.stdout + result.stderr


def test_shell_expansion_is_not_executed(env_file, tmp_path):
    target = tmp_path / 'must-not-exist'
    with env_file.open('a') as output:
        output.write(f'WECOM_BOT_SECRET=$(touch {target})\n')
    result = doctor('env', GC_WECOM_ADAPTER_ENV=str(env_file))
    assert result.returncode == 1
    assert not target.exists()
    assert 'WECOM_BOT_SECRET' in result.stdout


def test_missing_explicit_file_never_falls_back(env_file):
    result = doctor('env', GC_WECOM_ADAPTER_ENV=str(env_file.parent / 'absent'),
                    GC_SERVICE_SECRETS_DIR=str(env_file.parent))
    assert result.returncode == 2


def test_city_scopes_credentials(env_file, tmp_path):
    result = doctor('env', GC_CITY_PATH=str(tmp_path))
    assert result.returncode == 2
    directory = tmp_path / '.gc/services/wecom/secrets'
    directory.mkdir(parents=True)
    shutil.copy(env_file, directory / 'env')
    assert doctor('env', GC_CITY_PATH=str(tmp_path)).returncode == 0


def test_dependency_marker_must_match_and_sdk_must_exist(tmp_path):
    adapter = tmp_path / 'adapter'
    modules = adapter / 'node_modules'
    modules.mkdir(parents=True)
    lock = adapter / 'pnpm-lock.yaml'
    lock.write_text('lockfileVersion: 9.0\n')
    (adapter / 'package.json').write_text('{}')
    marker = modules / '.gc-deps-ok'
    marker.write_text('stale')
    assert doctor('dependencies', GC_PACK_DIR=str(tmp_path)).returncode == 2
    marker.write_text(hashlib.sha256(lock.read_bytes()).hexdigest())
    assert doctor('dependencies', GC_PACK_DIR=str(tmp_path)).returncode == 2
    sdk = modules / '@wecom/aibot-node-sdk'
    sdk.mkdir(parents=True)
    (sdk / 'index.js').write_text('module.exports = {};')
    assert doctor('dependencies', GC_PACK_DIR=str(tmp_path)).returncode == 0
    lock.write_text('changed')
    assert doctor('dependencies', GC_PACK_DIR=str(tmp_path)).returncode == 2


def test_socket_absent_is_warning():
    assert doctor('socket').returncode == 1


@pytest.mark.parametrize('socket_path', ['relative.sock', '/tmp/' + 'x' * 110])
def test_invalid_socket_path_is_error(socket_path):
    assert doctor('socket', GC_SERVICE_SOCKET=socket_path).returncode == 2


def test_regular_file_is_not_a_socket(tmp_path):
    file = tmp_path / 'regular'
    file.touch()
    assert doctor('socket', GC_SERVICE_SOCKET=str(file)).returncode == 2


@pytest.mark.parametrize('status, expected', [(200, 0), (503, 2)])
def test_socket_health(status, expected):
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            assert self.path == '/healthz'
            self.send_response(status)
            self.end_headers()

        def log_message(self, *_):
            pass

    # macOS pytest roots can already exceed the Unix socket path limit.
    with tempfile.TemporaryDirectory(prefix='wecom-', dir='/tmp') as directory:
        address = str(Path(directory) / 'health.sock')
        with socketserver.UnixStreamServer(address, Handler) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                result = doctor('socket', GC_SERVICE_SOCKET=address)
                assert result.returncode == expected, result.stdout + result.stderr
            finally:
                server.shutdown()
                thread.join()
