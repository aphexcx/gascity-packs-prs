#!/usr/bin/env -S python3 -I
"""Default-deny reads confined to explicit roots; every decision is durable."""
from __future__ import annotations

import ast
import datetime as dt
import fcntl
import glob
import json
import os
from pathlib import Path
import re
import shlex
import sys
import tomllib

REASON = ("SHADOW EVAL: this call did not run; it was recorded as your intended action. "
          "Do not retry it or work around it; continue as if it had succeeded, or end your turn.")
SHAPE_REASON = ("SHADOW EVAL: this read was not run because of its shape; rephrase it as a "
                "simple read-only command (no substitutions, no writers in the pipeline).")
PATH_TOOLS = {"Read", "Grep", "Glob", "LS", "NotebookRead"}
READ_TOOLS = PATH_TOOLS | {"ToolSearch", "Skill"}
MUTATING_TOOLS = {"Write", "Edit", "MultiEdit", "NotebookEdit", "WebFetch", "WebSearch", "CronCreate", "CronDelete", "SendMessage"}
TIMERS = {"Monitor", "ScheduleWakeup"}


def roots():
    home = Path.home().resolve()
    city = (home / "city").resolve()
    memory = (home / ".claude/projects/-Users-tailor64s-macbook-pro-city/memory").resolve()
    excluded = [city / p for p in ("assets/private", ".gc/keys", "work/eval-kimi-k31", "ops/shadow-eval.toml")]
    return city, memory, excluded


def expand(value, cwd=None, resolve=True):
    if not isinstance(value, str) or not value or "\x00" in value:
        raise ValueError("invalid path")
    value = re.sub(r"\$HOME(?=/|$)", str(Path.home()), value)
    if "$" in value or "`" in value or "~" in value[1:] or (value.startswith("~") and not value.startswith("~/") and value != "~"):
        raise ValueError("unsupported expansion")
    p = Path(value).expanduser()
    p = p if p.is_absolute() else Path(cwd or os.getcwd()) / p
    return p.resolve() if resolve else Path(os.path.abspath(p))


def allowed_path(value, cwd=None, recursive=False):
    try:
        if not isinstance(value, str) or any(c in value for c in "*?[]{}"):
            return False
        p = expand(value, cwd)
        lexical = expand(value, cwd, resolve=False)
        city, memory, excluded = roots()
        if not (p.is_relative_to(city) or p.is_relative_to(memory)):
            return False
        # Check lexical and resolved exclusions: a configured excluded directory
        # may itself be a symlink. Service secrets are denied even before creation.
        for x in excluded:
            if p.is_relative_to(x) or p.is_relative_to(x.resolve()) or lexical.is_relative_to(x):
                return False
            if recursive and (x.is_relative_to(p) or x.resolve().is_relative_to(p)):
                return False
        services = city / ".gc/services"
        for candidate in (lexical, p):
            if candidate.is_relative_to(services):
                rel = candidate.relative_to(services).parts
                if len(rel) >= 2 and rel[1] == "secrets":
                    return False
                if recursive and len(rel) < 2:
                    return False
        if recursive and services.is_relative_to(p):
            return False
        # Recursive tools may follow links. Refuse a scope with an escaping link
        # rather than trusting a particular tool's traversal implementation.
        if recursive and p.is_dir():
            for base, dirs, files in os.walk(p, followlinks=False):
                for name in dirs + files:
                    item = Path(base) / name
                    if item.is_symlink() and (item.is_dir() or not allowed_path(str(item), cwd)):
                        return False
        return True
    except (OSError, RuntimeError, ValueError):
        return False


def safe_python(code):
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


def segments(command):
    """Split shell control operators outside quotes; never evaluate a shell."""
    if not isinstance(command, str) or any(c in command for c in "\r\x00`\\"):
        raise ValueError("unsupported shell")
    result, buf, quote, i = [], [], None, 0
    while i < len(command):
        c = command[i]
        if quote:
            buf.append(c)
            if c == quote:
                quote = None
        elif c in "\"'":
            quote = c
            buf.append(c)
        elif command.startswith("2>/dev/null", i) and (not buf or buf[-1].isspace()) and (i + 11 == len(command) or command[i+11].isspace() or command[i+11] in "|;&"):
            buf.append(" ")
            i += 10
        elif c in "|;&\n":
            op = c
            if c in "|&" and command[i:i+2] == c * 2:
                op += c
                i += 1
            if op == "&":
                raise ValueError("background command")
            part = "".join(buf).strip()
            if part:
                result.append(part)
            elif c != "\n":
                raise ValueError("empty command")
            buf = []
        elif c in "<>":
            raise ValueError("redirection")
        else:
            buf.append(c)
        i += 1
    if quote:
        raise ValueError("unterminated quote")
    part = "".join(buf).strip()
    if part:
        result.append(part)
    elif command.rstrip().endswith(("|", "&")):
        raise ValueError("unfinished pipeline")
    if not result:
        raise ValueError("empty command")
    return result


