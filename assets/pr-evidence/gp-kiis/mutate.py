"""Mutation check for THE REVIEW RULES tests: apply each text mutation to the
fragment (or README) in a scratch clone, run the fragment's static tests, and
record whether they went red. Usage:
  python3 -I mutate.py <scratch-clone> <mutations.json> <out.txt>
Each mutation is {"name", "file", "old", "new"}; "old" must occur exactly once.
The original text is written back after every run (never a git command)."""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

clone = Path(sys.argv[1])
mutations = json.loads(Path(sys.argv[2]).read_text(encoding="utf-8"))
out_lines = []
survivors = 0
for m in mutations:
    path = clone / m["file"]
    original = path.read_text(encoding="utf-8")
    count = original.count(m["old"])
    if count != 1:
        out_lines.append(f"SETUP-ERROR {m['name']}: old text occurs {count} times")
        survivors += 1
        continue
    try:
        path.write_text(original.replace(m["old"], m["new"], 1), encoding="utf-8")
        run = subprocess.run(
            [sys.executable, "-m", "pytest", "tests/test_gc_standing_notes_fragment.py",
             "-q", "-p", "no:cacheprovider", "-rf"],
            cwd=clone, capture_output=True, text=True,
        )
    finally:
        path.write_text(original, encoding="utf-8")
    failed = [line.split("::", 1)[1].split(" ", 1)[0]
              for line in run.stdout.splitlines() if line.startswith("FAILED ")]
    verdict = "KILLED" if run.returncode != 0 else "SURVIVED"
    if run.returncode == 0:
        survivors += 1
    out_lines.append(f"{verdict} {m['name']} (exit {run.returncode}; red: {', '.join(failed) or 'none'})")
out_lines.append(f"{len(mutations)} mutations, {len(mutations) - survivors} killed, {survivors} survived")
Path(sys.argv[3]).write_text("\n".join(out_lines) + "\n", encoding="utf-8")
print("\n".join(out_lines))
