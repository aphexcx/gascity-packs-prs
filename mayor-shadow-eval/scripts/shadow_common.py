"""Shared private storage, process control, config, and accounting (stdlib only)."""
from __future__ import annotations

import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import time
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
    for k in ("max_turns", "timeout_seconds", "context_window", "daily_token_cap", "max_output_tokens"):
        if not isinstance(default[k], int) or default[k] <= 0:
            raise ValueError("positive integer required: " + k)
    default["config_path"] = str(p if p.exists() else PACK / "assets/shadow-eval.toml")
    return default


def private_dir(p: Path) -> Path:
    p.mkdir(parents=True, exist_ok=True, mode=0o700)
    if p.is_symlink():
        raise ValueError("private directory cannot be a symlink")
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
    data = p.read_bytes()
    lines = data.splitlines(keepends=True)
    for i, line in enumerate(lines):
        try:
            row = json.loads(line)
        except ValueError:
            if i == len(lines) - 1 and not line.endswith(b"\n"):
                break  # writer has not completed this row
            raise ValueError("invalid transcript/ledger JSON row") from None
        if isinstance(row, dict):
            rows.append(row)
    return rows


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
                if p.is_file():
                    return p, "gc-session"
    except (OSError, ValueError, subprocess.SubprocessError):
        pass
    # Match watchdog's paragraph anchor; a quoted marker in worker memory is
    # not enough. Deliberately no loose substring fallback for replay safety.
    marker = re.compile(rb"(?:\\n|\n)You are the mayor of this Gas City workspace")
    for p in sorted(project.glob("*.jsonl"), key=lambda p: p.stat().st_mtime, reverse=True):
        with p.open("rb") as f:
            if marker.search(f.read(200000)):
                return p, "paragraph-start"
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
        reserved_tokens += max(0, shadow.get("unconfirmed_token_reservation", 0) - actual)
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
    for k in ("BASH_ENV", "ENV", "GC_RIG", "GC_RIG_ROOT", "GC_TEMPLATE", "GC_SESSION_NAME", "GC_SESSION_ID", "GC_DIR", "GC_WORK_DIR"):
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
    elif __import__("sys").platform == "darwin":
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


def moonshot_key(cfg):
    name = cfg["secret_name"]
    if not re.fullmatch(r"[A-Z][A-Z0-9_]+", name):
        raise ValueError("invalid secret name")
    if os.environ.get(name):
        return os.environ[name]
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
    return token


def prepare_runtime(cfg, name="runtime"):
    root = private_dir(path(cfg["work_dir"]) / name)
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
    save(root / "settings.json", config)
    return root, project


def sandbox_argv(cfg, argv):
    if __import__("sys").platform != "darwin":
        raise RuntimeError("live replay requires macOS sandbox-exec write confinement")
    # The tool hook provides semantic denial; the OS additionally prevents
    # harness/plugin/background writes outside the private work directory.
    allowed = json.dumps(str(path(cfg["work_dir"])))
    profile = '(version 1)(allow default)(deny file-write*)(allow file-write* (subpath ' + allowed + ') (literal "/dev/null"))'
    return ["/usr/bin/sandbox-exec", "-p", profile, *argv]


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
                                stdout=out, stderr=err, start_new_session=True)
        try:
            while proc.poll() is None:
                if kill_switch and path(kill_switch).exists():
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
