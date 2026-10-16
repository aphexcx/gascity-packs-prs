#!/usr/bin/env python3
"""Record or summarize a normal worker's typed close trail (Beads 1.3.1+)."""
from __future__ import annotations

import argparse
import base64
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

SCHEMA = "gc.worker-close.v1"
SOURCE = "gc-worker-close/v1/"
OUTCOMES = ("shipped", "no-op", "blocked", "abandoned")
GC_BIN = os.environ.get("GC_BIN") or "gc"


def nonempty(value: str) -> str:
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 for c in value):
        raise ValueError("expected nonempty text without control characters")
    return value


def sha(value: str) -> str:
    if not re.fullmatch(r"[0-9a-f]{40}", value):
        raise ValueError("commit must be a full 40-character lowercase SHA")
    return value


def pr_number(value: str) -> int:
    match = re.fullmatch(r"https://github\.com/[^/\s]+/[^/\s]+/pull/([1-9][0-9]*)", value)
    if not match:
        raise ValueError("PR must be a canonical https://github.com/OWNER/REPO/pull/N URL")
    return int(match[1])


def fact(kind: str, ref_kind: str, ref: str, name: str, **fields) -> dict:
    nonempty(ref)
    if len(ref) > 255:
        raise ValueError("provenance refs must be at most 255 characters")
    return dict(kind=kind, ref_kind=ref_kind, ref=ref,
                payload=dict(schema=SCHEMA, fact=name, **fields))


def close_facts(args) -> list[dict]:
    """Validate the entire input before the first append."""
    if args.outcome == "shipped" and not args.commit:
        raise ValueError("shipped code requires --commit")
    if args.commit:
        sha(args.commit)
    records = []
    if args.commit:
        records.append(fact("commit", "git-sha", args.commit, "commit", sha=args.commit))
    if args.pr:
        records.append(fact("handoff", "pr", args.pr, "pr", url=args.pr, number=pr_number(args.pr)))
    if args.reviews:
        reviews = json.loads(Path(args.reviews).read_text(encoding="utf-8"))
        if not isinstance(reviews, list):
            raise ValueError("reviews must be a JSON array")
        for review in reviews:
            if not isinstance(review, dict) or review.get("stage") not in ("codex", "gate", "read"):
                raise ValueError("review stage must be codex, gate, or read")
            stage = review["stage"]
            round_number = review.get("round", 1)
            if type(round_number) is not int or round_number < 1:
                raise ValueError("review round must be a positive integer")
            records.append(fact("used", "transcript", review["ref"], "review",
                                stage=stage, round=round_number,
                                verdict=nonempty(review["verdict"]), sha=sha(review["sha"])))
    if args.evidence_dir:
        if not Path(args.evidence_dir).is_absolute():
            raise ValueError("evidence directory must be absolute")
        records.append(fact("used", "transcript", args.evidence_dir, "evidence"))
    records.append(fact("handoff", "work-id", "worker-close:" + args.outcome, "outcome",
                        outcome=args.outcome, sha=args.commit, pr=args.pr))
    return records


def record(args) -> None:
    records = close_facts(args)
    # Reusable facts deduplicate forever; outcome transitions must also retain
    # A -> B -> A. Link each changed outcome to its predecessor. An immediate
    # retry after append/metadata failure sees the same outcome and skips it.
    prior = latest_outcome(typed_facts(read_events(args.bead)))
    outcome = records[-1]["payload"]
    if prior and {k: v for k, v in prior[3].items() if k != "previous"} == outcome:
        records.pop()
    else:
        outcome["previous"] = prior[2]["id"] if prior else ""
    at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    for item in records:
        payload = json.dumps(item["payload"], sort_keys=True, separators=(",", ":"))
        source = SOURCE + base64.urlsafe_b64encode(hashlib.sha256(payload.encode()).digest()).decode().rstrip("=")
        subprocess.run([GC_BIN, "bd", "provenance", "record", "--issue", args.bead,
                        "--kind", item["kind"], "--ref-kind", item["ref_kind"],
                        "--ref", item["ref"], "--source", source, "--at", at,
                        "--payload", payload], check=True)
    # Compatibility for gascity pack 0.1.x. Remove in 0.2.0 after readers AND
    # the core work-record close gate migrate; do not drop these independently.
    command = [GC_BIN, "bd", "update", args.bead,
               "--set-metadata", "gc.work_outcome=" + args.outcome]
    for key, value in (("gc.work_commit", args.commit), ("gc.pr", args.pr)):
        if value:
            command.extend(["--set-metadata", key + "=" + value])
        else:
            command.extend(["--unset-metadata", key])
    subprocess.run(command, check=True)


def read_events(bead: str) -> list[dict]:
    # Beads accepts abbreviated IDs, including for immutable provenance appends.
    # Resolve read-only and reject aliases before reading the trail or writing.
    result = subprocess.run([GC_BIN, "bd", "show", bead, "--json"],
                            text=True, capture_output=True, check=True)
    resolved = json.loads(result.stdout)
    if (not isinstance(resolved, list) or len(resolved) != 1
            or not isinstance(resolved[0], dict) or resolved[0].get("id") != bead):
        raise ValueError(f"expected exact bead id {bead!r}; show resolved a different or invalid result")
    result = subprocess.run([GC_BIN, "bd", "provenance", "log", bead, "--json"],
                            text=True, capture_output=True, check=True)
    events = json.loads(result.stdout)
    if not isinstance(events, list):
        raise ValueError("provenance log must return a JSON array")
    return events


