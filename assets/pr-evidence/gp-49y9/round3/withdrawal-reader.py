"""Exercise withdrawal ledgers with the current, released, and main readers.

Run from the repository root. GCHAT_WITHDRAWAL_EVIDENCE selects the output
directory; otherwise a temporary directory is used. No checkout is switched.
"""

import collections
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True).strip())
HERE = Path(__file__).resolve().parent
ENV = dict(os.environ)
for name in ("GC_BIN", "NEVER_SILENT"):
    ENV.pop(name, None)
READERS = [("released", "d2f453b83a428546dfe7fa01dc2bd831c0b47ae0"),
           ("main", "205d0aecedf944792f1e3b3fbc420b9e5ad910d8")]


def git(*args):
    return subprocess.check_output(["git", *args], cwd=ROOT)


def run(stage, package, test, revision, overlay, destination):
    command = ["go", "test"]
    if overlay:
        mapping = destination / (stage + "-overlay.json")
        mapping.write_text(json.dumps({"Replace": overlay}))
        command += ["-overlay", str(mapping)]
    command += [package, "-run", "^" + test + "$", "-count=1", "-timeout=10m", "-json"]
    logfile = destination / (stage + ".jsonl")
    with logfile.open("w") as output:
        result = subprocess.run(command, cwd=ROOT / "gchat/adapter", env=ENV,
                                stdout=output, stderr=subprocess.STDOUT)
    counts = collections.Counter()
    failures = []
    for line in logfile.read_text().splitlines():
        try:
            item = json.loads(line)
        except ValueError:
            continue
        if item.get("Action") in ("pass", "fail", "skip"):
            kind = "package" if "Test" not in item else "subtest" if "/" in item["Test"] else "test"
            counts[kind + "_" + item["Action"]] += 1
            if item["Action"] == "fail":
                failures.append(item.get("Test", item.get("Package")))
    record = dict(stage=stage, source=revision, exit=result.returncode,
                  command=" ".join(command).replace(str(destination), "<evidence>"),
                  counts=dict(counts), failures=failures)
    print(json.dumps(record), flush=True)
    return record


with tempfile.TemporaryDirectory(prefix="gchat-withdrawal-") as temporary:
    destination = Path(os.environ.get("GCHAT_WITHDRAWAL_EVIDENCE", temporary)).resolve()
    destination.mkdir(parents=True, exist_ok=True)
    fixtures = destination / "fixtures"
    fixtures.mkdir(exist_ok=True)
    ENV["GCHAT_WITHDRAWAL_FIXTURES"] = str(fixtures)
    head = git("rev-parse", "HEAD").decode().strip()
    records = [run("writer", "./internal/inbound", "TestWithdrawalReachesEveryStoredCopy",
                   head, {}, destination)]
    # A red writer still exports every row so the old readers' red results
    # remain observable; never skip their run just because the invariant failed.
    if len(list(fixtures.glob("*.meta.json"))) != 34:
        raise RuntimeError("writer did not export every withdrawal row")
    virtual = ROOT / "gchat/adapter/internal/server/withdrawal_evidence_test.go"
    for stage, revision in [("current", head), *READERS]:
        overlay = {}
        if stage != "current":
            paths = git("ls-tree", "-r", "--name-only", revision, "gchat/adapter").decode().splitlines()
            old = {p for p in paths if p.endswith(".go")}
            if not old:
                raise RuntimeError("empty reader source inventory")
            current = {str(p.relative_to(ROOT)) for p in (ROOT / "gchat/adapter").rglob("*.go")}
            overlay = {str(ROOT / path): "" for path in current - old}
            source = destination / (stage + "-source")
            source.mkdir(exist_ok=True)
            for path in sorted(old):
                target = source / path.replace("/", "_")
                target.write_bytes(git("show", revision + ":" + path))
                overlay[str(ROOT / path)] = str(target)
        overlay[str(virtual)] = str(HERE / "withdrawal-reader.go.txt")
        records.append(run(stage, "./internal/server", "TestWithdrawalReaderCompatibility",
                           revision, overlay, destination))
    (destination / "manifest.json").write_text(json.dumps(records, indent=2) + "\n")
    if any(r["exit"] for r in records):
        raise SystemExit(1)
