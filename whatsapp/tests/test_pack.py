"""Pack discovery, the doctor rows' contracts (each failing closed with its
documented message in a clean env), and the verbs against a fake adapter."""
import hashlib
import http.server
import json
import os
from pathlib import Path
import re
import shutil
import socketserver
import subprocess
import tempfile
import threading
import time
import tomllib

import pytest

PACK = Path(__file__).resolve().parents[1]
ROWS = {'env', 'dependencies', 'runtime', 'gateway', 'deadletters', 'held', 'restore', 'probes', 'keeper'}


def clean_env(**env):
    # Never inherit a live city's credential or service paths.
    clean = {key: os.environ[key] for key in ('PATH', 'HOME') if key in os.environ}
    clean.update(env)
    return clean


def doctor(name, **env):
    return subprocess.run([str(PACK / 'doctor' / f'check-{name}.sh')],
                          env=clean_env(**env), text=True, capture_output=True, timeout=20)


def verb(script, *args, **env):
    return subprocess.run([str(PACK / 'commands' / script), *args],
                          env=clean_env(**env), text=True, capture_output=True, timeout=20)


# --- discovery -----------------------------------------------------------------

def test_pack_service_and_fragment():
    pack = tomllib.loads((PACK / 'pack.toml').read_text())
    assert pack['pack'] == {'name': 'whatsapp', 'version': '0.0.1', 'schema': 2}
    service, = pack['service']
    assert service['name'] == 'whatsapp'
    assert service['kind'] == 'proxy_process'
    assert service['publication'] == {'visibility': 'private'}
    for command in service['process']['command']:
        assert os.access(PACK / command, os.X_OK)
    assert service['process']['health_path'] == '/healthz'
    route, = tomllib.loads((PACK / 'city-fragment.toml').read_text())['extmsg']['default_route']
    assert route == {'provider': 'whatsapp', 'agent': 'mayor'}


def test_doctor_entries_resolve():
    entries = list((PACK / 'doctor').glob('*/doctor.toml'))
    assert {entry.parent.name for entry in entries} == ROWS
    for entry in entries:
        config = tomllib.loads(entry.read_text())
        assert config['description']
        assert os.access(entry.parent / config['run'], os.X_OK)


def test_command_entries_resolve_to_nested_words():
    found = {}
    for manifest in (PACK / 'commands').rglob('command.toml'):
        config = tomllib.loads(manifest.read_text())
        target = (manifest.parent / config['run']).resolve()
        assert target.is_relative_to(PACK.resolve()), manifest
        assert os.access(target, os.X_OK), target
        assert config['description']
        assert (manifest.parent / 'help.md').is_file(), manifest
        found[' '.join(manifest.parent.relative_to(PACK / 'commands').parts)] = target.name
    assert found == {
        'publish': 'publish.sh', 'invite': 'invite.sh', 'consent': 'consent.sh', 'hours': 'hours.sh',
        'enroll': 'enroll.sh', 'status': 'status.sh',
        'inbound resolve': 'resolve.sh', 'inbound clear-gap': 'clear-gap.sh',
    }


def test_no_meta_secret_name_in_the_pack():
    # The box never holds a Meta secret: no META_* name, no access token name,
    # anywhere in the box side of the pack (whatsapp/gateway/ is the key holder).
    offenders = []
    for path in PACK.rglob('*'):
        if not path.is_file() or 'gateway' in path.relative_to(PACK).parts[:1] or 'node_modules' in path.parts:
            continue
        if '__pycache__' in path.parts or path.suffix in {'.pyc', '.png', '.jpg', '.ogg'}:
            continue
        text = path.read_text(errors='replace')
        if re.search(r'META_|ACCESS_TOKEN', text) and path.name != 'test_pack.py':
            offenders.append(str(path.relative_to(PACK)))
    assert offenders == []


def test_run_sh_deps_only_needs_no_env_file(tmp_path):
    result = subprocess.run([str(PACK / 'adapter' / 'run.sh'), '--deps-only'],
                            env=clean_env(HOME=str(tmp_path)), text=True, capture_output=True, timeout=20)
    assert result.returncode == 0, result.stderr
    assert 'dependencies ready' in result.stdout