def typed_facts(events: list[dict]) -> list[tuple]:
    facts = []
    for event in events:
        if not isinstance(event, dict) or not event.get("source", "").startswith(SOURCE):
            continue
        try:
            payload = json.loads(event.get("payload", ""))
        except (TypeError, ValueError):
            continue
        if not isinstance(payload, dict) or payload.get("schema") != SCHEMA:
            continue
        expected = {"commit": ("commit", "git-sha"), "pr": ("handoff", "pr"),
                    "review": ("used", "transcript"), "outcome": ("handoff", "work-id")}
        if expected.get(payload.get("fact")) != (event.get("kind"), event.get("ref_kind")):
            continue
        # Parse instants, not RFC3339 strings: offsets and fractional precision vary.
        try:
            when = datetime.fromisoformat((event.get("occurred_at") or event["created_at"]).replace("Z", "+00:00"))
            if when.tzinfo is None:
                continue
        except (KeyError, TypeError, ValueError):
            continue
        facts.append((when, event.get("id", ""), event, payload))
    facts.sort(key=lambda item: item[:2])
    return facts


def latest_outcome(facts: list[tuple]) -> tuple | None:
    # bd 1.3.1 stores DATETIME at second precision. Hash/id order cannot
    # break ties: the predecessor chain identifies the final transition.
    outcomes = {item[2]["id"]: item for item in facts if item[3]["fact"] == "outcome"}
    if not outcomes:
        return None
    previous = {item[3].get("previous") for item in outcomes.values()}
    tips = set(outcomes) - previous
    if len(tips) != 1:
        raise ValueError("ambiguous outcome provenance chain")
    tip = tips.pop()
    current, seen = tip, set()
    while current:
        if current in seen or current not in outcomes:
            raise ValueError("broken outcome provenance chain")
        seen.add(current)
        current = outcomes[current][3].get("previous")
        if not isinstance(current, str):
            raise ValueError("outcome provenance requires a predecessor id")
    if len(seen) != len(outcomes):
        raise ValueError("disconnected outcome provenance chain")
    return outcomes[tip]


def summary(events: list[dict]) -> str:
    facts = typed_facts(events)
    latest = latest_outcome(facts)
    if not latest:
        return "PR unknown at unknown, gate unknown, read unknown"
    outcome = latest[3]
    commit, pr = outcome.get("sha", ""), outcome.get("pr", "")
    commits = [e for _, _, e, p in facts if p["fact"] == "commit" and e["ref"] == p.get("sha") == commit]
    prs = [p for _, _, e, p in facts if p["fact"] == "pr" and e["ref"] == p.get("url") == pr]
    verdicts = {}
    if commit and commits:
        rounds = {}
        for _, _, _, p in facts:
            if p["fact"] == "review" and p.get("sha") == commit:
                stage, round_number = p.get("stage"), p.get("round")
                if stage not in ("codex", "gate", "read") or type(round_number) is not int:
                    continue
                rounds.setdefault(stage, {}).setdefault(round_number, set()).add(p.get("verdict", "unknown"))
        for stage, values in rounds.items():
            final = values[max(values)]
            verdicts[stage] = next(iter(final)) if len(final) == 1 else "conflicting"
    line = f"PR {prs[-1]['number'] if prs else 'unknown'} at {commit if commits else 'unknown'}"
    line += f", gate {verdicts.get('gate', 'unknown')}, read {verdicts.get('read', 'unknown')}"
    if outcome.get("outcome") != "shipped":
        line = str(outcome.get("outcome", "unknown")) + "; " + line
    return line


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    writer = commands.add_parser("record", help="append known facts and compatibility keys; never closes")
    writer.add_argument("bead")
    writer.add_argument("--outcome", required=True, choices=OUTCOMES)
    writer.add_argument("--commit", default="")
    writer.add_argument("--pr", default="")
    writer.add_argument("--reviews", metavar="FILE",
                        help="path to a JSON array file: stage, round, verdict, sha, ref; [] for no reviews")
    writer.add_argument("--evidence-dir", default="")
    reader = commands.add_parser("summary", help="render typed provenance; no metadata fallback")
    reader.add_argument("bead")
    args = parser.parse_args()
    try:
        nonempty(args.bead)
        if args.bead.startswith("-") or any(c.isspace() for c in args.bead):
            raise ValueError("expected one explicit bead id")
        if args.command == "record":
            record(args)
        else:
            print(summary(read_events(args.bead)))
        return 0
    except (OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError) as exc:
        print(f"worker provenance: {exc}", file=sys.stderr)
        if isinstance(exc, subprocess.CalledProcessError):
            for diagnostic in (exc.stdout, exc.stderr):
                if diagnostic:
                    print(diagnostic.rstrip(), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
