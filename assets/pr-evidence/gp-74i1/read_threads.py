"""Fetch bounded thread history, retaining only attribution and timing."""
import datetime as dt
import json
import pathlib
import subprocess

HERE = pathlib.Path(__file__).parent
rows = json.loads((HERE / "correlation.json").read_text())["alerts"]
groups = {}
for row in rows:
    groups.setdefault((row["channel"], row["thread_ts"] or row["message_ts"]), []).append(row)
with (HERE / "thread-reads.manifest").open("w") as manifest:
    for (channel, thread), group in sorted(groups.items()):
        epoch = lambda s: dt.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()
        oldest = min(epoch(r["delivered_at"]) for r in group) - 60
        newest = max(epoch(r["ts"]) for r in group) + 60
        command = ["gc", "slack", "read", "--conversation-id", channel, "--thread-ts", thread,
                   "--oldest", str(oldest), "--newest", str(newest), "--limit", "200", "--no-names", "--json"]
        result = subprocess.run(command, capture_output=True, text=True)
        manifest.write(" ".join(command) + " exit=" + str(result.returncode) + "\n")
        manifest.flush()
        if result.returncode:
            (HERE / (channel + "-" + thread + "-error.txt")).write_text(result.stderr)
            continue
        data = json.loads(result.stdout)
        (HERE / (channel + "-" + thread + "-raw.json")).write_text(json.dumps(data))
        print(channel, thread, "type", type(data).__name__, "keys", list(data) if isinstance(data, dict) else len(data))
