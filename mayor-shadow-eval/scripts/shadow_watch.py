#!/usr/bin/env -S python3 -I
"""Queue completed mayor prompt turns, fork context, and record guarded replay."""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import signal
import sys
import time
import uuid

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from shadow_common import (PACK, cost, cst_day, discover, fingerprint, jsonl, ledgers,
    load_config, lock, moonshot_key, normal_auth, normal_env, path, prepare_runtime,
    private_dir, run_process, sandbox_argv, save, timestamp, totals, usage, write_private)
from shadow_guard import decide


def content(row):
    c = row.get("message", {}).get("content", [])
    return [{"type": "text", "text": c}] if isinstance(c, str) else c


def prompt(row):
    if row.get("type") != "user" or row.get("isMeta") or row.get("isCompactSummary") or row.get("isSidechain"):
        return False
    c = content(row)
    return bool(c) and not any(b.get("type") == "tool_result" for b in c)


def text_of(row):
    return "\n".join(b.get("text", "") for b in content(row) if b.get("type") == "text")


def blocks(rows):
    result = []
    for row in rows:
        if row.get("type") == "assistant" and not row.get("isSidechain"):
            for b in content(row):
                if b.get("type") == "text":
                    result.append({"type": "text", "text": b.get("text", "")})
                elif b.get("type") == "tool_use":
                    result.append({"type": "tool_use", "id": b.get("id"),
                                   "name": b.get("name"), "input": b.get("input", {})})
    return result


def completed_turns(rows, settle_seconds, now=None):
    starts = [i for i, r in enumerate(rows) if prompt(r)]
    now = time.time() if now is None else now
    out = []
    for n, start in enumerate(starts):
        end = starts[n + 1] if n + 1 < len(starts) else len(rows)
        assistants = [r for r in rows[start+1:end] if r.get("type") == "assistant" and not r.get("isSidechain")]
        if n + 1 < len(starts) or (assistants and timestamp(assistants[-1].get("timestamp")) and now - timestamp(assistants[-1].get("timestamp")) >= settle_seconds):
            out.append((start, end))
    return out


def fork_rows(rows, start, new_id):
    # Do not reconstruct only messages. Hook output, attachment, summary,
    # compaction boundary and snapshot rows all belong to the prefix.
    prefix = copy.deepcopy(rows[:start])
    for r in prefix:
        for key in ("sessionId", "session_id"):
            if key in r:
                r[key] = new_id
    return prefix


def context_estimate(prefix, trigger):
    active_start = max((i for i, r in enumerate(prefix) if r.get("subtype") == "compact_boundary"), default=0)
    active = prefix[active_start:]
    # Prefer actual provider context usage on the latest assistant row, then
    # conservatively budget each subsequent UTF-8 byte as a token. Includes
    # cache categories, which input_tokens alone omits.
    for i in range(len(active)-1, -1, -1):
        r = active[i]
        u = r.get("message", {}).get("usage", {})
        if r.get("type") == "assistant" and u:
            tokens = usage(u)
            base = sum(tokens[k] for k in ("input", "cache_read", "cache_write", "cache_write_1h", "output"))
            suffix = active[i+1:]
            return base + sum(len(json.dumps(x, ensure_ascii=False).encode()) for x in suffix) + len(text_of(trigger).encode()) + 4096
    return sum(len(json.dumps(r, ensure_ascii=False).encode()) for r in active) + len(text_of(trigger).encode()) + 4096


def action(blocks_):
    for b in blocks_:
        if b.get("type") != "tool_use" or decide(b.get("name", ""), b.get("input", {})) == "allow":
            continue
        name, data = b.get("name"), b.get("input", {})
        target = next((str(data[k]) for k in ("recipient", "to", "channel", "channel_id", "file_path", "path") if data.get(k)), None)
        head = None
        if name == "Bash":
            try:
                a = shlex.split(data.get("command", ""))
            except ValueError:
                a = []
            head = a[:3]
            for flag in ("--to", "--channel", "--channel-id", "--path"):
                if flag in a and a.index(flag)+1 < len(a):
                    target = a[a.index(flag)+1]
            if len(a) > 3 and a[:3] in (["gc", "mail", "send"], ["gc", "mail", "reply"]):
                target = a[3]
            target = target or " ".join(head)
        return {"tool": name, "target": target, "command_head": head}
    return None


