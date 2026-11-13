"""Run actual released-reader recovery without switching the implementation lane.

From the repository root: python3 assets/pr-evidence/gp-49y9/round2/rollback-reader.py
Uses the current module dependency lock and pinned released Go source.
"""

import collections
import json
import os
from pathlib import Path
import subprocess
import tempfile


ROOT = Path(subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True).strip())
HERE = Path(__file__).resolve().parent
RELEASED = "d2f453b83a428546dfe7fa01dc2bd831c0b47ae0"
MAIN = "205d0aecedf944792f1e3b3fbc420b9e5ad910d8"
ENV = dict(os.environ)
for name in ("GC_BIN", "NEVER_SILENT"):
    ENV.pop(name, None)


def git(*args):
    return subprocess.check_output(["git", *args], cwd=ROOT)


def run(stage, overlay, name, package, revision, destination):
    mapping = destination / (stage + "-overlay.json")
    mapping.write_text(json.dumps({"Replace": overlay}))
    command = ["go", "test", "-overlay", str(mapping), package,
               "-run", "^" + name + "$", "-count=1", "-timeout=3m", "-json"]
    logfile = destination / (stage + ".jsonl")
    with logfile.open("w") as output:
        result = subprocess.run(command, cwd=ROOT / "gchat/adapter", env=ENV,
                                stdout=output, stderr=subprocess.STDOUT)
    counts = collections.Counter()
    failures = []
    for line in logfile.read_text().splitlines():
        try:
            row = json.loads(line)
        except ValueError:
            continue
        if row.get("Action") in ("pass", "fail", "skip"):
            kind = "package" if "Test" not in row else "subtest" if "/" in row["Test"] else "test"
            counts[kind + "_" + row["Action"]] += 1
            if row["Action"] == "fail":
                failures.append(row.get("Test", row.get("Package")))
    record = {"stage": stage, "source": revision,
              "command": " ".join(command).replace(str(destination), "<evidence>"),
              "exit": result.returncode, "counts": dict(counts), "failures": failures}
    print(json.dumps(record), flush=True)
    return record


with tempfile.TemporaryDirectory(prefix="gchat-rollback-") as temporary:
    destination = Path(temporary)
    if os.environ.get("GCHAT_ROLLBACK_EVIDENCE"):
        destination = Path(os.environ["GCHAT_ROLLBACK_EVIDENCE"]).resolve()
        destination.mkdir(parents=True, exist_ok=True)
    fixtures = destination / "fixtures"
    fixtures.mkdir(exist_ok=True)
    ENV["GCHAT_ROLLBACK_FIXTURES"] = str(fixtures)
    virtual = ROOT / "gchat/adapter/internal/inbound/rollback_evidence_test.go"
    head = git("rev-parse", "HEAD").decode().strip()
    records = [run("writer", {str(virtual): str(HERE / "rollback-export.go.txt")},
                   "TestExportRollbackLedgers", "./internal/inbound", head, destination)]
    if records[0]["exit"] == 0:
        virtual = ROOT / "gchat/adapter/internal/server/rollback_evidence_test.go"
        for stage, revision in [("current-reader", head), ("released-reader", RELEASED), ("main-reader", MAIN)]:
            overlay = {}
            if stage != "current-reader":
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
            overlay[str(virtual)] = str(HERE / "rollback-reader.go.txt")
            records.append(run(stage, overlay, "TestCoalescedLedgerCompatibility", "./internal/server", revision, destination))
    (destination / "manifest.json").write_text(json.dumps(records, indent=2) + "\n")
    if any(record["exit"] for record in records):
        raise SystemExit(1)
