"""Reduce go test -json logs to test counts and package completion records."""
import hashlib
import json
from pathlib import Path
import sys

for name in sys.argv[1:]:
    path = Path(__file__).resolve().parent / name
    raw = path.read_bytes()
    events = [json.loads(line) for line in raw.splitlines() if line.startswith(b'{')]
    packages = {}
    for event in events:
        package = event.get('Package')
        if not package:
            continue
        stats = packages.setdefault(package, {'passed': 0, 'top_level_passed': 0,
                                              'skipped': 0, 'failed': [], 'completion': None})
        action, test = event['Action'], event.get('Test')
        if test:
            if action == 'pass':
                stats['passed'] += 1
                stats['top_level_passed'] += int('/' not in test)
            elif action == 'skip':
                stats['skipped'] += 1
            elif action == 'fail':
                stats['failed'].append(test)
        elif action in ('pass', 'fail', 'skip'):
            stats['completion'] = event
    assert packages and all(s['completion'] and s['completion']['Action'] == 'pass'
                            and not s['failed'] for s in packages.values()), packages
    report = {'format': 'summary of go test -json; output events omitted',
              'raw_sha256': hashlib.sha256(raw).hexdigest(), 'raw_bytes': len(raw),
              'packages': packages}
    path.write_text(json.dumps(report, indent=2) + '\n')
    print(name, json.dumps(report))
