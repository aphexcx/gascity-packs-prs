"""Shared private storage, process control, config, and accounting (stdlib only)."""
from __future__ import annotations

import datetime as dt
import fcntl
import glob
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import time
import uuid
import sys
import tomllib
from zoneinfo import ZoneInfo

PACK = Path(__file__).resolve().parents[1]
CST = ZoneInfo("Asia/Shanghai")
USAGE_KEYS = {"input": "input_tokens", "cache_read": "cache_read_input_tokens",
              "cache_write": "cache_creation_input_tokens", "output": "output_tokens"}


def path(value) -> Path:
    return Path(value).expanduser().resolve()


def load_config(filename) -> dict:
    default = tomllib.loads((PACK / "assets/shadow-eval.toml").read_text())
    p = path(filename)
    if p.exists():
        for k, v in tomllib.loads(p.read_text()).items():
            if isinstance(v, dict) and isinstance(default.get(k), dict):
                default[k].update(v)
            else:
                default[k] = v
    elif str(filename) != "~/city/ops/shadow-eval.toml":
        raise ValueError("config does not exist")
    for k in ("city_dir", "source_config_dir", "work_dir", "kill_switch", "codex_shim"):
        default[k] = str(path(default[k]))
    for k in ("max_turns", "timeout_seconds", "context_window", "daily_token_cap", "max_output_tokens", "max_wait", "target_turns_per_day", "poll_seconds"):
        if type(default[k]) is not int or default[k] <= 0:
            raise ValueError("positive integer required: " + k)
    for k in ("settle_seconds", "sample_every"):
        if type(default[k]) is not int or default[k] < 0:
            raise ValueError("nonnegative integer required: " + k)
    default["config_path"] = str(p if p.exists() else PACK / "assets/shadow-eval.toml")
    default.setdefault("sandbox", Path("/usr/bin/sandbox-exec").is_file())
    if type(default["sandbox"]) is not bool:
        raise ValueError("sandbox must be a boolean")
    exclusions = default.get("sandbox_excluded_paths", [])
    if not isinstance(exclusions, list) or any(not isinstance(p, str) or not p for p in exclusions):
        raise ValueError("sandbox_excluded_paths must be a list of paths")
    return default


def private_dir(p: Path) -> Path:
    if p.is_symlink():
        raise ValueError("private directory cannot be a symlink")
    if not p.parent.exists():
        private_dir(p.parent)
    p.mkdir(exist_ok=True, mode=0o700)
    p.chmod(0o700)
    return p


def write_private(p: Path, text: str, exclusive=False):
    private_dir(p.parent)
    flags = os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW | (os.O_EXCL if exclusive else os.O_TRUNC)
    fd = os.open(p, flags, 0o600)
    with os.fdopen(fd, "w") as f:
        os.fchmod(f.fileno(), 0o600)
        f.write(text)
        f.flush()
        os.fsync(f.fileno())


def save(p: Path, obj, exclusive=False):
    write_private(p, json.dumps(obj, ensure_ascii=False, indent=2) + "\n", exclusive)


def jsonl(p: Path) -> list[dict]:
    if not p.exists():
        return []
    rows = []
    with p.open("rb") as f:
        for line in f:
            try:
                row = json.loads(line)
            except ValueError:
                if not line.endswith(b"\n"):
                    break
                raise ValueError("invalid transcript/ledger JSON row") from None
            if isinstance(row, dict):
                rows.append(row)
    return rows


_TAILS = {}


