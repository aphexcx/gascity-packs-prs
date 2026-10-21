#!/bin/bash
# Mint a GitHub App installation token without persisting credentials.
# Requires bash, python3, openssl and curl; infisical is optional.
{ set +x; } >/dev/null 2>&1
set -euo pipefail

gat_die() { printf 'github-app-token: %s\n' "$1" >&2; exit 1; }
gat_mode=token
gat_target=PACKS
gat_operation=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --target)
      [ "$#" -ge 2 ] || gat_die '--target requires PACKS or WWW'
      gat_target=$2; shift ;;
    --json|--git-credential|--check)
      [ "$gat_mode" = token ] || gat_die 'choose only one output mode'
      gat_mode=${1#--} ;;
    get|store|erase)
      [ -z "$gat_operation" ] || gat_die 'choose only one credential operation'
      gat_operation=$1 ;;
    --help|-h)
      printf '%s\n' 'Usage: github-app-token.sh [--target PACKS|WWW] [--json|--git-credential|--check]' \
        'Git appends get/store/erase to --git-credential; store and erase are no-ops.' \
        'GITHUB_APP_PREFIX defaults to JADEGATE_GITHUB_APP. See gascity/README.md.'
      exit 0 ;;
    *) gat_die 'unknown argument; see --help' ;;
  esac
  shift
done
case "$gat_target" in PACKS|WWW) ;; *) gat_die '--target requires PACKS or WWW' ;; esac
if [ "$gat_mode" = git-credential ]; then
  gat_protocol= gat_host=
  while IFS= read -r gat_line || [ -n "$gat_line" ]; do
    [ -n "$gat_line" ] || break
    case "$gat_line" in
      protocol=*) gat_protocol=${gat_line#*=} ;;
      host=*) gat_host=${gat_line#*=} ;;
    esac
  done
  case "${gat_operation:-get}" in store|erase) exit 0 ;; esac
  [ "$gat_protocol" = https ] || exit 0
  case "$gat_host" in github.com|github.com:443) ;; *) exit 0 ;; esac
elif [ -n "$gat_operation" ]; then
  gat_die 'get/store/erase require --git-credential'
fi

gat_prefix=${GITHUB_APP_PREFIX:-JADEGATE_GITHUB_APP}
[[ "$gat_prefix" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]] || gat_die 'invalid GITHUB_APP_PREFIX'
command -v python3 >/dev/null 2>&1 || gat_die 'missing dependency: python3'

# JSON, dotenv parsing, base64url, and pipe management use the Python stdlib.
# This heredoc contains code only. Keys, JWTs and tokens travel through memory
# and anonymous pipes, never temporary files or subprocess argument lists.
exec python3 -I - "$gat_prefix" "$gat_target" "$gat_mode" <<'PY'
import base64
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import time
from urllib.parse import urlsplit


class TokenError(Exception):
    pass


child_env = {name: os.environ[name] for name in ('PATH', 'HOME', 'LANG') if name in os.environ}


def run(args, **kwargs):
    kwargs.setdefault('env', child_env)
    return subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                          timeout=45, check=False, **kwargs)


def load_identity_token():
    helper = os.path.join(os.environ.get('HOME', ''), '.config/infisical-agent/token.sh')
    if not os.environ.get('HOME') or not os.access(helper, os.R_OK):
        return ''
    # Only the token crosses the shell boundary. Close stdin, silence helper
    # output/traps, and bound the entire process group (including descendants).
    command = ('exec 3>&1 >/dev/null 2>&1; . "$1" && set +x && '
               'builtin printf \'%s\' "${INFISICAL_TOKEN-}" >&3')
    with subprocess.Popen(['/bin/bash', '-c', command, 'github-app-token', helper],
                          stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                          stderr=subprocess.DEVNULL, start_new_session=True) as proc:
        try:
            token, _ = proc.communicate(timeout=20)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            proc.communicate()
            raise TokenError('Infisical machine-identity login timed out after 20 seconds') from None
        if proc.returncode or not token:
            raise TokenError('Infisical machine-identity login failed; INFISICAL_TOKEN unavailable')
        return token.decode('utf-8')


def json_export(text):
    try:
        data = json.loads(text)
    except (ValueError, UnicodeError):
        raise TokenError('invalid Infisical JSON export') from None
    if isinstance(data, list):
        if any(not isinstance(item, dict) or not isinstance(item.get('key'), str)
               or not isinstance(item.get('value'), str) for item in data):
            raise TokenError('invalid Infisical JSON export')
        return {item['key']: item['value'] for item in data}
    if isinstance(data, dict) and all(isinstance(value, str) for value in data.values()):
        return data
    raise TokenError('invalid Infisical JSON export')


