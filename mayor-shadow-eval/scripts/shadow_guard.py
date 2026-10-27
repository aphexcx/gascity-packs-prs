#!/usr/bin/env -S python3 -I
"""Default-deny PreToolUse guard. The shell grammar cannot be widened by config."""
from __future__ import annotations

import ast
import datetime as dt
import fcntl
import json
import os
from pathlib import Path
import re
import shlex
import sys
import tomllib

REASON = ("SHADOW EVAL: this call did not run; it was recorded as your intended action. "
          "Do not retry it or work around it; continue as if it had succeeded, or end your turn.")
READ_TOOLS = {"Read", "Grep", "Glob", "LS", "WebFetch", "WebSearch", "ToolSearch"}
FORBIDDEN = re.compile(r"(?:^|\W)(?:rm|mv|cp|mkdir|touch|chmod|sudo|launchctl|infisical)(?:\W|$)")
SECRET_PATH = re.compile(r"(?:\.gc/services/[^/]+/secrets(?:/|$)|\.credentials\.json|token\.sh)")


def protected_path(value):
    if not isinstance(value, str):
        return False
    if SECRET_PATH.search(value):
        return True
    if "/" in value and len(value) < 4096:
        try:
            return bool(SECRET_PATH.search(str(Path(value).expanduser().resolve())))
        except (OSError, ValueError):
            return True
    return False


def safe_python(code: str) -> bool:
    """Allow only literal arithmetic/collections/print; -I is not a sandbox."""
    try:
        tree = ast.parse(code)
    except SyntaxError:
        return False
    allowed = (ast.Module, ast.Expr, ast.Constant, ast.List, ast.Tuple, ast.Dict,
               ast.Set, ast.Load, ast.BinOp, ast.UnaryOp, ast.Add, ast.Sub, ast.Mult,
               ast.Div, ast.Mod, ast.USub, ast.UAdd, ast.Call, ast.Name)
    for node in ast.walk(tree):
        if not isinstance(node, allowed):
            return False
        if isinstance(node, ast.Name) and node.id not in {"print", "len", "sum", "min", "max", "sorted"}:
            return False
        if isinstance(node, ast.Call) and (not isinstance(node.func, ast.Name) or node.keywords):
            return False
    return True


def safe_bash(command: str, patterns: list[str] | None = None) -> bool:
    if not isinstance(command, str) or not command.strip():
        return False
    # Reject substitutions, chaining, redirections, process substitutions and
    # escapes before shlex. Reject even inside quotes: conservative by design.
    if any(c in command for c in "\n\r\x00><|;&`$\\") or FORBIDDEN.search(command) or SECRET_PATH.search(command):
        return False
    try:
        a = shlex.split(command)
    except ValueError:
        return False
    if not a or (patterns is not None and not any(re.search(p, command) for p in patterns)):
        return False
    if any(protected_path(x) for x in a):
        return False
    exe, args = a[0], a[1:]
    if exe == "gc":
        if len(args) < 2:
            return False
        # gc mail read marks the message read. peek is the non-mutating form.
        verbs = {"mail": {"peek", "inbox", "list", "search"},
                 "bd": {"list", "show", "search"}, "session": {"list", "peek"}}
        if args[0] == "slack":
            rest = args[1:]
            if rest[0] in {"channel", "thread"}:
                rest = rest[1:]
            return bool(rest and rest[0] in {"history", "read"})
        return args[1] in verbs.get(args[0], set())
    if exe == "git":
        if not args or args[0] not in {"log", "show", "diff", "status", "branch"}:
            return False
        if any(x.startswith(("--output", "--ext-diff", "--textconv", "--exec", "--open-files-in-pager")) for x in args):
            return False
        if args[0] == "branch":
            return all(x in {"--list", "--all", "-a", "--remotes", "-r", "--show-current", "-v", "-vv", "--no-color"} for x in args[1:])
        # git log/show/diff may run external diff/textconv helpers from config.
        if args[0] in {"log", "show", "diff"}:
            return "--no-ext-diff" in args and "--no-textconv" in args
        return True
    if exe == "python3":
        return len(args) == 3 and args[:2] == ["-I", "-c"] and safe_python(args[2])
    if exe == "sed":
        return len(args) >= 2 and args[0] == "-n" and bool(re.fullmatch(r"\d+(?:,\d+|,\$)?p", args[1])) and all(not x.startswith("-") for x in args[2:])
    if exe == "find":
        return not any(x.startswith(("-exec", "-ok", "-delete", "-fprint", "-fls")) or x == "-L" for x in args)
    if exe == "rg":
        return not any(x.startswith(("--pre", "--hostname-bin")) for x in args)
    if exe == "date":
        return not args or (len(args) == 1 and (args[0] in {"-u", "--utc", "--iso-8601"} or args[0].startswith("+")))
    if exe == "md5":
        return not any(x in {"-p", "-t", "-x"} for x in args)
    if exe == "ffprobe":
        # ffprobe can access arbitrary protocols and write via -o/-report.
        return bool(args) and all(not x.startswith("-") or x in {"-v", "-show_format", "-show_streams", "-of", "-print_format"} for x in args) and not any(":" in x for x in args)
    return exe in {"cat", "head", "tail", "grep", "ls", "wc", "stat", "du", "shasum"}


def decide(name: str, data: dict, patterns: list[str] | None = None) -> str:
    if not isinstance(data, dict) or SECRET_PATH.search(json.dumps(data)):
        return "deny"
    if any(protected_path(data.get(k)) for k in ("file_path", "path", "cwd")):
        return "deny"
    if name in READ_TOOLS:
        return "allow"
    if name == "Bash" and safe_bash(data.get("command", ""), patterns):
        return "allow"
    return "deny"


def main() -> int:
    decision = "deny"
    reason = REASON
    try:
        event = json.load(sys.stdin)
        config = Path(os.environ["SHADOW_CONFIG"])
        defaults = Path(__file__).resolve().parents[1] / "assets/shadow-eval.toml"
        patterns = tomllib.loads(defaults.read_text())["guard"]["bash_allowlist"]
        patterns = tomllib.loads(config.read_text()).get("guard", {}).get("bash_allowlist", patterns)
        name, data = event.get("tool_name", ""), event.get("tool_input", {})
        decision = decide(name, data, patterns)
        entry = {"timestamp": dt.datetime.now(dt.timezone.utc).isoformat(),
                 "tool": name, "input": data, "decision": decision,
                 "tool_use_id": event.get("tool_use_id"), "session_id": event.get("session_id")}
        path = Path(os.environ["SHADOW_DECISIONS"])
        root = Path(os.environ["SHADOW_WORK_DIR"]).resolve()
        if not path.resolve().is_relative_to(root):
            raise ValueError("decision path outside work dir")
        fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "a") as out:
            fcntl.flock(out, fcntl.LOCK_EX)
            os.fchmod(out.fileno(), 0o600)
            out.write(json.dumps(entry, ensure_ascii=False) + "\n")
            out.flush()
            os.fsync(out.fileno())
    except Exception:
        # Failure to record means no tool may run. No exception text: it may
        # contain sensitive input. Exit 2 independently blocks the tool.
        print("SHADOW EVAL: guard or decision ledger unavailable; tool blocked.", file=sys.stderr)
        return 2
    print(json.dumps({"hookSpecificOutput": {"hookEventName": "PreToolUse",
          "permissionDecision": decision, "permissionDecisionReason": reason if decision == "deny" else "SHADOW EVAL: permitted read-only call."}}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
