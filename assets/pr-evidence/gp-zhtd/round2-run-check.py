"""Run one check, recording its exit and counts before reporting results."""
import collections
import datetime
import json
import pathlib
import re
import subprocess
import sys
import time

root = pathlib.Path(__file__).resolve().parents[3]
name, *command = sys.argv[1:]
evidence = pathlib.Path(__file__).resolve().parent
scratch = root / ".gc/scratch/gp-zhtd/round2"
scratch.mkdir(parents=True, exist_ok=True)
start = time.monotonic()
result = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
(scratch / (name + ".log")).write_text(result.stdout)
counts = collections.Counter()
for line in result.stdout.splitlines():
    try:
        event = json.loads(line)
    except json.JSONDecodeError:
        continue
    if isinstance(event, dict) and event.get("Test") and event.get("Action") in ("pass", "skip", "fail"):
        counts[event["Action"] + "_including_subtests"] += 1
        if "/" not in event["Test"]:
            counts[event["Action"] + "_top_level"] += 1
if "pytest" in command:
    for count, status in re.findall(r"(\d+) (passed|skipped|failed|warnings?)", result.stdout):
        counts[status] = int(count)
report = {
    "command": command,
    "cwd": str(pathlib.Path.cwd()),
    "head": subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip(),
    "utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "exit_code": result.returncode,
    "seconds": round(time.monotonic() - start, 2),
    "test_counts": dict(counts),
}
manifest = evidence / (name + ".json")
manifest.write_text(json.dumps(report, indent=2) + "\n")
with (evidence / "round2-completion.txt").open("a") as out:
    out.write(f"{name}: {json.dumps(command)} exit_code={result.returncode}\n")
print(manifest.read_text())
if result.returncode:
    print(result.stdout[-8000:])
sys.exit(result.returncode)
