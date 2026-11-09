"""Retain Slack attribution and reaction facts without text or credentials."""
import datetime as dt
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).parent.resolve()
sys.path.insert(0, str(HERE.parents[2] / "slack-full" / "scripts"))
import slack_chat_read as slack

token = slack.bot_token()
identity = slack.slack_get("auth.test", {}, token)
rows = json.loads((HERE / "correlation.json").read_text())["alerts"]
groups = {}
for row in rows:
    groups.setdefault((row["channel"], row["thread_ts"] or row["message_ts"]), []).append(row)
facts = {"bot_user_id": identity["user_id"], "bot_id": identity.get("bot_id"), "threads": [], "reactions": []}
for (channel, thread), group in sorted(groups.items()):
    epoch = lambda s: dt.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()
    oldest = min(epoch(r["delivered_at"]) for r in group) - 60
    newest = max(epoch(r["ts"]) for r in group) + 60
    messages, more = slack.fetch_replies(channel, thread, limit=200, oldest=str(oldest), newest=str(newest), token=token)
    facts["threads"].append({"channel": channel, "thread_ts": thread, "has_more": more,
        "messages": [{k: m[k] for k in ("ts", "user", "bot_id", "username", "thread_ts", "reactions") if k in m} for m in messages]})
for row in rows:
    response = slack.slack_get("reactions.get", {"channel": row["channel"], "timestamp": row["message_ts"], "full": "true"}, token)
    facts["reactions"].append({"channel": row["channel"], "message_ts": row["message_ts"], "reactions": response.get("message", {}).get("reactions", [])})
(HERE / "slack-facts.json").write_text(json.dumps(facts, indent=2) + "\n")
print("Read", len(facts["threads"]), "threads and", len(facts["reactions"]), "reaction sets; text and credentials omitted.")
