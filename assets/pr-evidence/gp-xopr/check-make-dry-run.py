"""Check that `make -n test` never executes an interpreter, using inert probes."""
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

makefile = pathlib.Path(sys.argv[1]).resolve()
make = shutil.which('make')
with tempfile.TemporaryDirectory(prefix='make-dry-run-') as directory:
    root = pathlib.Path(directory)
    (root / 'Makefile').write_bytes(makefile.read_bytes())
    (root / 'reachable/test').mkdir(parents=True)
    binaries = root / 'bin'
    binaries.mkdir()
    trace = root / 'executions'
    for name in ('node', 'python3', 'uv'):
        binary = binaries / name
        binary.write_text('#!/bin/sh\nprintf "called\\n" >> "$TRACE_FILE"\nexit 0\n')
        binary.chmod(0o755)
    env = {**os.environ, 'PATH': str(binaries) + os.pathsep + os.environ['PATH'], 'TRACE_FILE': str(trace)}
    result = subprocess.run([make, '-n', 'test'], cwd=root, env=env, capture_output=True, text=True, timeout=10)
    executions = len(trace.read_text().splitlines()) if trace.exists() else 0
    print(f'make -n test: exit={result.returncode}; interpreter executions={executions}')
    assert result.returncode == 0, 'dry run failed'
    assert executions == 0, 'dry run executed an interpreter'