def test_run_sh_refuses_without_the_env_file(tmp_path):
    result = subprocess.run([str(PACK / 'adapter' / 'run.sh')],
                            env=clean_env(HOME=str(tmp_path), GC_SERVICE_SECRETS_DIR=str(tmp_path / 'secrets')),
                            text=True, capture_output=True, timeout=20)
    assert result.returncode == 1
    assert 'env file not found' in result.stderr
    assert 'gc whatsapp enroll' in result.stderr


# --- doctor rows ---------------------------------------------------------------

@pytest.fixture
def env_file(tmp_path):
    file = tmp_path / 'env'
    file.write_text("WHATSAPP_GATEWAY_URL=https://gw.invalid\nWHATSAPP_GATEWAY_CREDENTIAL='fixture-credential'\n"
                    'GC_CITY_NAME=fixture-city\nGC_API_BASE_URL=http://127.0.0.1:9443\n')
    return file


def test_env_missing_file_fails_closed():
    result = doctor('env')
    assert result.returncode == 2
    assert 'env file missing' in result.stdout


def test_env_reports_names_only(env_file):
    result = doctor('env', GC_WHATSAPP_ADAPTER_ENV=str(env_file))
    assert result.returncode == 0
    assert 'WHATSAPP_GATEWAY_CREDENTIAL' in result.stdout
    assert 'fixture-' not in result.stdout + result.stderr


def test_env_missing_credential_reads_not_enrolled(tmp_path):
    file = tmp_path / 'env'
    file.write_text('WHATSAPP_GATEWAY_URL=https://gw.invalid\nGC_CITY_NAME=c\nGC_API_BASE_URL=http://x\n')
    result = doctor('env', GC_WHATSAPP_ADAPTER_ENV=str(file))
    assert result.returncode == 2
    assert 'gc whatsapp enroll' in result.stdout


def test_env_shell_expansion_is_not_executed(env_file, tmp_path):
    target = tmp_path / 'must-not-exist'
    with env_file.open('a') as out:
        out.write(f'WHATSAPP_GATEWAY_CREDENTIAL=$(touch {target})\n')
    result = doctor('env', GC_WHATSAPP_ADAPTER_ENV=str(env_file))
    assert result.returncode == 1
    assert not target.exists()


def test_env_city_scopes_the_file(env_file, tmp_path):
    assert doctor('env', GC_CITY_PATH=str(tmp_path)).returncode == 2
    secrets = tmp_path / '.gc/services/whatsapp/secrets'
    secrets.mkdir(parents=True)
    shutil.copy(env_file, secrets / 'env')
    assert doctor('env', GC_CITY_PATH=str(tmp_path)).returncode == 0


def test_dependencies_green_and_missing_node(tmp_path):
    assert doctor('dependencies').returncode == 0
    result = doctor('dependencies', PATH=str(tmp_path))
    assert result.returncode == 2
    assert 'Node.js 20' in result.stdout


