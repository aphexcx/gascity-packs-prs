#!/usr/bin/env python3
"""Record pack doctor and supervised health checks inside the Linux fixture."""
import os
from pathlib import Path
import re
import subprocess
import sys
import tomllib

stage, slack_pack, wecom_pack = sys.argv[1:]
city = Path('/proof/linux-proof')


def record(name, command, cwd=city):
    return subprocess.run([sys.executable, '/evidence/record.py', f'{stage}-{name}',
                           'runuser', '-u', 'node', '--', *command], cwd=cwd).returncode


failed = False
for pack, service in ((Path(slack_pack), 'slack'), (Path(wecom_pack), 'wecom')):
    log = (city / '.gc/services' / service / 'logs/service.log').read_text()
    sockets = re.findall(r'(?:UDS|uds:)\s*(/\S+\.sock)', log)
    if not sockets:
        raise SystemExit(f'No socket in {service} startup log')
    address = sockets[-1]
    environment = ['env', f'GC_CITY_PATH={city}', f'GC_PACK_DIR={pack}',
                   f'GC_SERVICE_SOCKET={address}',
                   'GC_SLACK_ADAPTER_ENV=/evidence/slack-fixture.env']
    for entry in sorted((pack / 'doctor').glob('*/doctor.toml')):
        config = tomllib.loads(entry.read_text())
        code = record(f'{service}-doctor-{entry.parent.name}',
                      environment + [str((entry.parent / config['run']).resolve())], pack)
        failed |= code != 0
    failed |= record(f'{service}-socket-health',
                     ['curl', '--fail', '--silent', '--show-error', '--unix-socket', address,
                      'http://localhost/healthz']) != 0
    failed |= record(f'{service}-proxy-health',
                     ['curl', '--fail', '--silent', '--show-error',
                      f'http://127.0.0.1:8372/v0/city/linux-proof/svc/{service}/healthz']) != 0
record('services', ['gc', 'service', 'list'])
# Core doctor findings are kept separate from the individual pack checks.
record('city-doctor', ['env', 'GC_SLACK_ADAPTER_ENV=/evidence/slack-fixture.env',
                       f'GC_SERVICE_SOCKET={address}', 'gc', 'doctor', '--json'])
raise SystemExit(1 if failed else 0)