def diff(fable, shadow):
    a, b = action(fable), action(shadow)
    return {"fable_first_mutation": a, "shadow_first_mutation": b, "same_action": a == b,
            "rule": "first denied-by-static-guard tool + explicit target + Bash first three argv tokens; both absent match; copy and decision semantics require judge"}


def parse_stream(rows):
    result = next((r for r in reversed(rows) if r.get("type") == "result"), {})
    total = usage(result.get("usage", {}))
    if not result.get("usage"):
        # Stream assistant messages can be split into blocks with the same id.
        # Use the latest cumulative usage per message id, never sum duplicates.
        messages = {}
        for r in rows:
            msg = r.get("message", {})
            if r.get("type") == "assistant" and msg.get("usage"):
                messages[msg.get("id", r.get("uuid"))] = usage(msg["usage"])
        total = {k: sum(v[k] for v in messages.values()) for k in total}
    return blocks(rows), total, result


def classify_error(rows, stderr, code, timed):
    if timed:
        return timed
    # Keep vendor errors private, and put only a stable category in status/mail.
    result = next((r for r in reversed(rows) if r.get("type") == "result"), {})
    if code == 0 and result and not result.get("is_error"):
        return None
    if result.get("subtype") == "error_max_turns":
        return "max_turns"
    error_rows = [r for r in rows if r.get("isApiErrorMessage") or r.get("type") == "error" or (r.get("type") == "result" and r.get("is_error"))]
    text = json.dumps(error_rows) + stderr
    for pattern, label in ((r"\b401\b|authentication_error|invalid.*api.key", "http_401"),
                           (r"\b429\b|rate_limit", "http_429"),
                           (r"\b5\d\d\b|overloaded_error", "http_5xx")):
        if re.search(pattern, text, re.I):
            return label
    if result.get("is_error"):
        return "model_error"
    if code:
        return "process_failed"
    if not result:
        return "missing_result"
    return None


def fork_evidence(source_prefix, seed, runtime_file):
    """Content-free provenance. Claude records prompt snapshots and wire shapes."""
    seed_rows = jsonl(seed)
    actual = jsonl(runtime_file) if runtime_file and runtime_file.exists() else []
    expected = fork_rows(source_prefix, len(source_prefix), seed.stem)
    snapshots = [r["attachment"] for r in source_prefix if r.get("attachment", {}).get("type") == "prompt_snapshot"]
    snapshot = next((s for s in reversed(snapshots) if s.get("tools")), snapshots[-1] if snapshots else {})
    retained = [r["attachment"] for r in actual if r.get("attachment", {}).get("type") == "prompt_snapshot"]
    shape = next((r["shape"] for r in actual if r.get("type") == "api-request-shape"), {})
    wire_tools = {t["name"]: t for t in shape.get("tools", [])}
    tools = snapshot.get("tools", [])
    deferred = {}
    for r in source_prefix:
        attachment = r.get("attachment", {})
        if attachment.get("type") == "deferred_tools_record":
            for t in attachment.get("entries", []):
                if isinstance(t, dict) and t.get("name"):
                    deferred[t["name"]] = t
    def tool_schema(t):
        schema = t.get("schema", {})
        return schema.get("input_schema", schema)
    unchanged = [t["name"] for t in tools if wire_tools.get(t["name"], {}).get("input_schema") == tool_schema(t) and wire_tools[t["name"]].get("description") == t.get("description")]
    system = snapshot.get("systemPrompt")
    canonical = lambda value: hashlib.sha256(json.dumps(value, sort_keys=True, ensure_ascii=False).encode()).hexdigest()
    return {"prefix_rows_identical_except_session_id": seed_rows == expected,
            "mayor_role_in_prefix": any("You are the mayor of this Gas City workspace" in text_of(r) for r in source_prefix),
            "source_system_prompt_sha256": canonical(system) if system else None,
            "system_prompt_snapshot_retained": bool(system and any(s.get("systemPrompt") == system for s in retained)),
            "source_tool_count": len(tools), "unchanged_wire_tool_count": len(unchanged),
            "all_source_tool_schemas_retained": bool(tools and len(unchanged) == len(tools)),
            "additional_wire_tools": sorted(set(wire_tools) - {t["name"] for t in tools}),
            "deferred_wire_schemas_from_source": sorted(n for n, t in wire_tools.items() if n in deferred and t.get("input_schema") == deferred[n].get("input_schema") and t.get("description") == deferred[n].get("description")),
            "instruction_attachments_in_prefix": sum(r.get("attachment", {}).get("type") == "instructions" for r in source_prefix),
            "runtime_fork_exists": bool(runtime_file and runtime_file.exists())}


