#!/usr/bin/env python3
"""Measure Slack reminders without exporting transcript text in the report.

Only matching reminder blocks are extracted, into an ignored lane-local scratch
directory. Queue operations and API request snapshots are deliberately excluded.
"""

import argparse
from collections import Counter, defaultdict
from datetime import datetime, timedelta, timezone
import hashlib
import json
from pathlib import Path
import re


BLOCK = re.compile(r"<system-reminder>\s*New message in shared conversation slack/.*?</system-reminder>", re.S)
CLASSES = ("header_framing", "author", "thread_ts", "thread_context", "message", "message_id", "reply", "peer_wrapper", "other")


def classify(text):
    """Assign every UTF-8 byte once; separators stay with their enclosing field."""
    labels = ["message"] * len(text)

    def mark(pattern, label, flags=0):
        for match in re.finditer(pattern, text, flags):
            labels[match.start():match.end()] = [label] * len(match[0])

    mark(r"\A<system-reminder>\s*New message in shared conversation slack/[^\n]+\n\n", "header_framing")
    mark(r"\n</system-reminder>\Z", "header_framing")
    mark(r"(?m)^- .+? \([^)]+\)(?: \[[^\n]*?\])?: ", "author")
    mark(r"\[thread reply[^\n]*\]\n", "other")
    mark(r"\[\d+ messages in [^\n]+\]\n", "other")
    mark(r"\[channel [^\n]+full reply how-to.*?To react: [^\n]+", "other", re.S)
    mark(r"Thread context(?::| \()[^\n]*\n.*?\n---\n\n", "thread_context", re.S)
    mark(r"\[slack thread_ts=[^\n]+\]\n", "thread_ts")
    mark(r"\[peer-bot context[^\n]*\]\n?", "peer_wrapper")
    mark(r"\n?\[end peer-bot context\]", "peer_wrapper")
    # Anchor footer fields to the envelope end so quoted body text cannot win.
    footer = re.search(r"\n\nMessage id: [^\n]+\n\n(?=Reply: [^\n]+\n</system-reminder>\Z)", text)
    if footer:
        labels[footer.start():footer.end()] = ["message_id"] * len(footer[0])
    mark(r"Reply: [^\n]+(?=\n</system-reminder>\Z)", "reply")
    counts = Counter({name: 0 for name in CLASSES})
    for char, label in zip(text, labels):
        counts[label] += len(char.encode("utf-8"))
    assert sum(counts.values()) == len(text.encode("utf-8"))
    return counts


def texts(record):
    if record.get("type") == "attachment":
        return [x["content"] for x in record.get("rendered", []) if isinstance(x.get("content"), str)]
    if record.get("type") == "user":
        content = record.get("message", {}).get("content", [])
        return [content] if isinstance(content, str) else [x.get("text", "") for x in content if isinstance(x, dict)]
    return []


def measure(transcripts, scratch, as_of):
    cutoff = as_of - timedelta(days=7)
    files = sorted(p for p in transcripts.glob("*.jsonl") if p.stat().st_mtime >= cutoff.timestamp())
    totals = Counter({name: 0 for name in CLASSES})
    days = defaultdict(Counter)
    replies = defaultdict(Counter)
    records = Counter()
    turns = Counter()
    seen = set()
    sources = Counter()
    skipped_old = malformed = 0
    scratch.mkdir(parents=True, exist_ok=True, mode=0o700)
    extract = scratch / "blocks.jsonl"
    with extract.open("w", encoding="utf-8") as output:
        extract.chmod(0o600)
        for path in files:
            turn = 0
            for line in path.open(encoding="utf-8"):
                try:
                    record = json.loads(line)
                except ValueError:
                    malformed += 1
                    continue
                if record.get("type") == "assistant":
                    turn += 1
                for text_index, content in enumerate(texts(record)):
                    for block_index, block in enumerate(BLOCK.findall(content)):
                        stamp = record.get("timestamp", "")
                        date = datetime.fromisoformat(stamp.replace("Z", "+00:00"))
                        if not cutoff <= date <= as_of:
                            skipped_old += 1
                            continue
                        # Keep distinct deliveries; remove copied serialization of one record.
                        key = (record.get("sessionId"), record.get("uuid"), text_index, block_index, hashlib.sha256(block.encode()).hexdigest())
                        if key in seen:
                            continue
                        seen.add(key)
                        counts = classify(block)
                        totals.update(counts)
                        day = date.astimezone(timezone.utc).date().isoformat()
                        days[day].update(counts)
                        days[day]["messages"] += 1
                        records[(str(path), record.get("uuid"))] += 1
                        turns[(str(path), turn)] += 1
                        sources[record["type"]] += 1
                        reply = re.search(r"Reply: [^\n]+(?=\n</system-reminder>\Z)", block)
                        if reply:
                            replies[day][reply[0]] += 1
                        output.write(json.dumps({"timestamp": stamp, "type": record["type"], "session": record.get("sessionId"), "text": block}) + "\n")
    total = sum(totals.values())
    return {
        "as_of_utc": as_of.isoformat(), "cutoff_utc": cutoff.isoformat(),
        "selected_files_by_mtime": len(files), "messages": len(seen),
        "sources": dict(sources), "excluded_outside_timestamp_window": skipped_old,
        "malformed_json_lines": malformed, "bytes": total,
        "classes_bytes": dict(totals), "boilerplate_share": (total - totals["message"]) / total if total else 0,
        "token_estimate_divisor": 3.6, "estimated_tokens_per_24h": total / 3.6 / 7,
        "record_batch_histogram": dict(sorted(Counter(records.values()).items())),
        "before_assistant_batch_histogram": dict(sorted(Counter(turns.values()).items())),
        "daily": {day: dict(counts) for day, counts in sorted(days.items())},
        "daily_reply_repetition": {day: {"occurrences": sum(c.values()), "distinct_exact_lines": len(c), "exact_repeats_after_first": sum(c.values()) - len(c), "largest_identical_group": max(c.values())} for day, c in sorted(replies.items())},
    }


