#!/usr/bin/env python3
"""Run one proof command, retaining its output and completion status."""
import json
import os
import pathlib
import subprocess
import sys
import time

name, *command = sys.argv[1:]
root = pathlib.Path(os.environ.get('PROOF_EVIDENCE_DIR', '/evidence'))
root.mkdir(parents=True, exist_ok=True)
start = time.monotonic()
with (root / f'{name}.log').open('w') as output:
    result = subprocess.run(command, stdout=output, stderr=subprocess.STDOUT)
record = dict(name=name, command=command, cwd=str(pathlib.Path.cwd()),
              exit_code=result.returncode, seconds=round(time.monotonic() - start, 3))
with (root / 'completion.jsonl').open('a') as manifest:
    manifest.write(json.dumps(record) + '\n')
print(json.dumps(record))
raise SystemExit(result.returncode)
