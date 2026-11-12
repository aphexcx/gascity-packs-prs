#!/usr/bin/env python3
"""Read adapter timing journals without contacting an adapter or channel."""
import argparse
from bisect import bisect_right
from collections import defaultdict
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path


def instant(value):
    stamp = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if stamp.tzinfo is None:
        raise ValueError("timestamp requires a timezone")
    return stamp.astimezone(timezone.utc)


def default_directory():
    env = os.environ
    if env.get("GC_SERVICE_STATE_ROOT"):
        return Path(env["GC_SERVICE_STATE_ROOT"])
    pack = Path(__file__).resolve().parent.parent.name
    city = env.get("GC_CITY_PATH") or env.get("GC_CITY")
    if pack == "whatsapp":
        return Path(env.get("WHATSAPP_STATE_DIR") or str(Path(city or ".") / ".gc/whatsapp"))
    if pack == "slack-full":
        return Path(city) / ".gc/slack" if city else Path("/tmp/gc-slack-adapter")
    if env.get("GCHAT_STATE_FILE"):
        return Path(env["GCHAT_STATE_FILE"]).expanduser().parent
    # Custom env-file paths are deliberately supplied through --state-dir:
    # the reporting process never sources an adapter's credential file.
    if env.get("GC_GCHAT_ADAPTER_ENV"):
        return Path(env["GC_GCHAT_ADAPTER_ENV"]).expanduser().parent.parent
    if env.get("GC_SERVICE_SECRETS_DIR"):
        return Path(env["GC_SERVICE_SECRETS_DIR"]).expanduser().parent
    return Path.home() / ".config"


def samples(values):
    values.sort()
    def rank(fraction):
        return values[math.ceil(len(values) * fraction) - 1] if values else None
    return {"count": len(values), "median_seconds": rank(.5), "p90_seconds": rank(.9)}


def report(directory, since=None, until=None, now=None):
    now = now or datetime.now(timezone.utc)
    inbound, reactions, words = {}, {}, defaultdict(list)
    for file in sorted(directory.glob("stopwatch-*.jsonl")):
        with file.open(encoding="utf-8", errors="replace") as stream:
            for line in stream:
                try:
                    event = json.loads(line)
                    at = instant(event["at"])
                    conversation = (event["channel"], event["conversation"])
                    key = (*conversation, event.get("message", ""))
                    kind = event["event"]
                    if kind == "inbound":
                        stamp = instant(event["message_at"])
                        if (since is None or stamp >= since) and (until is None or stamp <= until):
                            inbound.setdefault(key, (event, stamp))
                    elif kind == "reaction":
                        reactions[key] = min(at, reactions.get(key, at))
                    elif kind == "words":
                        words[(*conversation, event.get("thread", ""))].append(at)
                except (ValueError, KeyError, TypeError):
                    continue
    for stamps in words.values():
        stamps.sort()
    groups = {}
    for key, (event, stamp) in inbound.items():
        release = event.get("release", "unknown")
        group = groups.setdefault(release, {"first": stamp, "messages": 0, "reaction": [], "words": [], "missing": 0})
        group["first"] = min(group["first"], stamp)
        group["messages"] += 1
        if key in reactions and reactions[key] >= stamp:
            group["reaction"].append((reactions[key] - stamp).total_seconds())
        thread = event.get("thread", "")
        replies = words[(*key[:2], thread)]
        # Slack's root post has no thread_ts. A subsequent reply can be
        # top-level, or explicitly in the thread rooted at that post.
        if event["channel"] == "slack" and not thread:
            replies = sorted(replies + words[(*key[:2], event["message"])])
        offset = bisect_right(replies, stamp)
        if offset < len(replies):
            group["words"].append((replies[offset] - stamp).total_seconds())
        elif (now - stamp).total_seconds() > 3600:
            group["missing"] += 1
    return [{"release": release, "first_message": group["first"].isoformat().replace("+00:00", "Z"),
             "messages": group["messages"], "reaction": samples(group["reaction"]), "words": samples(group["words"]),
             "without_words_over_hour": group["missing"]}
            for release, group in sorted(groups.items(), key=lambda item: (item[1]["first"], item[0]))]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state-dir", type=Path, default=default_directory())
    parser.add_argument("--since", type=instant)
    parser.add_argument("--until", type=instant)
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()
    try:
        rows = report(args.state_dir, args.since, args.until)
    except OSError:
        parser.exit(1, "stopwatch: cannot read state directory\n")
    if args.json:
        print(json.dumps(rows, indent=2))
        return
    def seconds(value):
        return "-" if value is None else f"{value:.3f}s"
    blocks = []
    for row in rows:
        lines = [f"Release {row['release']}", f"First message: {row['first_message']}", f"Messages: {row['messages']}"]
        for key, label in (("reaction", "First reaction"), ("words", "First words")):
            value = row[key]
            lines.append(f"{label}: {value['count']}; median {seconds(value['median_seconds'])}; p90 {seconds(value['p90_seconds'])}")
        lines.append(f"Without words after one hour: {row['without_words_over_hour']}")
        blocks.append("\n".join(lines))
    if blocks:
        print("\n\n".join(blocks))


if __name__ == "__main__":
    main()