def serve_unix(handler_status, body=b'{"ok":true,"gateway_auth":"ok"}'):
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(handler_status)
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_):
            pass
    directory = tempfile.mkdtemp(prefix='wa-', dir='/tmp')
    address = str(Path(directory) / 'h.sock')
    server = socketserver.UnixStreamServer(address, Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, address, directory


def test_runtime_unset_socket_is_warning():
    assert doctor('runtime').returncode == 1


@pytest.mark.parametrize('status, body, expected', [
    (200, b'{"ok":true,"gateway_auth":"ok"}', 0),
    (503, b'{}', 2),
    (200, b'{"ok":true,"gateway_auth":"revoked or not enrolled"}', 2),
])
def test_runtime_health(status, body, expected):
    server, address, directory = serve_unix(status, body)
    try:
        result = doctor('runtime', GC_SERVICE_SOCKET=address)
        assert result.returncode == expected, result.stdout
    finally:
        server.shutdown()
        server.server_close()
        shutil.rmtree(directory, ignore_errors=True)


def serve_gateway(inbox_status):
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == '/healthz':
                status = 200
            else:
                status = inbox_status if self.headers.get('Authorization') == 'Bearer fixture-credential' else 401
            self.send_response(status)
            self.end_headers()
            self.wfile.write(b'{}')

        def log_message(self, *_):
            pass
    server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


@pytest.mark.parametrize('inbox_status, expected, words', [(200, 0, 'accepts'), (401, 2, 'not enrolled')])
def test_gateway_row(tmp_path, inbox_status, expected, words):
    server = serve_gateway(inbox_status)
    try:
        file = tmp_path / 'env'
        file.write_text(f"WHATSAPP_GATEWAY_URL=http://127.0.0.1:{server.server_address[1]}\nWHATSAPP_GATEWAY_CREDENTIAL='fixture-credential'\n")
        result = doctor('gateway', GC_WHATSAPP_ADAPTER_ENV=str(file))
        assert result.returncode == expected, result.stdout
        assert words in result.stdout
        assert 'fixture-credential' not in result.stdout
    finally:
        server.shutdown()


def test_gateway_row_fails_closed():
    assert doctor('gateway').returncode == 2


def write_journal(city, lines):
    state = city / '.gc' / 'whatsapp'
    state.mkdir(parents=True, exist_ok=True)
    (state / 'inbound_journal.jsonl').write_text(''.join(json.dumps(line) + '\n' for line in lines))
    return state


NOW_MS = int(time.time() * 1000)
RECEIVED = {'t': 'received', 'rid': 'whatsapp:dynasty:wamid.H', 'delivery_id': 3, 'kind': 'message', 'wa_id': '5511900001234', 'envelope': {}}


@pytest.mark.parametrize('row', ['held', 'restore', 'probes', 'deadletters', 'keeper'])
def test_state_rows_fail_closed_without_a_city(row):
    result = doctor(row)
    assert result.returncode == 1
    assert 'GC_CITY_PATH is unset' in result.stdout


def test_held_row(tmp_path):
    write_journal(tmp_path, [RECEIVED])
    assert doctor('held', GC_CITY_PATH=str(tmp_path)).returncode == 0
    write_journal(tmp_path, [RECEIVED, {'t': 'held_terminal', 'rid': RECEIVED['rid'], 'receipt_id': 'r1', 'at': NOW_MS - 600000}])
    result = doctor('held', GC_CITY_PATH=str(tmp_path))
    assert result.returncode == 1
    assert '1 terminal hold(s)' in result.stdout
    assert '...1234' in result.stdout and 'gc whatsapp inbound resolve' in result.stdout


def test_held_row_reads_an_accepted_terminal_line_as_held(tmp_path):
    write_journal(tmp_path, [RECEIVED, {'t': 'accepted', 'rid': RECEIVED['rid'], 'verdict': 'pending', 'terminal': True, 'at': NOW_MS}])
    assert doctor('held', GC_CITY_PATH=str(tmp_path)).returncode == 1


def test_restore_row(tmp_path):
    write_journal(tmp_path, [{'t': 'watermark', 'conversation_id': '5511900001234', 'sequence': 9, 'provider_message_id': 'w', 'accepted_at': NOW_MS}])
    assert doctor('restore', GC_CITY_PATH=str(tmp_path)).returncode == 0
    write_journal(tmp_path, [{'t': 'restore_gap', 'conversation_id': '5511900001234', 'watermark_sequence': 9,
                              'watermark_accepted_at': NOW_MS, 'head_sequence': 4, 'head_created_at': '2026-11-01T00:00:00Z', 'found_at': NOW_MS}])
    result = doctor('restore', GC_CITY_PATH=str(tmp_path))
    assert result.returncode == 1
    assert '...1234' in result.stdout and 'gc whatsapp inbound clear-gap' in result.stdout
    write_journal(tmp_path, [{'t': 'restore_gap', 'conversation_id': '5511900001234', 'watermark_sequence': 9, 'head_sequence': 4, 'found_at': NOW_MS},
                             {'t': 'gap_cleared', 'conversation_id': '5511900001234', 'at': NOW_MS}])
    assert doctor('restore', GC_CITY_PATH=str(tmp_path)).returncode == 0


# The codex gate r1's first finding on PR 74: a detected rollback is written
# at once with the head null (unknown); a head read that fails leaves it so.
GAP_HEAD_UNKNOWN = {'t': 'restore_gap', 'conversation_id': '5511900001234', 'watermark_sequence': 9, 'watermark_accepted_at': NOW_MS,
                    'head_sequence': None, 'head_created_at': None, 'found_at': NOW_MS, 'found_by': 'schedule'}


def test_restore_row_reads_a_head_it_never_got_as_unknown(tmp_path):
    write_journal(tmp_path, [GAP_HEAD_UNKNOWN])
    result = doctor('restore', GC_CITY_PATH=str(tmp_path))
    assert result.returncode == 1
    assert '(sequence unknown)' in result.stdout and 'null' not in result.stdout


def test_probes_row_counts_snapshots_of_the_last_seven_days(tmp_path):
    old = NOW_MS - 8 * 24 * 3600 * 1000
    write_journal(tmp_path, [{'t': 'probe', 'rid': 'r', 'session_id': 's-mayor', 'shape': 'pane', 'found': False, 'at': old},
                             {'t': 'probe', 'rid': 'r', 'session_id': 's-mayor', 'shape': 'log', 'found': False, 'at': NOW_MS}])
    assert doctor('probes', GC_CITY_PATH=str(tmp_path)).returncode == 0
    write_journal(tmp_path, [{'t': 'probe', 'rid': 'r', 'session_id': 's-mayor', 'shape': 'pane', 'found': False, 'at': NOW_MS}])
    result = doctor('probes', GC_CITY_PATH=str(tmp_path))
    assert result.returncode == 1
    assert '1 paste probe(s)' in result.stdout and 'marked path' in result.stdout


def test_deadletters_row(tmp_path):
    assert doctor('deadletters', GC_CITY_PATH=str(tmp_path)).returncode == 0
    dead = tmp_path / '.gc' / 'whatsapp' / 'inbound_dead_letter'
    dead.mkdir(parents=True)
    (dead / '5511900001234.jsonl').write_text(json.dumps({'rid': 'a', 'reason': 'redelivery_no_route'}) + '\n'
                                             + json.dumps({'rid': 'b', 'reason': 'pending_deadline'}) + '\n')
    result = doctor('deadletters', GC_CITY_PATH=str(tmp_path))
    assert result.returncode == 1
    assert '2 dead letter(s)' in result.stdout
    assert 'either already delivered it or nobody is routed' in result.stdout


def iso_ago(seconds):
    return time.strftime('%Y-%m-%dT%H:%M:%S.000Z', time.gmtime(time.time() - seconds))


def write_keeper(city, consent=True, lapses=0, **top):
    # The shape keeper.js writes: {version, conversations, invites, window_read_ok_at, window_read_error, window_read_error_at}.
    state = city / '.gc' / 'whatsapp'
    state.mkdir(parents=True, exist_ok=True)
    conversation = {'consent': consent, 'lapses': [{'close_at': '2026-11-04T09:28:00.000Z', 'at': iso_ago(60)}] * lapses,
                    'undelivered': [], 'refused_nudges': []}
    (state / 'keeper.json').write_text(json.dumps({'version': 1, 'conversations': {'5511900001234': conversation}, 'invites': {},
                                                   'window_read_ok_at': None, 'window_read_error': None, 'window_read_error_at': None, **top}))


def test_keeper_row(tmp_path):
    assert doctor('keeper', GC_CITY_PATH=str(tmp_path)).returncode == 0
    write_keeper(tmp_path, lapses=1, window_read_ok_at=iso_ago(60))
    result = doctor('keeper', GC_CITY_PATH=str(tmp_path))
    assert result.returncode == 1, result.stdout
    assert '...1234: 1 lapse(s)' in result.stdout


def test_keeper_row_is_red_when_the_window_clock_is_stale_while_the_keeper_is_on(tmp_path):
    # Never read OK, the keeper ON: red, the 7-day rows carried along.
    write_keeper(tmp_path, lapses=1)
    result = doctor('keeper', GC_CITY_PATH=str(tmp_path))
    assert result.returncode == 2, result.stdout
    assert 'never read OK' in result.stdout and '1 conversation(s) have the keeper ON' in result.stdout
    assert '...1234: 1 lapse(s)' in result.stdout
    # The last good read 10 minutes ago, an error since: red, naming the error.
    write_keeper(tmp_path, window_read_ok_at=iso_ago(600), window_read_error='HTTP 404 from https://gw.invalid/v1/window',
                 window_read_error_at=iso_ago(30))
    result = doctor('keeper', GC_CITY_PATH=str(tmp_path))
    assert result.returncode == 2, result.stdout
    assert '(10 min ago)' in result.stdout and 'HTTP 404 from https://gw.invalid/v1/window' in result.stdout
    assert '5511900001234' not in result.stdout
    # Fresh (4 minutes): green.
    write_keeper(tmp_path, window_read_ok_at=iso_ago(240))
    assert doctor('keeper', GC_CITY_PATH=str(tmp_path)).returncode == 0
    # Stale, but no conversation has the keeper ON: nothing depends on the clock.
    write_keeper(tmp_path, consent=False, window_read_ok_at=iso_ago(3600))
    assert doctor('keeper', GC_CITY_PATH=str(tmp_path)).returncode == 0


# --- the verbs against a fake adapter --------------------------------------------

class FakeAdapter:
    def __init__(self, answers):
        self.answers = answers
        self.requests = []
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def handle_one(self):
                length = int(self.headers.get('Content-Length') or 0)
                body = json.loads(self.rfile.read(length) or b'null') if length else None
                outer.requests.append((self.command, self.path, body))
                status, payload = outer.answers.get(f'{self.command} {self.path}', (404, {'error': 'no route'}))
                data = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.end_headers()
                self.wfile.write(data)

            do_GET = handle_one
            do_POST = handle_one

            def log_message(self, *_):
                pass

        self.server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = f'http://127.0.0.1:{self.server.server_address[1]}'

    def close(self):
        self.server.shutdown()


@pytest.fixture
def fake():
    servers = []

    def make(answers):
        server = FakeAdapter(answers)
        servers.append(server)
        return server
    yield make
    for server in servers:
        server.close()


def test_publish_sends_the_extmsg_shape_with_cli_origin_and_session(fake, tmp_path):
    adapter = fake({'POST /publish': (200, {'message_id': 'wamid.out.1', 'delivered': True})})
    text = tmp_path / 'reply.txt'
    text.write_text("it's 'quoted' and \"double\"\nsecond line")
    result = verb('publish.sh', '--to', '5511900001234', '--text-file', str(text),
                  WHATSAPP_ADAPTER_URL=adapter.url, GC_CITY_NAME='fixture-city', GC_SESSION_ID='s-mayor')
    assert result.returncode == 0, result.stderr
    method, path, body = adapter.requests[0]
    assert (method, path) == ('POST', '/publish')
    assert body['conversation'] == {'scope_id': 'fixture-city', 'provider': 'whatsapp', 'account_id': 'dynasty',
                                    'conversation_id': '5511900001234', 'kind': 'dm'}
    assert body['text'] == text.read_text()
    assert body['session_id'] == 's-mayor'
    assert body['metadata'] == {'origin': 'cli'}
    assert body['idempotency_key'].startswith('whatsapp-cli-')


def test_publish_window_closed_prints_the_hint(fake):
    adapter = fake({'POST /publish': (200, {'delivered': False, 'failure_kind': 'permanent',
                                            'metadata': {'reason': 'window_closed', 'hint': 'use gc whatsapp publish --update'}})})
    result = verb('publish.sh', '--to', '5511900001234', '--text', 'hi', WHATSAPP_ADAPTER_URL=adapter.url, GC_CITY_NAME='c')
    assert result.returncode == 1
    assert 'window_closed' in result.stderr and 'gc whatsapp publish --update' in result.stderr


def test_publish_update_and_usage_errors(fake):
    adapter = fake({'POST /publish-update': (200, {'message_id': 'wamid.out.2', 'delivered': True})})
    result = verb('publish.sh', '--update', '--to', '5511900001234', '--text', 'shipment cleared',
                  WHATSAPP_ADAPTER_URL=adapter.url, GC_CITY_NAME='c')
    assert result.returncode == 0, result.stderr
    assert adapter.requests[0][1] == '/publish-update'
    assert adapter.requests[0][2]['text'] == 'shipment cleared'
    assert verb('publish.sh', '--text', 'x').returncode == 2
    assert verb('publish.sh', '--to', '1', '--text', '').returncode == 2


def test_resolve_prints_last_four_and_receipt_never_the_text(fake):
    rid = 'whatsapp:dynasty:wamid.H'
    ref = 'm-' + hashlib.sha256(rid.encode()).hexdigest()[:10]
    adapter = fake({
        'GET /status': (200, {'inbound': {'held': [{'ref': ref, 'wa_id_last4': '1234', 'receipt_id': 'rcpt-9', 'held_at': NOW_MS}]}}),
        'POST /inbound/resolve': (200, {'ok': True}),
    })
    result = verb('resolve.sh', 'wamid.H', '--dead', WHATSAPP_ADAPTER_URL=adapter.url)
    assert result.returncode == 0, result.stderr
    assert '...1234' in result.stdout and 'rcpt-9' in result.stdout
    assert adapter.requests[-1][2] == {'rid': ref, 'as': 'dead'}
    by_ref = verb('resolve.sh', ref, '--delivered', WHATSAPP_ADAPTER_URL=adapter.url)
    assert by_ref.returncode == 0, by_ref.stderr
    assert 'wamid.H' not in by_ref.stdout
    assert verb('resolve.sh', 'wamid.H', WHATSAPP_ADAPTER_URL=adapter.url).returncode == 2


def test_resolve_refuses_a_rid_that_is_not_held(fake):
    adapter = fake({'GET /status': (200, {'inbound': {'held': []}})})
    result = verb('resolve.sh', 'wamid.X', '--delivered', WHATSAPP_ADAPTER_URL=adapter.url)
    assert result.returncode == 1
    assert 'not a terminal hold' in result.stderr
    assert all(path != '/inbound/resolve' for _, path, _ in adapter.requests)


def test_clear_gap_prints_the_gap_and_the_head(fake):
    adapter = fake({'POST /inbound/clear-gap': (200, {'ok': True, 'gap': {'conversation_id': '5511900001234', 'watermark_sequence': 9,
                                                                             'watermark_accepted_at': 1, 'head_sequence': 4},
                                                       'head': {'sequence': 4, 'created_at': '2026-11-01T00:00:00Z'}})})
    result = verb('clear-gap.sh', '5511900001234', WHATSAPP_ADAPTER_URL=adapter.url)
    assert result.returncode == 0, result.stderr
    assert 'sequence 9' in result.stdout and 'rebased to the store head now: sequence 4' in result.stdout


def test_clear_gap_and_status_read_a_head_never_got_as_unknown(fake):
    gap = {key: value for key, value in GAP_HEAD_UNKNOWN.items() if key != 't'}
    adapter = fake({'POST /inbound/clear-gap': (200, {'ok': True, 'gap': gap, 'head': {'sequence': 4, 'created_at': '2026-11-01T00:00:00Z'}})})
    result = verb('clear-gap.sh', '5511900001234', WHATSAPP_ADAPTER_URL=adapter.url)
    assert result.returncode == 0, result.stderr
    assert 'gc store head:     sequence unknown' in result.stdout and 'null' not in result.stdout
    status = fake({'GET /status': (200, {
        'health': {'gateway_auth': 'ok'},
        'inbound': {'last_poll_at': NOW_MS, 'last_poll_error': '', 'entries': {}, 'acks_owed': 0, 'dead_letters': 0, 'held': [],
                    'gaps': [{'conversation_id': '...1234', 'watermark_sequence': 9, 'head_sequence': None}]},
        'keeper': {'conversations': {}, 'window_read': {'ok_at': None, 'error': None, 'error_at': None, 'failures': 0}},
        'ledger': {'conversations': {}},
    })})
    result = verb('status.sh', WHATSAPP_ADAPTER_URL=status.url)
    assert result.returncode == 0, result.stderr
    assert 'journal watermark 9 ahead of the gc store head unknown' in result.stdout and 'null' not in result.stdout


def test_invite_consent_hours(fake):
    adapter = fake({'POST /invite': (200, {'code': '482913', 'expires_at': '2026-11-05T18:00:00Z'}),
                    'POST /consent': (200, {'ok': True, 'consent': False, 'wa_id_last4': '1234'}),
                    'POST /hours': (200, {'ok': True, 'wa_id_last4': '1234'})})
    result = verb('invite.sh', '--proactive-consent', '--hours', 'Mon-Fri 09:00-18:00', '--tz', 'America/Sao_Paulo',
                  WHATSAPP_ADAPTER_URL=adapter.url)
    assert result.returncode == 0, result.stderr
    assert '482913' in result.stdout and 'Save the Dynasty WhatsApp number' in result.stdout
    assert adapter.requests[0][2] == {'proactive_consent': True, 'hours': 'Mon-Fri 09:00-18:00', 'tz': 'America/Sao_Paulo'}
    assert verb('invite.sh', '--hours', 'Mon-Fri 09:00-18:00', WHATSAPP_ADAPTER_URL=adapter.url).returncode == 2
    # The verb sends the number as typed and prints the conversation the adapter matched.
    off = verb('consent.sh', '+55 11 90000-9999', '--off', WHATSAPP_ADAPTER_URL=adapter.url)
    assert off.returncode == 0, off.stderr
    assert adapter.requests[-1][2] == {'wa_id': '+55 11 90000-9999', 'on': False}
    assert 'OFF for ...1234' in off.stdout and '9999' not in off.stdout
    hours = verb('hours.sh', '5511900001234', '--hours', 'Mon-Sat 18:00-02:00', '--tz', 'America/Chicago',
                 WHATSAPP_ADAPTER_URL=adapter.url)
    assert hours.returncode == 0, hours.stderr
    assert 'Hours recorded for ...1234: Mon-Sat 18:00-02:00 (America/Chicago).' in hours.stdout


def test_hours_clear(fake):
    adapter = fake({'POST /hours': (200, {'ok': True, 'wa_id_last4': '1234', 'hours': None})})
    cleared = verb('hours.sh', '5511900001234', '--clear', WHATSAPP_ADAPTER_URL=adapter.url)
    assert cleared.returncode == 0, cleared.stderr
    assert adapter.requests[-1][2] == {'wa_id': '5511900001234', 'hours': ''}
    assert 'Hours cleared for ...1234 (the recorded zone kept).' in cleared.stdout
    with_zone = verb('hours.sh', '5511900001234', '--clear', '--tz', 'America/Sao_Paulo', WHATSAPP_ADAPTER_URL=adapter.url)
    assert with_zone.returncode == 0, with_zone.stderr
    assert adapter.requests[-1][2] == {'wa_id': '5511900001234', 'hours': '', 'tz': 'America/Sao_Paulo'}
    assert verb('hours.sh', '5511900001234', '--clear', '--hours', 'Mon 09:00-10:00', WHATSAPP_ADAPTER_URL=adapter.url).returncode == 2
    assert verb('hours.sh', '5511900001234', WHATSAPP_ADAPTER_URL=adapter.url).returncode == 2


def test_consent_and_hours_refuse_an_unknown_phone_and_never_confirm_without_the_match(fake):
    adapter = fake({'POST /consent': (404, {'error': 'no conversation ending 0009 is known to the window-keeper'}),
                    'POST /hours': (200, {'ok': True})})
    refused = verb('consent.sh', '5511900000009', '--off', WHATSAPP_ADAPTER_URL=adapter.url)
    assert refused.returncode == 1
    assert 'HTTP 404' in refused.stderr and 'ending 0009' in refused.stderr
    assert 'OFF for' not in refused.stdout
    # An answer without the matched conversation confirms nothing.
    unmatched = verb('hours.sh', '5511900000009', '--clear', WHATSAPP_ADAPTER_URL=adapter.url)
    assert unmatched.returncode == 1
    assert 'nothing confirmed' in unmatched.stderr and 'cleared' not in unmatched.stdout


def test_status_renders_holds_gaps_and_windows_without_full_numbers(fake):
    close_ms = 1793900000000
    adapter = fake({'GET /status': (200, {
        'health': {'gateway_auth': 'ok'},
        'inbound': {'last_poll_at': close_ms, 'last_poll_error': '', 'entries': {'accepted:pending': 1}, 'acks_owed': 0,
                    'dead_letters': 2, 'held': [{'ref': 'm-0123456789', 'wa_id_last4': '1234', 'receipt_id': 'rcpt-9', 'held_at': close_ms}],
                    'gaps': [{'conversation_id': '...1234', 'watermark_sequence': 9, 'head_sequence': 4}]},
        'keeper': {'conversations': {'5511900001234': {'consent': True, 'window_closes_at': '2026-11-05T15:33:20.000Z',
                                                      'update_day': '2026-11-04', 'marks_fired': [60, 30], 'lapses_7d': 1}},
                   'window_read': {'ok_at': '2026-11-05T15:20:00.000Z', 'error': 'HTTP 503 from https://gw.invalid/v1/window',
                                   'error_at': '2026-11-05T15:21:00.000Z', 'failures': 3}},
        'ledger': {'conversations': {'5511900001234': {'window_closes_at': close_ms - 60000}}},
    })})
    result = verb('status.sh', WHATSAPP_ADAPTER_URL=adapter.url)
    assert result.returncode == 0, result.stderr
    out = result.stdout
    assert 'Enrolled:      yes' in out and 'Dead letters:  2' in out
    assert 'm-0123456789  phone ...1234  receipt rcpt-9' in out
    assert 'journal watermark 9 ahead of the gc store head 4' in out
    assert '...1234  closes 2026-11-05T15:33:20Z (adapter ledger' in out
    assert 'keeper ON  update raised 2026-11-04  marks 60,30  lapses 1 (7 d)' in out
    assert ('Window read:   last good 2026-11-05T15:20:00Z; failing since 2026-11-05T15:21:00Z (3 attempt(s)): '
            'HTTP 503 from https://gw.invalid/v1/window') in out
    assert '5511900001234' not in out


def test_enroll_writes_the_credential_0600_and_prints_nothing_secret(tmp_path):
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            ok = body == {'code': 'enroll-ok', 'name': 'Hou Dynasty'}
            self.send_response(200 if ok else 403)
            self.end_headers()
            self.wfile.write(json.dumps({'city_id': 'city-7', 'credential': 'minted-secret-value'} if ok else {'error': 'used'}).encode())

        def log_message(self, *_):
            pass
    server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        env_file = tmp_path / 'secrets' / 'env'
        env_file.parent.mkdir()
        env_file.write_text('GC_CITY_NAME=hou\nGC_API_BASE_URL=http://127.0.0.1:8372\n')
        gateway = f'http://127.0.0.1:{server.server_address[1]}'
        result = verb('enroll.sh', '--code', 'enroll-ok', '--name', 'Hou Dynasty',
                      GC_WHATSAPP_ADAPTER_ENV=str(env_file), WHATSAPP_GATEWAY_URL=gateway)
        assert result.returncode == 0, result.stderr
        assert 'minted-secret-value' not in result.stdout + result.stderr
        text = env_file.read_text()
        assert "WHATSAPP_GATEWAY_CREDENTIAL='minted-secret-value'" in text
        assert 'GC_CITY_NAME=hou' in text
        assert oct(env_file.stat().st_mode & 0o777) == '0o600'
        again = verb('enroll.sh', '--code', 'enroll-ok', '--name', 'Hou Dynasty',
                     GC_WHATSAPP_ADAPTER_ENV=str(env_file), WHATSAPP_GATEWAY_URL=gateway)
        assert again.returncode == 1 and 'already holds' in again.stderr
    finally:
        server.shutdown()
