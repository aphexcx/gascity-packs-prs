#!/usr/bin/env python3
"""Prepare private replay inputs; print only aggregate structure and byte counts.

The Go replay test renders these inputs with the production minimal renderer.
Historical reminders omit sender ids and channel names: use an 11-byte synthetic
sender id and the renderer's unresolved-channel fallback. This is a projection
of captured text, not a reconstruction of clipped or missing Slack payloads.
"""
import argparse
import json
from pathlib import Path
import re

OUTER = re.compile(
    r"\A(?P<head><system-reminder>\s*New message in shared conversation slack/(?P<channel>[^:\n]+):\n\n"
    r"- (?P<sender>.+?) \((?P<kind>[^)]+)\): )(?P<body>.*)"
    r"(?P<foot>\n\nMessage id: (?P<ts>[\d.]+)(?: \(in thread (?P<thread>[\d.]+)\))?\n\n)"
    r"Reply: [^\n]+\n</system-reminder>\Z", re.S)
BATCH = re.compile(r"\[(\d+) messages in [^\n]+\]\n")
MEMBER = re.compile(r"(?m)^\[(?P<ts>[\d.]+)\] (?P<sender>[^\n]+?)(?: \(in thread (?P<thread>[\d.]+)\))?: ")
PEERS = re.compile(r"\[peer-bot context[^\n]*\]\n.*?\[end peer-bot context\]\n*", re.S)
HELP = re.compile(r"\[channel [^\n]+full reply how-to.*?To react: [^\n]+", re.S)
CONTEXT = re.compile(r"Thread context(?::| \()[^\n]*\n.*?\n---\n\n", re.S)


def sinan(text, compact=False):
    text = re.sub(r"\[slack thread_ts=[^\n]+\]\n", "", text)
    text = text.replace("[thread reply — reply with --thread-ts ", "[reply in thread ")
    text = re.sub(r"(Thread context: \d+ earlier messages? already delivered) \(newest ts [^)]+\) — not re-quoted\.", r"\1.", text)
    if compact:
        text = re.sub(r"Reply: gc slack reply-current --conversation-id \S+ --reply-to (\S+) --body-file <file>(?=\n</system-reminder>)", r"reply-to \1", text)
    return text


def unit(body, channel, sender, ts, thread, is_bot=False):
    body = HELP.sub("", body)
    body = CONTEXT.sub("", body)
    body = re.sub(r"\[slack thread_ts=[^\n]+\]\n", "", body)
    anchor = ""
    match = re.search(r"\[(?:thread reply|reply in thread)[^\n]+\]\n", body)
    if match:
        anchor = sinan(match[0]).rstrip("\n")
        body = body[:match.start()] + body[match.end():]
    if is_bot:
        # Immediate peer delivery is itself the new payload. Retain its text;
        # buffered peer blocks accompanying a human are context to read later.
        body = re.sub(r"\[peer-bot context[^\n]*\]\n|\n?\[end peer-bot context\]", "", body)
        body = re.sub(r"\Apeer-bot .*? at ts [\d.]+: ", "", body)
    else:
        body = PEERS.sub("", body)
    return dict(channel=channel, sender=sender, sender_id="U_REPLAY_ID", ts=ts,
                thread=thread or "", body=body.strip(), anchor=anchor, is_bot=is_bot)


def prepare(rows):
    outputs = []
    counts = dict(blocks=0, members=0, batches=0, baseline_bytes=0, sinan_full_bytes=0, sinan_compact_bytes=0)
    for index, row in enumerate(rows):
        text = row["text"]
        match = OUTER.fullmatch(text)
        if not match:
            raise ValueError(f"unrecognized envelope at row {index}; no text exported")
        g = match.groupdict()
        body = g["body"]
        batch = BATCH.search(body)
        if batch:
            body = PEERS.sub("", HELP.sub("", body))
            members = list(MEMBER.finditer(body))
            if len(members) != int(batch[1]):
                raise ValueError(f"batch member count mismatch at row {index}")
            units = []
            for i, m in enumerate(members):
                end = members[i+1].start() if i+1 < len(members) else len(body)
                units.append(unit(body[m.end():end], g["channel"], m["sender"], m["ts"], m["thread"]))
            counts["batches"] += 1
        else:
            units = [unit(body,g["channel"],g["sender"],g["ts"],g["thread"],g["kind"]=="agent")]
        counts["blocks"] += 1
        counts["members"] += len(units)
        counts["baseline_bytes"] += len(text.encode())
        counts["sinan_full_bytes"] += len(sinan(text).encode())
        counts["sinan_compact_bytes"] += len(sinan(text,True).encode())
        suffix = g["foot"] + "reply-to " + (g["thread"] or g["ts"]) + "\n</system-reminder>"
        outputs.append(dict(head=g["head"], foot=suffix, units=units))
    return outputs, counts


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rows", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    root = (Path.cwd()/".gc/scratch").resolve()
    if not args.output.resolve().is_relative_to(root):
        parser.error("--output must stay in this lane's .gc/scratch")
    rows = [json.loads(line) for line in args.rows.read_text().splitlines()]
    outputs, counts = prepare(rows)
    with args.output.open("x",encoding="utf-8") as f:
        args.output.chmod(0o600)
        for row in outputs:
            f.write(json.dumps(row)+"\n")
    print(json.dumps(counts,indent=2))


if __name__ == "__main__":
    main()