def dotenv(text):
    # Infisical dotenv: quoted (including multiline) or unquoted assignments.
    # Parse as data: never eval/source exported values or expand $()/backticks.
    entry = re.compile(r'''[ \t]*(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*=[ \t]*(?:"((?:[^"\\]|\\.)*)"|'([^']*)'|([^\r\n]*))[ \t]*(?:\#[^\r\n]*)?(?:\r?\n|$)''', re.S)
    values = {}
    pos = 0
    while pos < len(text):
        blank = re.match(r'[ \t]*(?:\#[^\r\n]*)?\r?\n', text[pos:])
        if blank:
            pos += blank.end()
            continue
        if not text[pos:].strip():
            break
        match = entry.match(text, pos)
        if not match:
            raise TokenError('invalid Infisical dotenv export')
        name, double, single, plain = match.groups()
        if double is not None:
            value = re.sub(r'\\([nrt"\\$])', lambda m: {
                'n': '\n', 'r': '\r', 't': '\t', '"': '"', '\\': '\\', '$': '$'
            }[m[1]], double)
        elif single is not None:
            value = single
        else:
            value = re.split(r'\s+#', plain, maxsplit=1)[0].strip()
        values[name] = value
        pos = match.end()
    return values


def b64url(data):
    return base64.urlsafe_b64encode(data).rstrip(b'=').decode('ascii')


def sign(pem, message):
    # Refuse EC/encrypted/invalid keys before labeling the signature RS256.
    checked = run(['openssl', 'rsa', '-check', '-noout', '-passin', 'pass:'],
                  input=pem.encode())
    if checked.returncode:
        raise TokenError('invalid or encrypted RSA private key: ' + key_name)
    read_fd, write_fd = os.pipe()
    try:
        # Start the reader before writing; even a large key cannot fill a pipe
        # while no process is reading it.
        with subprocess.Popen(['openssl', 'dgst', '-sha256', '-sign',
                               f'/dev/fd/{read_fd}', '-passin', 'pass:'],
                              stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                              stderr=subprocess.DEVNULL, pass_fds=(read_fd,), env=child_env) as proc:
            os.close(read_fd)
            read_fd = None
            with os.fdopen(write_fd, 'wb') as key_pipe:
                write_fd = None
                key_pipe.write(pem.encode())
            signature, _ = proc.communicate(message.encode(), timeout=15)
            if proc.returncode:
                raise TokenError('RSA signing failed')
            return b64url(signature)
    finally:
        if read_fd is not None:
            os.close(read_fd)
        if write_fd is not None:
            os.close(write_fd)


def safe_message(message):
    if not isinstance(message, str):
        return 'request failed'
    for secret in sorted(secrets, key=len, reverse=True):
        if secret:
            message = message.replace(secret, '[redacted]')
    return ''.join(c if c.isprintable() else ' ' for c in message)[:500]


def request(method, path, auth):
    # -q must be first: ~/.curlrc could otherwise enable logs or file output.
    # A config on stdin keeps Authorization out of the process argument list.
    config = 'header = "Authorization: Bearer ' + auth + '"\n'
    args = ['curl', '-q', '--silent', '--show-error', '--connect-timeout', '10',
            '--max-time', '30', '--request', method, '--url', api_url + path,
            '--header', 'Accept: application/vnd.github+json',
            '--header', 'X-GitHub-Api-Version: 2022-11-28',
            '--write-out', '\n%{http_code}', '--config', '-']
    if api_url != 'https://api.github.com':
        args += ['--noproxy', '*']
    result = run(args, input=config.encode())
    if result.returncode:
        raise TokenError(f'GitHub transport failed (curl exit {result.returncode})')
    body, _, status = result.stdout.rpartition(b'\n')
    code = status.decode('ascii', errors='replace')
    if not re.fullmatch(r'[1-5][0-9]{2}', code):
        raise TokenError('invalid GitHub HTTP response')
    try:
        data = json.loads(body)
    except (ValueError, UnicodeError):
        raise TokenError(f'GitHub HTTP {code}: invalid JSON response') from None
    if not code.startswith('2'):
        message = data.get('message', 'request failed') if isinstance(data, dict) else 'request failed'
        raise TokenError(f'GitHub HTTP {code}: {safe_message(message)}')
    if not isinstance(data, dict):
        raise TokenError(f'GitHub HTTP {code}: expected a JSON object')
    return data