def tail_rows(source, root):
    """Persist complete rows and byte offset; partial final lines wait for append.

    The cache is replay evidence, never a resume transcript. An inode/size
    regression starts a new generation; old evidence is retained, not deleted.
    """
    cache_root = private_dir(root / "runtime" / "tail-cache")
    key = hashlib.sha256(str(source).encode()).hexdigest()
    manifest = cache_root / (key + ".json")
    state = json.loads(manifest.read_text()) if manifest.exists() else {}
    st = source.stat()
    identity = [st.st_dev, st.st_ino]
    if state.get("identity") != identity or st.st_size < state.get("size", 0):
        state = {"identity": identity, "offset": 0, "size": 0,
                 "cache": str(cache_root / (key + "-" + uuid.uuid4().hex + ".jsonl"))}
    cache = Path(state["cache"])
    memo_key = str(cache)
    rows = _TAILS.setdefault(memo_key, jsonl(cache) if memo_key not in _TAILS else [])
    # Recover a crash between cache append and manifest persistence by trusting
    # only the committed byte offset in the manifest.
    if cache.exists() and cache.stat().st_size != state["offset"]:
        state["cache"] = str(cache_root / (key + "-" + uuid.uuid4().hex + ".jsonl"))
        cache = Path(state["cache"])
        rows = []
        state["offset"] = 0
        _TAILS[str(cache)] = rows
    fd = os.open(cache, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "ab") as out, source.open("rb") as inp:
        os.fchmod(out.fileno(), 0o600)
        inp.seek(state["offset"])
        while True:
            line = inp.readline()
            if not line or not line.endswith(b"\n"):
                break
            row = json.loads(line)
            if isinstance(row, dict):
                rows.append(row)
            out.write(line)
            state["offset"] += len(line)
        out.flush()
        os.fsync(out.fileno())
    state["size"] = st.st_size
    save(manifest, state)
    return rows


def switch_on(filename):
    try:
        return path(filename).read_text().strip() == "on"
    except OSError:
        return False


def row_text(row):
    value = row.get("message", {}).get("content", [])
    if isinstance(value, str):
        return value
    return "\n".join(b.get("text", "") for b in value if isinstance(b, dict) and b.get("type") == "text")


def is_prompt(row):
    if row.get("type") != "user" or any(row.get(k) for k in ("isMeta", "isCompactSummary", "isSidechain")):
        return False
    c = row.get("message", {}).get("content", [])
    if not c or (isinstance(c, list) and any(b.get("type") == "tool_result" for b in c if isinstance(b, dict))):
        return False
    text = row_text(row).lstrip()
    return not text.startswith(("[Request interrupted by user]", "[Request interrupted by user for tool use]", "<command-name>", "<local-command-stdout>"))


_SOURCE_CACHE = {}
_SOURCE_PROGRESS = {}
MARKER = re.compile(r"(?:^|\n)You are the mayor of this Gas City workspace")


def marker_in_row(row):
    if row.get("type") == "user":
        c = row.get("message", {}).get("content", [])
        # Never let tool_result text identify a source.
        if isinstance(c, list) and any(b.get("type") == "tool_result" for b in c if isinstance(b, dict)):
            return False
        return bool(MARKER.search(row_text(row)))
    if row.get("type") == "attachment":
        def strings(value):
            if isinstance(value, str):
                yield value
            elif isinstance(value, list):
                for v in value:
                    yield from strings(v)
            elif isinstance(value, dict) and value.get("type") != "tool_result":
                for v in value.values():
                    yield from strings(v)
        return any(MARKER.search(t) for t in strings([row.get("attachment", {}), row.get("rendered", [])]))
    return False


def source_info(source):
    st = source.stat()
    key = (str(source), st.st_dev, st.st_ino, st.st_size)
    if key not in _SOURCE_CACHE:
        identity = (str(source), st.st_dev, st.st_ino)
        progress = _SOURCE_PROGRESS.get(identity, {})
        if st.st_size < progress.get("size", 0):
            progress = {}
        found = progress.get("marker", False)
        newest, trigger = progress.get("newest_timestamp", 0), progress.get("newest_trigger_timestamp", 0)
        offset = progress.get("offset", 0)
        with source.open("rb") as f:
            f.seek(offset)
            for raw in f:
                if not raw.endswith(b"\n"):
                    break
                offset += len(raw)
                try:
                    row = json.loads(raw)
                except ValueError:
                    continue
                if not isinstance(row, dict):
                    continue
                found = found or marker_in_row(row)
                newest = max(newest, timestamp(row.get("timestamp")))
                if is_prompt(row):
                    trigger = max(trigger, timestamp(row.get("timestamp")))
        _SOURCE_CACHE[key] = {"marker": found, "newest_timestamp": newest,
                              "newest_trigger_timestamp": trigger}
        _SOURCE_PROGRESS[identity] = {**_SOURCE_CACHE[key], "offset": offset, "size": st.st_size}
    info = dict(_SOURCE_CACHE[key])
    now = time.time()
    info.update({"source_basename": source.name,
                 "newest_row_age_seconds": max(0, now - info["newest_timestamp"]),
                 "newest_trigger_age_seconds": max(0, now - info["newest_trigger_timestamp"]),
                 "stale": not info["newest_timestamp"] or now - info["newest_timestamp"] >= 6 * 3600})
    return info


