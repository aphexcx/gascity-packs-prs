"""Mutation check for THE REVIEW RULES tests: apply each text mutation to the
fragment (or README) in a scratch clone, run the fragment's static tests, and
record whether they went red. Usage:
  python3 -I mutate.py <scratch-clone> <mutations.json> <out.txt>
Each mutation is {"name", "file", "old", "new"}; "file" must resolve to a file
inside the scratch clone, and "old" must occur in it exactly once. The clone
must be green before the first mutation. A mutation counts as KILLED only when
pytest exits 1 and names a failing fragment test; any other non-zero exit is
BROKEN, never a kill. The original bytes are written back after every run
(never a git command)."""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

TESTS = "tests/test_gc_standing_notes_fragment.py"
clone = Path(sys.argv[1]).resolve()
mutations = json.loads(Path(sys.argv[2]).read_text(encoding="utf-8"))


def run_tests() -> tuple[int, list[str], str]:
    run = subprocess.run(
        [sys.executable, "-m", "pytest", TESTS, "-q", "-p", "no:cacheprovider", "-rf"],
        cwd=clone, capture_output=True, text=True,
    )
    failed = [line.split("::", 1)[1].split(" ", 1)[0]
              for line in run.stdout.splitlines() if line.startswith(f"FAILED {TESTS}::")]
    return run.returncode, failed, run.stdout


code, failed, stdout = run_tests()
if code != 0 or failed or " passed" not in stdout:
    sys.exit(f"the clone is not green before any mutation (exit {code}); nothing was run")
out_lines = []
verdicts = {"KILLED": 0, "SURVIVED": 0, "BROKEN": 0, "SETUP-ERROR": 0}
for m in mutations:
    path = (clone / m["file"]).resolve()
    if not path.is_relative_to(clone) or not path.is_file():
        verdicts["SETUP-ERROR"] += 1
        out_lines.append(f"SETUP-ERROR {m['name']}: {m['file']!r} is not a file inside the clone")
        continue
    original = path.read_bytes()
    text = original.decode("utf-8")
    count = text.count(m["old"])
    if count != 1:
        verdicts["SETUP-ERROR"] += 1
        out_lines.append(f"SETUP-ERROR {m['name']}: old text occurs {count} times")
        continue
    try:
        path.write_bytes(text.replace(m["old"], m["new"], 1).encode("utf-8"))
        code, failed, _ = run_tests()
    finally:
        path.write_bytes(original)
    verdict = "KILLED" if code == 1 and failed else "SURVIVED" if code == 0 else "BROKEN"
    verdicts[verdict] += 1
    out_lines.append(f"{verdict} {m['name']} (exit {code}; red: {', '.join(failed) or 'none'})")
out_lines.append(f"{len(mutations)} mutations: " + ", ".join(f"{n} {v.lower()}" for v, n in verdicts.items()))
Path(sys.argv[3]).write_text("\n".join(out_lines) + "\n", encoding="utf-8")
print("\n".join(out_lines))