def run_turn(cfg, source, discovery, rows, interval, seq, *, model=None):
    start, end = interval
    trigger = rows[start]
    trigger_id = trigger.get("uuid")
    if not isinstance(trigger_id, str) or not re.fullmatch(r"[a-zA-Z0-9-]+", trigger_id):
        raise ValueError("invalid trigger uuid")
    root = private_dir(path(cfg["work_dir"]))
    today = cst_day()
    directory = private_dir(root / "ledger" / today)
    dest = directory / f"{seq:06d}-{trigger_id}.json"
    artifact = private_dir(root / "turns" / f"{seq:06d}-{trigger_id}")
    selected = model or cfg["model"]
    fable = blocks(rows[start+1:end])
    entry = {"schema": 1, "session_id": trigger.get("sessionId", source.stem),
             "trigger_uuid": trigger_id, "trigger_timestamp": trigger.get("timestamp"),
             "trigger_text": text_of(trigger), "trigger_day_cst": cst_day(trigger.get("timestamp")),
             "run_day_cst": today, "source": str(source), "discovery": discovery,
             "fable": fable, "status": "running", "shadow": {"model": selected, "blocks": [], "decisions": [], "errors": [], "usage": {}, "cost_usd": None},
             "diff": diff(fable, [])}
    save(dest, entry, exclusive=True)  # claim before forking; crashes never replay silently
    shadow = entry["shadow"]
    before_size = source.stat().st_size
    before_hash = fingerprint(source, before_size)
    total_started = time.monotonic()
    try:
        estimate = context_estimate(rows[:start], trigger)
        shadow["context_estimate"] = estimate
        day_total = totals(ledgers(root, today))["budget_used_tokens"]
        # Reserve a full upper bound for each request. Claude's max output
        # token limit is passed in env; input cannot exceed context_window.
        reservation = cfg["max_turns"] * (cfg["context_window"] + cfg["max_output_tokens"])
        shadow["token_reservation"] = reservation
        skip = None
        if path(cfg["kill_switch"]).exists():
            skip = "kill_switch"
        elif any(b.get("type") != "text" for b in content(trigger)):
            skip = "non_text_trigger"  # text CLI cannot faithfully reproduce images
        elif estimate + cfg["max_output_tokens"] > cfg["context_window"]:
            skip = "context_window"
        elif day_total + reservation > cfg["daily_token_cap"]:
            skip = "daily_token_cap"
        if skip:
            entry["status"], entry["skip_reason"] = "skipped", skip
            return dest, entry
        runtime, projects = prepare_runtime(cfg)
        seed_id = str(uuid.uuid4())
        seed = projects / (seed_id + ".jsonl")
        write_private(seed, "".join(json.dumps(r, ensure_ascii=False) + "\n" for r in fork_rows(rows, start, seed_id)), exclusive=True)
        shadow.update({"fork_file": str(seed), "fork_session_id": seed_id, "prefix_rows": start,
                       "guard_decisions_file": str(artifact / "decisions.jsonl")})
        save(dest, entry)
        env = normal_env()
        env.update({"CLAUDE_CONFIG_DIR": str(runtime), "TMPDIR": str(private_dir(root / "tmp")),
                    "GC_AGENT": "mayor", "GC_CITY_PATH": cfg["city_dir"], "GC_CITY": cfg["city_dir"],
                    "SHADOW_PACK_DIR": str(PACK), "SHADOW_CONFIG": cfg["config_path"],
                    "SHADOW_WORK_DIR": str(root), "SHADOW_DECISIONS": shadow["guard_decisions_file"],
                    "CLAUDE_CODE_MAX_OUTPUT_TOKENS": str(cfg["max_output_tokens"])})
        env.update({k: str(v) for k, v in cfg.get("env", {}).items()})
        if selected.startswith("claude-"):
            normal_auth(env, cfg["source_config_dir"])
        else:
            env["ANTHROPIC_BASE_URL"] = cfg["base_url"]
            env["ANTHROPIC_AUTH_TOKEN"] = moonshot_key(cfg)
            for k in ("ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL"):
                env[k] = selected
        # Settings are sourced only from the private runtime. The command's
        # final settings override supplies the only live hook.
        all_usage = {k: 0 for k in usage({})}
        prompt_file = artifact / "trigger.txt"
        write_private(prompt_file, text_of(trigger), exclusive=True)
        for attempt in range(3):
            if path(cfg["kill_switch"]).exists():
                shadow["errors"].append("kill_switch")
                break
            out, err = artifact / f"stream-{attempt}.jsonl", artifact / f"stderr-{attempt}.txt"
            argv = ["claude", "-p", "--resume", seed_id, "--fork-session", "--model", selected,
                    "--permission-mode", "dontAsk", "--setting-sources", "user",
                    "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}',
                    "--settings", str(PACK / "assets/shadow-settings.json"),
                    "--output-format", "stream-json", "--verbose", "--max-turns", str(cfg["max_turns"]),
                    "--effort", "xhigh"]
            shadow["request_started"] = True
            save(dest, entry)
            code, wall, timed = run_process(sandbox_argv(cfg, argv), env, cfg["city_dir"], out, err,
                                           cfg["timeout_seconds"], cfg["kill_switch"], stdin_path=prompt_file)
            stream = jsonl(out)
            answer, spent, result = parse_stream(stream)
            for k, n in spent.items():
                all_usage[k] += n
            error = classify_error(stream, err.read_text(), code, timed)
            shadow.update({"blocks": answer, "usage": dict(all_usage), "exit_status": code,
                           "stream_file": str(out), "stderr_file": str(err), "attempts": attempt + 1,
                           "runtime_session_id": result.get("session_id"),
                           "runtime_init": next((r for r in stream if r.get("type") == "system" and r.get("subtype") == "init"), None)})
            shadow["observed_models"] = sorted({r["message"]["model"] for r in stream if r.get("type") == "assistant" and r.get("message", {}).get("model")})
            if result.get("session_id"):
                shadow["runtime_fork_file"] = str(projects / (result["session_id"] + ".jsonl"))
            if error:
                shadow["errors"].append(error)
                if error not in {"http_401", "http_429", "http_5xx"} and not result.get("usage"):
                    shadow["unconfirmed_token_reservation"] = reservation
            if not error:
                entry["status"] = "completed"
                break
            entry["status"] = "failed"
            # Do not replay partial actions, auth failures or unknown token
            # spend. Retry at most twice, only a rejected request with no usage.
            if error not in {"http_429", "http_5xx"} or answer or sum(spent.values()) or attempt == 2:
                break
            time.sleep(1)
        if entry["status"] == "running":
            entry["status"] = "failed"
    except Exception as e:
        entry["status"] = "failed"
        shadow["errors"].append("harness_" + type(e).__name__)
        if shadow.get("request_started"):
            shadow["unconfirmed_token_reservation"] = shadow.get("token_reservation", 0)
    except BaseException:
        entry["status"] = "failed"
        shadow["errors"].append("interrupted")
        if shadow.get("request_started"):
            shadow["unconfirmed_token_reservation"] = shadow.get("token_reservation", 0)
        raise
    finally:
        shadow["wall_seconds"] = round(time.monotonic() - total_started, 3)
        shadow["decisions"] = jsonl(artifact / "decisions.jsonl")
        attempted = {b.get("id") for b in shadow["blocks"] if b.get("type") == "tool_use"}
        recorded = {d.get("tool_use_id") for d in shadow["decisions"]}
        if attempted - recorded:
            entry["status"] = "failed"
            shadow["errors"].append("missing_guard_decision")
        shadow["cost_usd"] = cost(cfg, selected, shadow["usage"])
        if shadow.get("fork_file"):
            runtime_file = path(shadow["runtime_fork_file"]) if shadow.get("runtime_fork_file") else None
            shadow["fork_evidence"] = fork_evidence(rows[:start], path(shadow["fork_file"]), runtime_file)
        entry["diff"] = diff(fable, shadow["blocks"])
        after_size = source.stat().st_size
        shadow["source_integrity"] = {"before_bytes": before_size, "after_bytes": after_size,
             "prefix_sha256": before_hash, "prefix_unchanged": after_size >= before_size and fingerprint(source, before_size) == before_hash}
        if not shadow["source_integrity"]["prefix_unchanged"]:
            entry["status"] = "failed"
            shadow["errors"].append("live_source_changed")
        save(dest, entry)
    return dest, entry