def cst_day(timestamp=None):
    if timestamp is None:
        return dt.datetime.now(CST).date().isoformat()
    if isinstance(timestamp, str):
        timestamp = dt.datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
    return timestamp.astimezone(CST).date().isoformat()


def timestamp(value):
    try:
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    except (AttributeError, ValueError):
        return 0


def lock(root: Path):
    private_dir(root)
    f = open(root / "watch.lock", "a+")
    try:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        f.close()
        raise RuntimeError("another shadow watcher holds the lock") from None
    return f


def project_dir(config_dir: Path, city: Path):
    return config_dir / "projects" / re.sub(r"[^a-zA-Z0-9]", "-", str(city))


def discover(cfg):
    project = project_dir(path(cfg["source_config_dir"]), path(cfg["city_dir"]))
    try:
        out = subprocess.run(["gc", "session", "list", "--json"], cwd=cfg["city_dir"],
                             capture_output=True, text=True, check=True, timeout=10)
        sessions = json.loads(out.stdout).get("sessions", [])
        mayors = [s for s in sessions if s.get("alias") == "mayor" and not s.get("closed")]
        if len(mayors) == 1:
            key = mayors[0].get("session_key", "")
            if re.fullmatch(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}", key):
                p = project / (key + ".jsonl")
                if p.is_file() and not source_info(p)["stale"]:
                    return p, "gc-session"
    except (OSError, ValueError, subprocess.SubprocessError):
        pass
    for p in sorted(project.glob("*.jsonl"), key=lambda p: p.stat().st_mtime, reverse=True):
        if source_info(p)["marker"]:
            return p, "paragraph-start-full-scan"
    raise RuntimeError("no mayor transcript found")


def usage(raw):
    out = {k: int(raw.get(v, 0) or 0) for k, v in USAGE_KEYS.items()}
    out["cache_write_1h"] = int(raw.get("cache_creation", {}).get("ephemeral_1h_input_tokens", 0) or 0)
    out["cache_write"] = max(0, out["cache_write"] - out["cache_write_1h"])
    return out


def cost(cfg, model, tokens):
    rates = cfg.get("prices", {}).get(model)
    if not rates or any(k not in rates for k, n in tokens.items() if n):
        return None
    return sum(n * rates.get(k, 0) for k, n in tokens.items()) / 1000000


def ledgers(root: Path, day=None):
    files = (root / "ledger" / day).glob("*.json") if day else (root / "ledger").glob("*/*.json")
    return [(p, json.loads(p.read_text())) for p in sorted(files)]


def totals(items):
    counts = {"seen": len(items), "forked": 0, "skipped": 0, "failed": 0, "running": 0}
    tokens, reserved_tokens, known_cost, unpriced, latencies = 0, 0, 0.0, 0, []
    last_error = None
    for _, r in items:
        shadow = r.get("shadow", {})
        counts["forked"] += bool(shadow.get("fork_file"))
        status = r.get("status")
        if status in counts:
            counts[status] += 1
        actual = sum(shadow.get("usage", {}).values())
        tokens += actual
        reserved = shadow.get("token_reservation", 0) if status == "running" else shadow.get("unconfirmed_token_reservation", 0)
        reserved_tokens += max(0, reserved - actual)
        amount = shadow.get("cost_usd")
        if amount is None and shadow.get("usage"):
            unpriced += 1
        elif amount is not None:
            known_cost += amount
        if shadow.get("wall_seconds") is not None:
            latencies.append(shadow["wall_seconds"])
        if shadow.get("errors"):
            last_error = shadow["errors"][-1]
    return {**counts, "tokens": tokens, "reserved_tokens": reserved_tokens,
            "budget_used_tokens": tokens + reserved_tokens, "known_cost_usd": known_cost,
            "unpriced_turns": unpriced, "latencies": latencies, "last_error": last_error}