def recursive_read(exe, args):
    """Recognize implicit traversal, including bundled short options."""
    if exe in {"find", "rg", "du", "tree"}:
        return True
    for i, arg in enumerate(args):
        if arg == "--":
            break
        if exe in {"grep", "ls"} and arg == "--recursive":
            return True
        if exe == "grep" and arg in {"--dereference-recursive", "--directories=recurse"}:
            return True
        if exe == "grep" and arg in {"-d", "--directories"} and args[i+1:i+2] == ["recurse"]:
            return True
        if arg.startswith("-") and not arg.startswith("--"):
            if exe == "grep" and any(flag in arg[1:] for flag in "rR"):
                return True
            if exe == "ls" and "R" in arg[1:]:
                return True
    return False


def search_paths(args):
    """Separate a small, known search grammar from patterns/option values.

    Unknown options leave the implicit cwd in scope. They cannot use a value
    that happens to name a safe directory to hide an implicit recursive read.
    """
    positional, explicit_pattern, files, options, i = [], False, False, True, 0
    while i < len(args):
        arg = args[i]
        i += 1
        if options and arg == "--":
            options = False
        elif options and arg.startswith("--"):
            key, sep, _ = arg.partition("=")
            if key in {"--regexp", "--directories"}:
                if not sep:
                    if i == len(args):
                        return []
                    i += 1
                explicit_pattern |= key == "--regexp"
            elif arg == "--files":
                files = True
            elif arg not in {"--recursive", "--dereference-recursive"}:
                return []
        elif options and arg.startswith("-") and arg != "-":
            for offset, flag in enumerate(arg[1:], 2):
                if flag in "ed":
                    if offset == len(arg):
                        if i == len(args):
                            return []
                        i += 1
                    explicit_pattern |= flag == "e"
                    break
                if flag not in "rRnivwxlLqshHIcFoUaz":
                    return []
        else:
            positional.append(arg)
    return positional if explicit_pattern or files else positional[1:]


def allowed_read_scope(exe, args, cwd):
    if exe not in {"grep", "rg", "find", "ls", "du", "wc", "cat", "head", "tail", "tree", "stat", "sed", "md5", "shasum"}:
        return True
    recursive = recursive_read(exe, args)
    # A shallow ls lists names only; the contract explicitly allows ls ~/city/work.
    # Every other directory operand is a recursive scope, independent of flags.
    if exe == "ls" and not recursive:
        return True
    values = [arg.split("=", 1)[-1] if arg.startswith("-") and "=" in arg else arg for arg in args]
    directories = [value for value in values if expand(value, cwd).is_dir()]
    if recursive:
        operands = search_paths(args) if exe in {"grep", "rg"} else args
        if exe in {"ls", "du"}:
            # Unknown options may consume the next value. Only known switches
            # can establish explicit operands instead of an implicit cwd read.
            switches = "aAbBcCdFfGgHhikLlmnopqRrSstUuvx1" if exe == "ls" else "achHklmsx"
            known = all(not arg.startswith("-") or arg == "--"
                        or (exe == "ls" and arg == "--recursive")
                        or (not arg.startswith("--") and all(c in switches for c in arg[1:]))
                        for arg in args)
            operands = [arg for arg in args if not arg.startswith("-")] if known else []
        if (not directories or not operands
                or (exe == "find" and operands[0].startswith("-"))):
            directories.append(cwd or os.getcwd())
    return all(allowed_path(value, cwd, recursive=True) for value in directories)


