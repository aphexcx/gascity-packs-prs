"""Offline command-flow tests: real bash/openssl/curl/git and a loopback API.

The throwaway RSA key and all returned credentials stay in memory. The stub
verifies the RS256 signature and claims before it will issue an access token.
Run directly with test_github_app_token.sh or through the pack's pytest suite.
"""

import base64
import hashlib
import http.server
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from urllib.parse import parse_qs, urlsplit


SCRIPT = Path(__file__).resolve().parents[1] / 'assets/scripts/github-app-token.sh'
TOKEN = 'ghs_offline_test_token'
EXPIRY = '2026-10-19T09:00:00Z'
PERMISSIONS = {'contents': 'write', 'pull_requests': 'write'}


class GitHubAppTokenFlowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.programs = {name: shutil.which(name) for name in ('openssl', 'curl', 'git')}
        if not all(cls.programs.values()):
            raise RuntimeError('flow tests require openssl, curl and git')
        cls.pem = subprocess.run(
            [cls.programs['openssl'], 'genrsa', '2048'], capture_output=True,
            check=True, timeout=20).stdout.decode()
        modulus = subprocess.run(
            [cls.programs['openssl'], 'rsa', '-modulus', '-noout'],
            input=cls.pem.encode(), capture_output=True, check=True, timeout=10).stdout
        cls.modulus = int(modulus.split(b'=')[1], 16)

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        for name, program in {**self.programs, 'python3': sys.executable}.items():
            (self.bin / name).symlink_to(program)
        self.requests = []
        self.server_errors = []
        self.expected_app = '12345'
        self.expected_install = '45678'
        self.mint_status = 201
        self.mint_payload = {'token': TOKEN, 'expires_at': EXPIRY, 'permissions': PERMISSIONS}
        self.repo_status = 200
        self.repo_names = ['aphexcx/gascity-packs-prs']
        self.echo_auth = False
        test = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def respond(self, status, data):
                body = json.dumps(data).encode()
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self):
                test.requests.append(('POST', self.path))
                try:
                    test.assertEqual(self.path, f'/app/installations/{test.expected_install}/access_tokens')
                    test.assertEqual(self.headers['Accept'], 'application/vnd.github+json')
                    test.assertEqual(self.headers['X-GitHub-Api-Version'], '2022-11-28')
                    bearer = self.headers['Authorization']
                    test.assertTrue(bearer.startswith('Bearer '))
                    jwt = bearer.removeprefix('Bearer ')
                    header, claims, signature = jwt.split('.')
                    decode = lambda part: base64.urlsafe_b64decode(part + '=' * (-len(part) % 4))
                    test.assertEqual(json.loads(decode(header)), {'alg': 'RS256', 'typ': 'JWT'})
                    payload = json.loads(decode(claims))
                    test.assertEqual(payload['iss'], test.expected_app)
                    now = int(time.time())
                    test.assertLessEqual(abs(payload['iat'] - (now - 60)), 5)
                    test.assertGreater(payload['exp'], now)
                    test.assertLessEqual(payload['exp'], now + 600)
                    # RSASSA-PKCS1-v1_5 verification, independent of the signer:
                    # 00 01 FF... 00 || SHA-256 DigestInfo || SHA-256(message).
                    signed = (header + '.' + claims).encode()
                    digest_info = bytes.fromhex('3031300d060960864801650304020105000420')
                    digest_info += hashlib.sha256(signed).digest()
                    width = (test.modulus.bit_length() + 7) // 8
                    expected = b'\0\1' + b'\xff' * (width - len(digest_info) - 3) + b'\0' + digest_info
                    recovered = pow(int.from_bytes(decode(signature), 'big'), 65537, test.modulus).to_bytes(width, 'big')
                    test.assertTrue(recovered == expected, 'invalid RSA signature')
                    data = {'message': 'Bad credentials: ' + jwt} if test.echo_auth else test.mint_payload
                    self.respond(test.mint_status, data)
                except Exception as error:
                    test.server_errors.append(type(error).__name__)
                    self.respond(400, {'message': 'stub rejected JWT or request contract'})

            def do_GET(self):
                test.requests.append(('GET', self.path))
                parsed = urlsplit(self.path)
                try:
                    test.assertEqual(parsed.path, '/installation/repositories')
                    test.assertEqual(self.headers['Authorization'], 'Bearer ' + TOKEN)
                    test.assertEqual(self.headers['Accept'], 'application/vnd.github+json')
                    test.assertEqual(self.headers['X-GitHub-Api-Version'], '2022-11-28')
                    query = parse_qs(parsed.query)
                    test.assertEqual(query['per_page'], ['100'])
                    page = int(query['page'][0])
                    names = test.repo_names[(page - 1) * 100:page * 100]
                    data = {'total_count': len(test.repo_names),
                            'repositories': [{'full_name': name} for name in names]}
                    if test.repo_status != 200:
                        data = {'message': 'Repository access denied: ' + TOKEN}
                    self.respond(test.repo_status, data)
                except Exception as error:
                    test.server_errors.append(type(error).__name__)
                    self.respond(400, {'message': 'stub rejected repository request'})

        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={'poll_interval': 0.01}, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop_server)
        self.env = {
            'HOME': str(self.root), 'PATH': str(self.bin),
            'GITHUB_APP_API_URL': f'http://127.0.0.1:{self.server.server_port}',
            'JADEGATE_GITHUB_APP_ID': self.expected_app,
            'JADEGATE_GITHUB_APP_PRIVATE_KEY': self.pem,
            'JADEGATE_GITHUB_APP_INSTALLATION_ID_PACKS': self.expected_install,
        }

    def stop_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def invoke(self, *args, stdin='', env=None, command=None):
        proc = subprocess.run(command or [str(SCRIPT), *args], input=stdin,
                              env=self.env if env is None else env, cwd=self.root,
                              capture_output=True, text=True, timeout=20)
        self.assertFalse(self.server_errors, 'HTTP stub rejected the real request')
        self.assertNotIn(self.pem, proc.stderr)
        self.assertNotIn(TOKEN, proc.stderr)
        self.assertNotIn('eyJ', proc.stderr, 'JWT leaked to diagnostics')
        return proc

    def success(self, *args, **kwargs):
        proc = self.invoke(*args, **kwargs)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(proc.stderr, '')
        return proc.stdout

    def fake_infisical(self, values, *, exit_code=0):
        # All values, including the ephemeral key, are passed in memory.
        self.env.update(INFISICAL_PROJECT_ID='offline-project', INFISICAL_TOKEN='offline-identity',
                        FLOW_DOTENV=values)
        fake = self.bin / 'infisical'
        fake.write_text(f'''#!{sys.executable}
import os, sys
assert sys.argv[1:] == ['export', '--env', 'agents', '--projectId', 'offline-project', '--format', 'dotenv']
assert os.environ['INFISICAL_TOKEN'] == 'offline-identity'
sys.stdout.write(os.environ['FLOW_DOTENV'])
sys.exit({exit_code})
''')
        fake.chmod(0o755)

    def test_default_and_json_outputs(self):
        self.assertEqual(self.success(), TOKEN + '\n')
        self.assertEqual(json.loads(self.success('--json')), self.mint_payload)
        self.assertEqual(len(self.requests), 2)

    def test_prefix_and_www_target(self):
        self.env = {key: value for key, value in self.env.items() if not key.startswith('JADEGATE_')}
        self.env.update(GITHUB_APP_PREFIX='OTHER_APP', OTHER_APP_ID=self.expected_app,
                        OTHER_APP_PRIVATE_KEY=self.pem, OTHER_APP_INSTALLATION_ID_WWW=self.expected_install)
        self.assertEqual(self.success('--target', 'WWW'), TOKEN + '\n')

    def test_check_reuses_one_token_across_repository_pages(self):
        self.repo_names = [f'owner/repo-{index}' for index in range(101)]
        self.assertEqual(self.success('--check').splitlines(), ['expires_at=' + EXPIRY, *self.repo_names])
        self.assertEqual([verb for verb, _ in self.requests], ['POST', 'GET', 'GET'])

    def test_git_credential_protocol_and_real_git_fill(self):
        query = 'protocol=https\nhost=github.com\npath=aphexcx/gascity-packs-prs.git\n\n'
        self.assertEqual(self.success('--git-credential', 'get', stdin=query),
                         'username=x-access-token\npassword=' + TOKEN + '\n\n')
        command = [self.programs['git'], '-c', 'credential.helper=', '-c',
                   'credential.https://github.com.helper=!' + shlex.quote(str(SCRIPT)) + ' --git-credential --target PACKS',
                   'credential', 'fill']
        result = self.success(command=command, stdin=query)
        self.assertIn('username=x-access-token\npassword=' + TOKEN, result)
        self.assertEqual(len(self.requests), 2)

    def test_credential_store_erase_and_other_hosts_do_not_mint(self):
        self.env.pop('JADEGATE_GITHUB_APP_PRIVATE_KEY')
        for op in ('store', 'erase'):
            with self.subTest(op=op):
                self.assertEqual(self.success('--git-credential', op,
                    stdin='protocol=https\nhost=github.com\npassword=ignored\n\n'), '')
        for query in ('protocol=http\nhost=github.com\n\n',
                      'protocol=https\nhost=github.com.evil.test\n\n', 'host=github.com\n\n'):
            self.assertEqual(self.success('--git-credential', stdin=query), '')
        self.assertEqual(self.requests, [])

    def test_missing_variables_and_invalid_args_fail_without_requests(self):
        self.env.pop('JADEGATE_GITHUB_APP_PRIVATE_KEY')
        proc = self.invoke()
        self.assertEqual(proc.returncode, 1)
        self.assertIn('JADEGATE_GITHUB_APP_PRIVATE_KEY', proc.stderr)
        self.assertEqual(proc.stdout, '')
        for args in (('--target',), ('--target', 'BAD'), ('--json', '--check'), ('get',), ('--bad',)):
            with self.subTest(args=args):
                self.assertEqual(self.invoke(*args).returncode, 1)
        self.assertEqual(self.requests, [])

    def test_infisical_multiline_and_escaped_pem_preserve_environment_values(self):
        self.env.pop('JADEGATE_GITHUB_APP_PRIVATE_KEY')
        for encoded in ('"' + self.pem + '"', json.dumps(self.pem), "'" + self.pem + "'"):
            with self.subTest(encoding=encoded[:1]):
                self.fake_infisical('JADEGATE_GITHUB_APP_ID=999\nJADEGATE_GITHUB_APP_PRIVATE_KEY=' + encoded + '\n')
                self.assertEqual(self.success(), TOKEN + '\n')
        self.assertEqual(len(self.requests), 3)

    def test_environment_complete_skips_infisical(self):
        self.fake_infisical('not dotenv', exit_code=1)
        self.assertEqual(self.success(), TOKEN + '\n')

    def test_infisical_loads_all_values_for_custom_prefix_and_target(self):
        self.env = {key: value for key, value in self.env.items() if not key.startswith('JADEGATE_')}
        self.env['GITHUB_APP_PREFIX'] = 'OTHER_APP'
        self.fake_infisical('# exported credentials\nexport OTHER_APP_ID=' + self.expected_app + '\n' +
                            'OTHER_APP_PRIVATE_KEY=' + json.dumps(self.pem) + '\n' +
                            'OTHER_APP_INSTALLATION_ID_PACKS=999\n' +
                            'OTHER_APP_INSTALLATION_ID_WWW=' + self.expected_install + '\n')
        self.assertEqual(self.success('--target', 'WWW'), TOKEN + '\n')

    def test_infisical_values_are_data_and_do_not_execute_shell(self):
        self.env.pop('JADEGATE_GITHUB_APP_PRIVATE_KEY')
        marker = self.root / 'must-not-exist'
        self.fake_infisical('IGNORED="$(printf hacked > ' + shlex.quote(str(marker)) + ')"\n' +
                            'JADEGATE_GITHUB_APP_PRIVATE_KEY=' + json.dumps(self.pem) + '\n')
        self.assertEqual(self.success(), TOKEN + '\n')
        self.assertFalse(marker.exists())

    def test_token_helper_output_tracing_and_exit_trap_are_isolated(self):
        self.env.pop('JADEGATE_GITHUB_APP_PRIVATE_KEY')
        self.fake_infisical('JADEGATE_GITHUB_APP_PRIVATE_KEY=' + json.dumps(self.pem) + '\n')
        self.env.pop('INFISICAL_TOKEN')
        helper = self.root / '.config/infisical-agent/token.sh'
        helper.parent.mkdir(parents=True)
        helper.write_text('''set -x
export INFISICAL_TOKEN=offline-identity
echo must-not-leak
echo must-not-leak >&2
trap 'echo trap-must-not-leak' EXIT
set -- replaced-argv
PATH=/dev/null
''')
        self.assertEqual(self.success(), TOKEN + '\n')

    def test_failed_token_helper_fails_closed(self):
        self.env.pop('JADEGATE_GITHUB_APP_PRIVATE_KEY')
        self.fake_infisical('unused')
        self.env.pop('INFISICAL_TOKEN')
        helper = self.root / '.config/infisical-agent/token.sh'
        helper.parent.mkdir(parents=True)
        helper.write_text('echo must-not-leak\nexit 7\n')
        proc = self.invoke()
        self.assertEqual(proc.returncode, 1)
        self.assertIn('machine-identity login failed', proc.stderr)
        self.assertEqual(proc.stdout, '')
        self.assertEqual(self.requests, [])

    def test_infisical_failures_name_missing_inputs_without_export_dump(self):
        self.env.pop('JADEGATE_GITHUB_APP_PRIVATE_KEY')
        self.fake_infisical(self.pem, exit_code=8)
        proc = self.invoke()
        self.assertEqual(proc.returncode, 1)
        self.assertIn('Infisical export failed (exit 8)', proc.stderr)
        self.assertIn('JADEGATE_GITHUB_APP_PRIVATE_KEY', proc.stderr)
        self.env.pop('INFISICAL_PROJECT_ID')
        self.assertIn('missing INFISICAL_PROJECT_ID', self.invoke().stderr)
        self.assertEqual(self.requests, [])

    def test_http_failures_redact_jwt_and_installation_token(self):
        self.mint_status = 401
        self.echo_auth = True
        proc = self.invoke()
        self.assertEqual(proc.returncode, 1)
        self.assertIn('HTTP 401: Bad credentials: [redacted]', proc.stderr)
        self.assertEqual(proc.stdout, '')
        self.mint_status, self.echo_auth, self.repo_status = 201, False, 403
        proc = self.invoke('--check')
        self.assertEqual(proc.returncode, 1)
        self.assertIn('HTTP 403: Repository access denied: [redacted]', proc.stderr)
        self.assertEqual(proc.stdout, '')

    def test_malformed_token_response_and_bad_key_fail_without_partial_output(self):
        self.mint_payload = {'token': 'bad\npassword=injection', 'expires_at': EXPIRY}
        proc = self.invoke('--git-credential', stdin='protocol=https\nhost=github.com\n\n')
        self.assertEqual(proc.returncode, 1)
        self.assertEqual(proc.stdout, '')
        self.env['JADEGATE_GITHUB_APP_PRIVATE_KEY'] = 'invalid-key'
        proc = self.invoke()
        self.assertEqual(proc.returncode, 1)
        self.assertIn('invalid or encrypted RSA private key', proc.stderr)
        self.assertEqual(len(self.requests), 1)

    def test_curlrc_and_inherited_xtrace_cannot_write_or_print_credentials(self):
        leaked = self.root / 'must-not-exist'
        (self.root / '.curlrc').write_text('output = "' + str(leaked) + '"\nverbose\n')
        env = {**self.env, 'SHELLOPTS': 'xtrace', 'PS4': '${JADEGATE_GITHUB_APP_PRIVATE_KEY} ', 'BASH_XTRACEFD': '1'}
        self.assertEqual(self.success(env=env), TOKEN + '\n')
        self.assertFalse(leaked.exists())

    def test_non_loopback_override_rejected_before_mint(self):
        self.env['GITHUB_APP_API_URL'] = 'https://example.com'
        proc = self.invoke()
        self.assertEqual(proc.returncode, 1)
        self.assertIn('loopback HTTP URL', proc.stderr)
        self.assertEqual(self.requests, [])


if __name__ == '__main__':
    unittest.main(verbosity=2)
