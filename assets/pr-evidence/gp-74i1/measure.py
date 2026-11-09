"""Read-only baseline; persist identifiers and times, never Slack message text."""
import datetime as dt
import json
import math
import pathlib
import re
from zoneinfo import ZoneInfo

HERE = pathlib.Path(__file__).parent
UTC = dt.timezone.utc

def timestamp(value):
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))

data = json.loads((HERE / "journal-measure.json").read_text())
acts = []
log = pathlib.Path("/Users/tailor512/city/.gc/services/slack/logs/service.log")
for number, line in enumerate(log.open(), 1):
    match = re.match(r"(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) (react|publish): (.*)", line)
    if not match:
        continue
    at = dt.datetime.strptime(match[1], "%Y/%m/%d %H:%M:%S").replace(tzinfo=ZoneInfo("America/Chicago")).astimezone(UTC)
    fields = dict(re.findall(r"(op|conv|ts|reply_to|session)=([^ ]+)", match[3]))
    if match[2] == "react" and fields.get("op") != "reactions.add":
        continue
    acts.append({"at": at.isoformat(), "kind": match[2], "line": number, **fields})

alerts = []
for row in data["alerts"]:
    start, end = timestamp(row["delivered_at"]), timestamp(row["ts"])
    matching = [a for a in acts if a.get("conv") == row["channel"] and start <= timestamp(a["at"]) <= end and (
        (a["kind"] == "react" and a.get("ts") == row["message_ts"]) or
        (a["kind"] == "publish" and a.get("reply_to") == (row.get("thread_ts") or row["message_ts"]) and a.get("session") in row["target_sessions"]))]
    alerts.append({k: row.get(k) for k in ("receipt_id", "channel", "message_ts", "thread_ts", "delivered_at", "ts", "target_sessions")} | {"calls_before_alert": matching})

late = []
for row in data["late"]:
    if row["ts"][:10] not in ("2026-11-08", "2026-11-09"):
        continue
    proof = row["ingestion_evidence"]["timestamp"]
    late.append({"receipt_id": row["receipt_id"], "delivered_at": row["delivered_at"], "proof_at": proof,
                 "proof_lag_seconds": round((timestamp(proof) - timestamp(row["delivered_at"])).total_seconds(), 3),
                 "observed_lag_seconds": round((timestamp(row["ts"]) - timestamp(row["delivered_at"])).total_seconds(), 3)})
lags = sorted(r["proof_lag_seconds"] for r in late)
result = {"alerts": alerts, "late": late, "late_proof_p95_seconds": lags[math.ceil(.95 * len(lags)) - 1] if lags else None}
(HERE / "correlation.json").write_text(json.dumps(result, indent=2) + "\n")
print("alerted receipts:", len(alerts), "with calls before alert:", sum(bool(a["calls_before_alert"]) for a in alerts))
for row in alerts:
    print(row["receipt_id"], row["channel"], row["message_ts"], row["delivered_at"], "calls=" + str(len(row["calls_before_alert"])))
print("late proof lag seconds:", lags, "p95:", result["late_proof_p95_seconds"])
