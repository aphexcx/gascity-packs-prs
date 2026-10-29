"""Record the exact command, exit code and output before citing checks."""
import json
from pathlib import Path
import shlex
import subprocess
import sys

evidence = Path(__file__).resolve().parent
label, *command = sys.argv[1:]
result = subprocess.run(command, capture_output=True, text=True)
output = result.stdout + result.stderr
(evidence / (label + '.log')).write_text(output)
entry = {'command': shlex.join(command), 'cwd': str(Path.cwd()),
         'exit_code': result.returncode, 'log': label + '.log'}
with (evidence / 'checks.jsonl').open('a') as manifest:
    manifest.write(json.dumps(entry) + '\n')
print(json.dumps(entry))
print(output[-1800:])
sys.exit(result.returncode)