try:
    prefix, target, mode = sys.argv[1:]
    id_name, key_name, install_name = (prefix + '_ID', prefix + '_PRIVATE_KEY',
                                     prefix + '_INSTALLATION_ID_' + target)
    names = (id_name, key_name, install_name)
    values = {name: os.environ.get(name, '') for name in names}
    secrets = [values[key_name], os.environ.get('INFISICAL_TOKEN', '')]
    missing = [name for name in names if not values[name]]
    if missing and shutil.which('infisical'):
        inherited_token = bool(os.environ.get('INFISICAL_TOKEN'))
        if not inherited_token:
            os.environ['INFISICAL_TOKEN'] = load_identity_token()
            secrets.append(os.environ['INFISICAL_TOKEN'])
        for name in ('INFISICAL_PROJECT_ID', 'INFISICAL_TOKEN'):
            if not os.environ.get(name):
                raise TokenError('missing ' + name + '; needed to load ' + ', '.join(missing))
        export_args = ['infisical', 'export', '--env', 'agents', '--projectId',
                       os.environ['INFISICAL_PROJECT_ID'], '--format']
        exported = run([*export_args, 'json'], env=os.environ)
        parser = json_export
        if exported.returncode:
            # Older CLIs may not support JSON; dotenv is compatibility only.
            exported = run([*export_args, 'dotenv'], env=os.environ)
            parser = dotenv
        if exported.returncode:
            hint = '; stale INFISICAL_TOKEN? unset it and retry' if inherited_token else ''
            raise TokenError(f'Infisical export failed (exit {exported.returncode}); missing '
                             + ', '.join(missing) + hint)
        loaded = parser(exported.stdout.decode('utf-8'))
        for name in missing:
            values[name] = loaded.get(name, '')
        del loaded, exported
    missing = [name for name in names if not values[name]]
    if missing:
        raise TokenError('missing required variables: ' + ', '.join(missing))
    for name in (id_name, install_name):
        if not re.fullmatch(r'[0-9]+', values[name]):
            raise TokenError('expected a numeric ID: ' + name)
    for dep in ('curl', 'openssl'):
        if not shutil.which(dep):
            raise TokenError('missing dependency: ' + dep)

    # The sole alternate endpoint is a loopback HTTP stub for offline tests.
    api_url = os.environ.get('GITHUB_APP_API_URL', 'https://api.github.com').rstrip('/')
    parsed = urlsplit(api_url)
    if api_url != 'https://api.github.com' and not (
        parsed.scheme == 'http' and parsed.hostname in ('127.0.0.1', '::1', 'localhost')
        and parsed.port and not parsed.username and not parsed.password
        and not parsed.path and not parsed.query and not parsed.fragment
    ):
        raise TokenError('GITHUB_APP_API_URL must be https://api.github.com or a loopback HTTP URL with a port')

    pem = values[key_name]
    secrets += [pem, *pem.splitlines()]
    now = int(time.time())
    header = b64url(b'{"alg":"RS256","typ":"JWT"}')
    claims = b64url(json.dumps({'iss': values[id_name], 'iat': now - 60, 'exp': now + 540}).encode())
    unsigned = header + '.' + claims
    jwt = unsigned + '.' + sign(pem, unsigned)
    secrets.append(jwt)
    # One token per invocation, reused by every page of --check; no disk cache.
    minted = request('POST', '/app/installations/' + values[install_name] + '/access_tokens', jwt)
    token = minted.get('token')
    expires = minted.get('expires_at')
    permissions = minted.get('permissions', {})
    if not isinstance(token, str) or not re.fullmatch(r'[A-Za-z0-9_.\-]+', token):
        raise TokenError('GitHub response missing a valid token')
    secrets.append(token)
    if not isinstance(expires, str) or not re.fullmatch(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z', expires):
        raise TokenError('GitHub response missing a valid expires_at')
    if not isinstance(permissions, dict):
        raise TokenError('GitHub response has invalid permissions')
    if mode == 'token':
        print(token)
    elif mode == 'json':
        print(json.dumps({'token': token, 'expires_at': expires, 'permissions': permissions}))
    elif mode == 'git-credential':
        print('username=x-access-token\npassword=' + token + '\n')
    else:
        repos = []
        page = 1
        while True:
            data = request('GET', f'/installation/repositories?per_page=100&page={page}', token)
            batch = data.get('repositories')
            if not isinstance(batch, list) or any(
                not isinstance(repo, dict) or not isinstance(repo.get('full_name'), str)
                or not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repo['full_name'])
                for repo in batch
            ):
                raise TokenError('GitHub response has invalid repositories')
            repos.extend(repo['full_name'] for repo in batch)
            if len(batch) < 100:
                break
            page += 1
        print('expires_at=' + expires)
        for repo in repos:
            print(repo)
except TokenError as error:
    print('github-app-token: ' + str(error), file=sys.stderr)
    sys.exit(1)
except (OSError, ValueError, subprocess.SubprocessError):
    # Library errors may include command inputs; never dump them or a traceback.
    print('github-app-token: credential loading, signing or request failed', file=sys.stderr)
    sys.exit(1)
PY
