"""Read-only Slack sample. Persist IDs, counts and match offsets, never bodies."""
import concurrent.futures
import importlib.util
import json
from pathlib import Path
import re
import subprocess
import sys
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[3]
CITY = Path('/Users/tailor512/city')
EVIDENCE = Path(__file__).resolve().parent
FOUNDERS = {'U0AN32RPBFT', 'U0AN33FNGU9'}
LIVE_IDS = {'1793017482.720239', '1792810032.204479'}
PIN = '7e3a7ea9044e70b7'
URL = re.compile(r'https?://[^\s<>|"\x27]+')


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


PINNED = load('pinned', CITY / '.gc/slack-tail-redaction' / PIN / 'slack-full/scripts/slack_redact.py')
WORKING = load('working', ROOT / 'slack-full/scripts/slack_redact.py')


def read(scope):
    channel, thread, oldest, newest = scope
    args = ['gc', 'slack', 'read', '--conversation-id', channel, '--limit', '200',
            '--no-names', '--json']
    for flag, value in [('--thread-ts', thread), ('--oldest', oldest), ('--newest', newest)]:
        if value:
            args.extend([flag, value])
    result = subprocess.run(args, capture_output=True, text=True, timeout=180)
    if result.returncode:
        raise RuntimeError(f'read failed: channel={channel} thread={thread} rc={result.returncode}')
    data = json.loads(result.stdout)
    return scope, data


def parallel(scopes):
    with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
        return list(pool.map(read, scopes))


def record(channel, message):
    text = message.get('text', '')
    urls = []
    for match in URL.finditer(text):
        url = match.group()
        urls.append({'host': urlsplit(url).hostname, 'length': len(url),
                     'pinned_redacted': PINNED.redact_text(url) != url,
                     'working_redacted': WORKING.redact_text(url) != url})
    spans = []
    for rule, pattern in zip(PINNED._RULES, PINNED._PATTERNS):
        for match in pattern.finditer(text):
            start, end = match.span('token') if 'token' in pattern.groupindex else match.span()
            spans.append({'rule': rule['kind'], 'start': start, 'end': end,
                          'distinct': len(set(text[start:end])),
                          'digits_only': text[start:end].isdigit()})
    return {'channel': channel, 'ts': message['ts'], 'thread_ts': message.get('thread_ts', ''),
            'user': message['user'], 'length': len(text), 'urls': urls, 'pinned_spans': spans,
            'pinned_changed': PINNED.redact_text(text) != text,
            'working_changed': WORKING.redact_text(text) != text}


def main():
    replay = len(sys.argv) > 1 and sys.argv[1] == '--replay'
    messages = {}
    scopes = []
    if replay:
        baseline = json.loads((EVIDENCE / 'baseline.json').read_text())
        for item in baseline['sample'] + baseline['live']:
            scopes.append((item['channel'], item['thread_ts'],
                           str(float(item['ts']) - 0.00001), str(float(item['ts']) + 0.00001)))
        pages = parallel(sorted(set(scopes)))
    else:
        # Same channel union and ten newest thread roots as founder-gap-sweep.sh.
        sweep = (CITY / 'assets/ops/mayor-tools/founder-gap-sweep.sh').read_text()
        literal = re.search(r'^CHANNELS_LITERAL="([^"]+)"', sweep, re.M).group(1).split()
        channels = sorted(set(literal) | set(json.loads(
            (CITY / '.gc/slack/inbound_liveness.json').read_text())['channels']))
        oldest = str(int(time.time()) - 7 * 86400)
        pages = parallel([(ch, '', oldest, '') for ch in channels])
        for (channel, _, _, _), page in pages:
            roots = [m['ts'] for m in page['messages'] if m.get('reply_count') and
                     m.get('thread_ts') == m['ts']]
            scopes.extend((channel, ts, oldest, '') for ts in roots[-10:])
        pages += parallel(scopes)
    for (channel, _, _, _), page in pages:
        for message in page['messages']:
            if message.get('user') in FOUNDERS:
                messages[(channel, message['ts'])] = (channel, message)
    latest = sorted(messages.values(), key=lambda item: float(item[1]['ts']), reverse=True)[:50]
    live = [item for item in messages.values() if item[1]['ts'] in LIVE_IDS]
    sample = [record(*item) for item in latest]
    counts = {}
    for item in sample:
        for url in item['urls']:
            host = counts.setdefault(url['host'], {'urls': 0, 'pinned_redacted': 0, 'working_redacted': 0})
            host['urls'] += 1
            for key in ['pinned_redacted', 'working_redacted']:
                host[key] += int(url[key])
    output = {'pinned_helper': PIN, 'scope': 'founder-gap-sweep channel union, newest ten roots per channel, seven days',
              'read_scopes': [list(scope) for scope, _ in pages],
              'sample': sample, 'live': [record(*item) for item in live], 'by_host': counts}
    name = 'after.json' if replay else 'baseline.json'
    (EVIDENCE / name).write_text(json.dumps(output, indent=2) + '\n')
    print(json.dumps({'sample_count': len(sample), 'live_count': len(live), 'by_host': counts,
                      'live': output['live']}))
    assert len(sample) == 50 and {m['ts'] for m in output['live']} == LIVE_IDS


if __name__ == '__main__':
    main()
