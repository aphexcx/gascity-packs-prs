"""Record each verification command and exit status before citing its result."""
import json
from pathlib import Path
import shlex
import subprocess
import sys

evidence = Path(__file__).resolve().parent
label, *command = sys.argv[1:]
result = subprocess.run(command, capture_output=True, text=True)
(evidence / (label + '.log')).write_text(result.stdout + result.stderr)
entry = {'command': shlex.join(command), 'cwd': str(Path.cwd()),
         'exit_code': result.returncode, 'log': label + '.log'}
with (evidence / 'checks.jsonl').open('a') as manifest:
    manifest.write(json.dumps(entry) + '\n')
print(json.dumps(entry))
print((result.stdout + result.stderr)[-5000:])
sys.exit(result.returncode)