def normal_env():
    e = dict(os.environ)
    for k in list(e):
        if k.startswith(("ANTHROPIC_", "CLAUDE_CODE_OAUTH", "CLAUDE_CODE_SUBAGENT", "CLAUDE_CODE_USE_")) or k in {"CLAUDE_CONFIG_DIR", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "MOONSHOT_API_KEY"}:
            e.pop(k, None)
    # gc resolves GC_SESSION_ID, then GC_ALIAS, then GC_AGENT. Never carry
    # worker aliases, bead-store overrides or session instance credentials into
    # the mayor replay. The watcher supplies its explicit city/agent identity.
    city_environment = {"GC_BIN", "GC_HOME", "GC_CITY", "GC_CITY_PATH", "GC_DOLT_PORT",
                        "GC_CITY_RUNTIME_DIR", "GC_DISABLE_USAGE_METRICS"}
    for k in list(e):
        if (k.startswith("GC_") and k not in city_environment) or k in {"BASH_ENV", "ENV", "RIPGREP_CONFIG_PATH"}:
            e.pop(k, None)
    e.update({"CLAUDE_CODE_MAX_RETRIES": "0", "GIT_OPTIONAL_LOCKS": "0", "GIT_PAGER": "cat",
              "PAGER": "cat", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
              "DISABLE_AUTOUPDATER": "1", "CLAUDE_BASH_NO_LOGIN": "1", "GIT_NO_LAZY_FETCH": "1",
              "GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "core.fsmonitor", "GIT_CONFIG_VALUE_0": "false",
              "GIT_CONFIG_KEY_1": "core.pager", "GIT_CONFIG_VALUE_1": "cat"})
    return e


def normal_auth(e, source_dir):
    """Keep normal first-party credentials in memory; never serialize secrets.

    A private CLAUDE_CONFIG_DIR changes the macOS keychain service hash. Read
    the normal service instead; do not log in, change or refresh its record.
    """
    inherited = os.environ.get("CLAUDE_CODE_OAUTH_TOKEN")
    if inherited:
        e["CLAUDE_CODE_OAUTH_TOKEN"] = inherited
        return
    credentials = path(source_dir) / ".credentials.json"
    raw = ""
    if credentials.is_file():
        raw = credentials.read_text()
    elif sys.platform == "darwin":
        r = subprocess.run(["security", "find-generic-password", "-s", "Claude Code-credentials", "-w"],
                           capture_output=True, text=True, timeout=10)
        if r.returncode == 0:
            raw = r.stdout
    if raw:
        token = json.loads(raw).get("claudeAiOauth", {}).get("accessToken")
        if token:
            e["CLAUDE_CODE_OAUTH_TOKEN"] = token
            return
    # Explicit normal API auth is also supported, without changing endpoints.
    for name in ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"):
        if os.environ.get(name):
            e[name] = os.environ[name]
            return
    raise RuntimeError("normal Anthropic auth unavailable in private runtime")


_MOONSHOT_KEYS = {}


def moonshot_key(cfg):
    name = cfg["secret_name"]
    if not re.fullmatch(r"[A-Z][A-Z0-9_]+", name):
        raise ValueError("invalid secret name")
    if os.environ.get(name):
        return os.environ[name]
    cache_key = (cfg["infisical_project"], name)
    if cache_key in _MOONSHOT_KEYS:
        if not _MOONSHOT_KEYS[cache_key]:
            raise RuntimeError("Moonshot credential unavailable for this watcher run")
        return _MOONSHOT_KEYS[cache_key]
    _MOONSHOT_KEYS[cache_key] = None
    # The sourced token only enters this subprocess. JSON export is parsed,
    # never evaluated as shell code and never printed (including on failure).
    r = subprocess.run(["/bin/bash", "-c", 'source "$HOME/.config/infisical-agent/token.sh" >/dev/null 2>&1 && exec infisical export --env=agents --projectId "$1" --format=json',
                        "shadow-secret", cfg["infisical_project"]], capture_output=True, text=True, timeout=20)
    if r.returncode:
        raise RuntimeError("secret export failed")
    data = json.loads(r.stdout)
    if isinstance(data, list):
        data = {x.get("key"): x.get("value") for x in data}
    token = data.get(name)
    if not token:
        raise RuntimeError("Moonshot credential unavailable")
    _MOONSHOT_KEYS[cache_key] = token
    return token