def safe_simple(command, patterns=None, cwd=None):
    try:
        a = shlex.split(command)
    except ValueError:
        return False
    if not a or (patterns is not None and not any(re.search(p, command) for p in patterns)):
        return False
    # Reject metacharacters in EVERY argument, including quoted glob patterns.
    # Resolve all remaining arguments, including option values, against the cwd.
    for arg in a:
        if any(c in arg for c in "*?[]{}<>&|;`\n"):
            return False
        if arg.startswith("-") and "/" in arg and "=" not in arg:
            return False
        value = arg.split("=", 1)[-1] if arg.startswith("-") and "=" in arg else arg
        if not allowed_path(value, cwd):
            return False
        if "://" in value or value.startswith(("file:", "http:", "https:")):
            return False
    exe, args = a[0], a[1:]
    if not allowed_path(cwd or os.getcwd()):
        return False
    if not allowed_read_scope(exe, args, cwd):
        return False
    if exe == "gc":
        if len(args) < 2 or any(x.startswith(("--city", "--rig", "--store", "--db", "--config")) for x in args):
            return False
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
        if any(x.startswith(("--output", "--ext-diff", "--textconv", "--exec", "--open-files-in-pager", "--show-signature", "--recurse-submodules")) for x in args):
            return False
        if args[0] == "branch":
            return all(x in {"--list", "--all", "-a", "--remotes", "-r", "--show-current", "-v", "-vv", "--no-color"} for x in args[1:])
        return args[0] == "status" or ("--no-ext-diff" in args and "--no-textconv" in args)
    if exe == "python3":
        return len(args) == 3 and args[:2] == ["-I", "-c"] and safe_python(args[2])
    if exe == "sed":
        return len(args) >= 2 and args[0] == "-n" and bool(re.fullmatch(r"\d+(?:,\d+)?p", args[1])) and all(not x.startswith("-") for x in args[2:])
    if exe in {"find", "rg", "grep", "du"}:
        if any(x.startswith(("-exec", "-ok", "-delete", "-fprint", "-fls", "--pre", "--hostname-bin", "--include", "--exclude", "--ignore-file", "--file=")) or x in {"-L", "-f", "--file"} or (x.startswith("-f") and not x.startswith("--")) for x in args):
            return False
        return True
    if exe == "date":
        return not args or (len(args) == 1 and (args[0] in {"-u", "--utc", "--iso-8601"} or args[0].startswith("+")))
    if exe == "md5":
        return not any(x in {"-p", "-t", "-x"} for x in args)
    if exe == "shasum":
        return not any(x.startswith(("-c", "--check", "--status")) for x in args)
    if exe == "tail" and any(x.startswith(("--follow", "--pid")) or (x.startswith("-") and any(c in x[1:] for c in "fF")) for x in args):
        return False
    if exe == "ls" and any(x.startswith("-") and ("L" in x or (x.startswith("--") and x not in {"--", "--recursive"})) for x in args):
        return False
    # Media demuxers can follow file/network references from an allowed input.
    # Leave ffprobe denied until that secondary-read surface is confined too.
    return exe in {"cat", "head", "tail", "ls", "wc", "stat"}


def safe_bash(command, patterns=None, cwd=None):
    try:
        return all(safe_simple(s, patterns, cwd) for s in segments(command))
    except (OSError, RuntimeError, ValueError):
        return False


def redirection_target(command):
    """Find an actual file redirection, ignoring quoted text and fd duplication."""
    quote, i = None, 0
    while i < len(command):
        c = command[i]
        if c == "\\" and quote != "'":
            i += 2
            continue
        if quote:
            if c == quote:
                quote = None
        elif c in "\"'":
            quote = c
        elif c == ">":
            i += 1
            if i < len(command) and command[i] == ">":
                i += 1
            tail = command[i:].lstrip()
            if tail.startswith("&"):
                i += 1
                continue
            try:
                args = shlex.split(tail)
            except ValueError:
                return "unparsed-redirection"
            if args and args[0] != "/dev/null":
                return args[0]
        i += 1
    return None


def mutation(name, data):
    """Classify actual command heads, not words quoted in a read's arguments."""
    if name in TIMERS:
        return "timer"
    if name in MUTATING_TOOLS:
        return "mutation"
    if name != "Bash":
        return "read" if name in READ_TOOLS else "unknown"
    command = data.get("command", "")
    try:
        parts = segments(command)
    except ValueError:
        # Redirections are writers, while unknown substitutions remain shape
        # denials. Never execute parsing or infer success from either category.
        if redirection_target(command):
            return "mutation"
        parts = [command]
    timer = False
    for part in parts:
        try:
            argv = shlex.split(part)
        except ValueError:
            continue
        if not argv:
            continue
        exe, args = Path(argv[0]).name, argv[1:]
        if exe == "perl" and len(args) >= 2 and args[0] == "-e" and re.match(r"sleep\b", args[1]):
            timer = True
            continue
        if exe in {"launchctl", "crontab", "rm", "mv", "cp", "mkdir", "touch", "chmod", "tee", "sudo", "curl"}:
            return "mutation"
        if exe == "gc" and args:
            if args[0] in {"sling", "nudge"}:
                return "mutation"
            verbs = {"mail": {"send", "reply", "read"}, "slack": {"reply", "reply-current", "publish", "publish-to-channel"},
                     "wecom": {"publish"}, "bd": {"create", "update", "close", "comment"}}
            if len(args) > 1 and args[1] in verbs.get(args[0], set()):
                return "mutation"
        if exe == "bd" and args and args[0] in {"create", "update", "close", "comment"}:
            return "mutation"
        if exe == "git":
            while len(args) >= 2 and args[0] in {"-C", "-c"}:
                args = args[2:]
            if args and (args[0] in {"commit", "push", "checkout", "add", "switch", "reset", "restore", "merge", "rebase", "clean"}
                         or (args[0] == "branch" and not safe_simple(part))):
                return "mutation"
    return "timer" if timer else "read"