def tick(cfg, args):
    root = path(cfg["work_dir"])
    source, by = (path(args.transcript), "explicit") if args.transcript else discover(cfg)
    rows = jsonl(source)
    intervals = completed_turns(rows, cfg["settle_seconds"])
    existing = ledgers(root)
    seen = {(r.get("session_id"), r.get("trigger_uuid")) for _, r in existing}
    eligible = [v for v in intervals if (rows[v[0]].get("sessionId", source.stem), rows[v[0]].get("uuid")) not in seen]
    state_file = root / "watch-state.json"
    state = json.loads(state_file.read_text()) if state_file.exists() else {}
    source_key = str(source)
    if source_key not in state and intervals:
        first = intervals[-min(args.backfill or 1, len(intervals))][0]
        state[source_key] = rows[first]["uuid"]
        save(state_file, state)
    cutoff = next((i for i, r in enumerate(rows) if r.get("uuid") == state.get(source_key)), None)
    if intervals and cutoff is None:
        raise RuntimeError("source cutoff missing; refusing to guess replay history")
    cutoff = len(rows) if cutoff is None else cutoff
    eligible = [v for v in eligible if v[0] >= cutoff]
    if args.backfill:
        eligible = eligible[-args.backfill:]
    if args.once:
        eligible = eligible[-1:]
    result = 0
    for n, interval in enumerate(eligible, len(existing)+1):
        dest, entry = run_turn(cfg, source, by, rows, interval, n, model=args.model)
        print(json.dumps({"ledger": str(dest), "status": entry["status"], "errors": entry["shadow"]["errors"], "skip_reason": entry.get("skip_reason")} ), flush=True)
        if entry["status"] == "failed":
            result = 1
        if path(cfg["kill_switch"]).exists():
            break
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--config", default="~/city/ops/shadow-eval.toml")
    p.add_argument("--once", action="store_true")
    p.add_argument("--backfill", type=int, default=0)
    p.add_argument("--transcript", help="explicit source for a canary or synthetic fixture")
    p.add_argument("--model", help="claude-opus-5-5 uses normal first-party auth")
    args = p.parse_args()
    def interrupted(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    if args.backfill < 0:
        p.error("--backfill must be nonnegative")
    try:
        cfg = load_config(args.config)
        with lock(path(cfg["work_dir"])):
            # An interrupted run is visible and never automatically replayed.
            for dest, r in ledgers(path(cfg["work_dir"])):
                if r.get("status") == "running":
                    r["status"] = "failed"
                    r["shadow"]["errors"].append("interrupted")
                    if r["shadow"].get("request_started"):
                        r["shadow"]["unconfirmed_token_reservation"] = r["shadow"].get("token_reservation", 0)
                    save(dest, r)
            while True:
                result = tick(cfg, args)
                if args.once or args.backfill:
                    return result
                time.sleep(cfg["poll_seconds"])
    except KeyboardInterrupt:
        return 130
    except (OSError, ValueError, RuntimeError) as e:
        if "cfg" in locals():
            save(path(cfg["work_dir"]) / "watch-health.json", {"last_error": type(e).__name__})
        print("shadow-watch: " + type(e).__name__ + " (details suppressed; check config and private ledger)", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
