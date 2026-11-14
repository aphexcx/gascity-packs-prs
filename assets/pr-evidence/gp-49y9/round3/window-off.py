"""Compare the same 14 acts with the window disabled against the merged parent."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True).strip())
HERE = Path(__file__).resolve().parent
PARENT = "48c4097b1e48660b3c1373572e71bd7bce44d065"
ENV = dict(os.environ)
for name in ("GC_BIN", "NEVER_SILENT"):
    ENV.pop(name, None)


def git(*args):
    return subprocess.check_output(["git", *args], cwd=ROOT)


with tempfile.TemporaryDirectory(prefix="gchat-window-off-") as temporary:
    destination = Path(os.environ.get("GCHAT_WINDOW_OFF_EVIDENCE", temporary)).resolve()
    destination.mkdir(parents=True, exist_ok=True)
    head = git("rev-parse", "HEAD").decode().strip()
    records = []
    for stage, revision in [("current", head), ("parent", PARENT)]:
        overlay = {}
        if stage == "parent":
            paths = git("ls-tree", "-r", "--name-only", revision, "gchat/adapter").decode().splitlines()
            old = {p for p in paths if p.endswith(".go")}
            if not old:
                raise RuntimeError("empty parent source inventory")
            current = {str(p.relative_to(ROOT)) for p in (ROOT / "gchat/adapter").rglob("*.go")}
            overlay = {str(ROOT / path): "" for path in current - old}
            source = destination / "parent-source"
            source.mkdir(exist_ok=True)
            for path in sorted(old):
                target = source / path.replace("/", "_")
                target.write_bytes(git("show", revision + ":" + path))
                overlay[str(ROOT / path)] = str(target)
        virtual = ROOT / "gchat/adapter/internal/inbound/window_off_evidence_test.go"
        overlay[str(virtual)] = str(HERE / "window-off.go.txt")
        mapping = destination / (stage + "-overlay.json")
        mapping.write_text(json.dumps({"Replace": overlay}))
        output = destination / (stage + "-bytes.json")
        ENV["PROBE_OUT"] = str(output)
        command = ["go", "test", "-overlay", str(mapping), "./internal/inbound", "-run",
                   "^TestProbeDiffOffIsParent$", "-count=1", "-timeout=3m", "-json"]
        logfile = destination / (stage + ".jsonl")
        with logfile.open("w") as log:
            result = subprocess.run(command, cwd=ROOT / "gchat/adapter", env=ENV,
                                    stdout=log, stderr=subprocess.STDOUT)
        record = dict(stage=stage, source=revision, exit=result.returncode,
                      command=" ".join(command).replace(str(destination), "<evidence>"),
                      sha256=hashlib.sha256(output.read_bytes()).hexdigest() if output.exists() else None)
        records.append(record)
        print(json.dumps(record), flush=True)
    (destination / "manifest.json").write_text(json.dumps(records, indent=2) + "\n")
    if any(r["exit"] for r in records) or records[0]["sha256"] != records[1]["sha256"]:
        raise SystemExit(1)