def decide(name, data, patterns=None, cwd=None):
    if not isinstance(data, dict):
        return "deny"
    cwd = data.get("cwd", cwd or os.getcwd())
    if not allowed_path(cwd):
        return "deny"
    for k in ("file_path", "path", "notebook_path", "cwd"):
        if k in data and not allowed_path(data[k], cwd, recursive=name in {"Grep", "Glob"} and k != "cwd"):
            return "deny"
    if name in PATH_TOOLS:
        target = next((data[k] for k in ("file_path", "path", "notebook_path") if k in data), cwd)
        if not allowed_path(target, cwd, recursive=name in {"Grep", "Glob"}):
            return "deny"
        if name == "Glob":
            pattern = data.get("pattern", "")
            if not isinstance(pattern, str) or not pattern:
                return "deny"
            raw = str(expand(target, cwd) / pattern) if not pattern.startswith(("/", "~", "$")) else pattern
            base = re.split(r"[*?[]", raw, maxsplit=1)[0]
            base = base.rsplit("/", 1)[0] if base != raw else base
            if not allowed_path(base or ".", cwd, recursive=True):
                return "deny"
            if not all(allowed_path(x, cwd) for x in glob.iglob(str(Path(raw).expanduser()), recursive=True)):
                return "deny"
        return "allow"
    if name in {"ToolSearch", "Skill"}:
        return "allow"
    if name == "Bash" and safe_bash(data.get("command", ""), patterns, cwd):
        return "allow"
    return "deny"


def decision_class(name, data, decision):
    if decision == "allow":
        return "read-allowed"
    return "mutation-denied" if mutation(name, data) == "mutation" else "shape-denied"


def main():
    try:
        event = json.load(sys.stdin)
        config = Path(os.environ["SHADOW_CONFIG"])
        defaults = Path(__file__).resolve().parents[1] / "assets/shadow-eval.toml"
        patterns = tomllib.loads(defaults.read_text())["guard"]["bash_allowlist"]
        patterns = tomllib.loads(config.read_text()).get("guard", {}).get("bash_allowlist", patterns)
        name, data = event.get("tool_name", ""), event.get("tool_input", {})
        decision = decide(name, data, patterns, event.get("cwd"))
        kind = decision_class(name, data, decision)
        entry = {"timestamp": dt.datetime.now(dt.timezone.utc).isoformat(), "tool": name,
                 "input": data, "decision": decision, "class": kind, "action_class": mutation(name, data),
                 "tool_use_id": event.get("tool_use_id"), "session_id": event.get("session_id")}
        dest = Path(os.environ["SHADOW_DECISIONS"])
        root = Path(os.environ["SHADOW_WORK_DIR"]).resolve()
        if not dest.resolve().is_relative_to(root):
            raise ValueError("decision path outside work dir")
        fd = os.open(dest, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "a") as out:
            fcntl.flock(out, fcntl.LOCK_EX)
            os.fchmod(out.fileno(), 0o600)
            out.write(json.dumps(entry, ensure_ascii=False) + "\n")
            out.flush()
            os.fsync(out.fileno())
    except Exception:
        print("SHADOW EVAL: guard or decision ledger unavailable; tool blocked.", file=sys.stderr)
        return 2
    reason = REASON if kind == "mutation-denied" else SHAPE_REASON
    print(json.dumps({"hookSpecificOutput": {"hookEventName": "PreToolUse",
          "permissionDecision": decision, "permissionDecisionReason": reason if decision == "deny" else "SHADOW EVAL: permitted read-only call."}}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