def prepare_runtime(cfg, name="runtime"):
    root = private_dir(path(cfg["work_dir"]) / name)
    for project_settings in (path(cfg["city_dir"]) / ".claude/settings.json", path(cfg["city_dir"]) / ".claude/settings.local.json"):
        if project_settings.exists():
            project_config = json.loads(project_settings.read_text())
            if any(project_config.get(k) for k in ("hooks", "enabledPlugins", "env", "apiKeyHelper", "statusLine")):
                raise RuntimeError("project settings can launch live hooks; refusing replay")
    source = path(cfg["source_config_dir"])
    project = private_dir(project_dir(root, path(cfg["city_dir"])))
    # Only read-only context is shared. Runtime settings, sessions, telemetry,
    # caches, and preferences are private. No credentials are copied to disk.
    for rel in ("CLAUDE.md", "skills", "agents", "plugins"):
        origin, dest = source / rel, root / rel
        if origin.exists() and not dest.exists():
            dest.symlink_to(origin, target_is_directory=origin.is_dir())
    origin = project_dir(source, path(cfg["city_dir"])) / "memory"
    if origin.exists() and not (project / "memory").exists():
        (project / "memory").symlink_to(origin, target_is_directory=True)
    settings = source / "settings.json"
    config = json.loads(settings.read_text()) if settings.exists() else {}
    # Existing hooks can send messages before PreToolUse. Keep presentation
    # and plugin settings, but only our explicit hook runs in the shadow.
    for k in ("hooks", "env", "permissions", "apiKeyHelper", "statusLine", "enabledPlugins"):
        config.pop(k, None)
    config["enabledPlugins"] = {}
    save(root / "plugin-fidelity.json", {"matched": False, "reason": "plugins disabled because their SessionStart hooks run before the tool guard"})
    save(root / "settings.json", config)
    return root, project


def sandbox_argv(cfg, argv):
    if sys.platform != "darwin":
        raise RuntimeError("live replay requires macOS sandbox-exec write confinement")
    # The tool hook provides semantic denial; the OS additionally prevents
    # harness/plugin/background writes outside the private work directory.
    allowed = json.dumps(str(path(cfg["work_dir"])))
    profile = '(version 1)(allow default)(deny file-write*)(allow file-write* (subpath ' + allowed + ') (literal "/dev/null"))'
    return ["/usr/bin/sandbox-exec", "-p", profile, *argv]