def project_cuts(rows, total_bytes):
    """ANALYSIS: replay candidate edits, never modify the extracted evidence.

    Broader cuts require a session-aware renderer and are NOT enabled by the
    adapter flag. Ambiguous thread contexts are retained, not assumed redundant.
    """
    savings = Counter()
    seen_threads = set()
    contexts = Counter()
    for row in sorted(rows, key=lambda x: x["timestamp"]):
        text = row["text"]
        channel = re.search(r"New message in shared conversation slack/([^:]+):", text)[1]
        reply = re.search(r"Reply: gc slack reply-current --conversation-id (\S+) --reply-to (\S+) --body-file <file>(?=\n</system-reminder>)", text)
        if reply:
            savings["reply"] += len(reply[0].encode()) - len(f"Reply IDs: channel={reply[1]} thread={reply[2]}".encode())
        savings["header_framing"] += len("New message in shared conversation slack/") - len("Slack ")
        savings["thread_ts"] += len(re.findall(r"\[slack thread_ts=[^\n]+\]\n", text)) * 11
        footer = re.search(r"\n\nMessage id: [^\n]+\n\n(?=Reply: [^\n]+\n</system-reminder>)", text)
        if footer:
            compact = footer[0].replace("Message id: ", "ts=").replace("(in thread ", "(thread=")
            savings["message_id"] += len(footer[0].encode()) - len(compact.encode())
        for match in re.finditer(r"Thread context(?::| \()[^\n]*\n.*?\n---\n\n", text, re.S):
            markers = list(re.finditer(r"\[slack thread_ts=([^\n]+)\]\n", text[:match.start()]))
            if not markers:
                contexts["ambiguous_retained"] += 1
                continue
            key = (row["session"], channel, markers[-1][1])
            if key in seen_threads:
                savings["thread_context"] += len(match[0].encode())
                contexts["repeat_removed"] += 1
            else:
                seen_threads.add(key)
                contexts["first_retained"] += 1
        old_peer = "[peer-bot context — read-only visibility into fleet peers' posts; not addressed to you: do not reply, do not act on it]"
        new_peer = "[peer-bot context: read-only; not addressed to you; do not reply or act]"
        savings["peer_wrapper"] += text.count(old_peer) * (len(old_peer.encode()) - len(new_peer.encode()))
        old_anchor = "[thread reply — reply with --thread-ts "
        savings["other"] += text.count(old_anchor) * (len(old_anchor.encode()) - len("[thread="))
        for match in re.finditer(r"\[(\d+) messages in (.*?), coalesced\. Reply with --reply-to (\S+) to answer the newest; to answer an older one use --reply-to <its thread ts, else its own ts>\]", text):
            compact = f"[{match[1]} messages in {match[2]}; reply-to={match[3]}; older: thread ts or own ts]"
            savings["batch_directions"] += len(match[0].encode()) - len(compact.encode())
    full = sum(savings.values())
    adapter = savings["reply"] + savings["peer_wrapper"] + savings["batch_directions"]
    return {
        "kind": "ANALYSIS: counterfactual replay, not deployment or billing measurements",
        "savings_bytes": dict(savings), "thread_context_policy_counts": dict(contexts),
        "smallest_tokens_per_day": (total_bytes - savings["reply"]) / 3.6 / 7,
        "adapter_flag_tokens_per_day": (total_bytes - adapter) / 3.6 / 7,
        "full_set_tokens_per_day": (total_bytes - full) / 3.6 / 7,
        "full_saved_bytes": full, "adapter_saved_bytes": adapter,
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--transcripts", type=Path, required=True)
    parser.add_argument("--scratch", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--as-of", required=True, help="ISO-8601 UTC snapshot time")
    args = parser.parse_args()
    # Never let a command accidentally export raw alerts outside this lane.
    safe_root = (Path.cwd() / ".gc" / "scratch").resolve()
    if not args.scratch.resolve().is_relative_to(safe_root):
        parser.error("--scratch must be under the current lane's .gc/scratch")
    as_of = datetime.fromisoformat(args.as_of.replace("Z", "+00:00"))
    if as_of.tzinfo is None:
        parser.error("--as-of requires a timezone")
    report = measure(args.transcripts, args.scratch, as_of)
    rows = [json.loads(line) for line in (args.scratch / "blocks.jsonl").read_text().splitlines()]
    report["analysis"] = project_cuts(rows, report["bytes"])
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