def shadow_sandbox_profile(cfg, runtime, temporary, decisions):
    """Seatbelt policy for the shadow and every descendant, including hooks.

    Resolve symlinks as well as lexical paths. Wildcards are single components;
    regex rules cover future service secret directories without reading them.
    Only this turn's runtime is exempt from the work-directory read denial.
    """
    city, work, source = (path(cfg[k]) for k in ("city_dir", "work_dir", "source_config_dir"))
    runtime, temporary, decisions = map(path, (runtime, temporary, decisions))
    if (runtime == work or not runtime.is_relative_to(work)
            or temporary == work or not temporary.is_relative_to(work)
            or not decisions.is_relative_to(work)
            or runtime.is_relative_to(source) or temporary.is_relative_to(source)):
        raise ValueError("invalid sandbox runtime boundary")

    def sub(p):
        return '(subpath ' + json.dumps(str(p), ensure_ascii=False) + ')'

    def literal(p):
        return '(literal ' + json.dumps(str(p), ensure_ascii=False) + ')'

    def deny_except(root, exceptions):
        return '(deny file-read* (require-all ' + sub(root) + ' (require-not (require-any ' + ' '.join(exceptions) + '))))'

    lines = ['(version 1)', '(allow default)', '(deny file-write*)',
             '(allow file-write* ' + ' '.join((sub(runtime), sub(temporary), literal(decisions), literal('/dev/null'))) + ')',
             deny_except(work, [sub(runtime), sub(temporary)]),
             deny_except(city / 'work/eval-kimi-k31', [sub(runtime), sub(temporary)]),
             '(allow file-read-metadata ' + literal(decisions) + ')']
    context = [source / rel for rel in ('CLAUDE.md', 'skills', 'agents', 'plugins')]
    context.append(project_dir(source, city) / 'memory')
    # Exceptions apply only to the live config exclusion, never to the work
    # ledger, credentials or explicitly configured excluded paths.
    lines.append(deny_except(source, [sub(p) for p in context]))
    exclusions = [str(city / p) for p in ('assets/private', '.gc/keys', '.gc/services/*/secrets', 'ops/shadow-eval.toml')]
    exclusions += [str(Path.home() / '.config/infisical-agent'), str(source / '.credentials.json'), cfg['config_path']]
    exclusions += cfg.get('sandbox_excluded_paths', [])
    for value in exclusions:
        p = Path(value).expanduser()
        p = p if p.is_absolute() else city / p
        if any(c in str(p) for c in '?[]{}\n\r\x00'):
            raise ValueError('unsupported sandbox exclusion')
        if '*' in str(p):
            if any('*' in part and part != '*' for part in p.parts):
                raise ValueError('wildcards must be whole path components')
            prefix, suffix = str(p).split('*', 1)
            for pattern in {str(p), str(path(prefix)) + '/*' + suffix}:
                regex = '^' + re.escape(pattern).replace(r'\*', '[^/]+') + '(/|$)'
                # Use an ordinary Scheme string: #"..." is raw and would
                # retain JSON's doubled backslashes, silently weakening rules.
                lines.append('(deny file-read* (regex ' + json.dumps(regex) + '))')
            # Resolve existing symlink targets without opening any excluded file.
            for match in glob.glob(str(p)):
                lines.append('(deny file-read* ' + sub(path(match)) + ')')
        else:
            for target in {Path(os.path.abspath(p)), path(p)}:
                lines.append('(deny file-read* ' + sub(target) + ')')
    return '\n'.join(lines) + '\n'


def shadow_sandbox_argv(cfg, argv):
    if not cfg['sandbox']:
        return argv
    if not Path('/usr/bin/sandbox-exec').is_file():
        raise RuntimeError('sandbox requested but sandbox-exec unavailable')
    runtime = path(cfg['_shadow_runtime'])
    profile = runtime / 'shadow.sb'
    write_private(profile, shadow_sandbox_profile(cfg, runtime, runtime / 'tmp', cfg['_shadow_decisions']))
    return ['/usr/bin/sandbox-exec', '-f', str(profile), *argv]


def prepare_codex_runtime(cfg):
    """Read-only links to normal auth/profile; all generated state stays private."""
    root = private_dir(path(cfg["work_dir"]) / "codex-runtime")
    source = path(os.environ.get("CODEX_HOME", "~/.codex"))
    for name in ("auth.json", "city.config.toml"):
        origin, dest = source / name, root / name
        if origin.exists() and not dest.exists():
            dest.symlink_to(origin)
    if not (root / "city.config.toml").exists():
        raise RuntimeError("city Codex profile unavailable")
    return root


def run_process(argv, env, cwd, stdout: Path, stderr: Path, timeout, kill_switch=None, stdin_path=None):
    started = time.monotonic()
    write_private(stdout, "", exclusive=True)
    write_private(stderr, "", exclusive=True)
    error = None
    with stdout.open("ab") as out, stderr.open("ab") as err, open(stdin_path or os.devnull, "rb") as inp:
        proc = subprocess.Popen(argv, env=env, cwd=cwd, stdin=inp,
                                stdout=out, stderr=err, start_new_session=True, umask=0o077)
        try:
            while proc.poll() is None:
                if kill_switch and not switch_on(kill_switch):
                    error = "kill_switch"
                    break
                if time.monotonic() - started >= timeout:
                    error = "timeout"
                    break
                time.sleep(0.1)
        finally:
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGTERM)
                try:
                    proc.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    os.killpg(proc.pid, signal.SIGKILL)
                    proc.wait()
    return proc.returncode, time.monotonic() - started, error


def fingerprint(p: Path, limit=None):
    h = hashlib.sha256()
    with p.open("rb") as f:
        remaining = p.stat().st_size if limit is None else limit
        while remaining > 0:
            chunk = f.read(min(remaining, 1024 * 1024))
            if not chunk:
                break
            h.update(chunk)
            remaining -= len(chunk)
    return h.hexdigest()
