"""Read-only Slack sample. Persist IDs, counts and match offsets, never bodies."""
import concurrent.futures
from collections import Counter
from decimal import Decimal
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
SLACK_LINK = re.compile(r'<(https?://[^<>|]+)(?:\|([^<>]*))?>')


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
    rendered = WORKING.redact_text(text)
    pinned_rendered = PINNED.redact_text(text)
    remaining = {name: Counter(m.group() for m in URL.finditer(value))
                 for name, value in [('pinned', pinned_rendered), ('working', rendered)]}
    original_links = list(SLACK_LINK.finditer(text))
    rendered_links = list(SLACK_LINK.finditer(rendered))
    slack_links = []
    for index, link in enumerate(original_links):
        after = rendered_links[index] if index < len(rendered_links) else None
        slack_links.append({'host': urlsplit(link.group(1)).hostname,
                            'working_target_readable': bool(after and after.group(1) == link.group(1)),
                            'working_label_changed': not after or after.group(2) != link.group(2)})
    urls = []
    for match in URL.finditer(text):
        url = match.group()
        entry = {'host': urlsplit(url).hostname, 'length': len(url),
                 'working_standalone_redacted': WORKING.redact_text(url) != url}
        for name, occurrences in remaining.items():
            entry[name + '_redacted'] = occurrences[url] == 0
            if occurrences[url]:
                occurrences[url] -= 1
        urls.append(entry)
    spans = []
    for rule, pattern in zip(PINNED._RULES, PINNED._PATTERNS):
        for match in pattern.finditer(text):
            start, end = match.span('token') if 'token' in pattern.groupindex else match.span()
            spans.append({'rule': rule['kind'], 'start': start, 'end': end,
                          'distinct': len(set(text[start:end])),
                          'digits_only': text[start:end].isdigit()})
    return {'channel': channel, 'ts': message['ts'], 'thread_ts': message.get('thread_ts', ''),
            'user': message['user'], 'length': len(text), 'urls': urls, 'pinned_spans': spans,
            'pinned_changed': pinned_rendered != text,
            'working_changed': rendered != text, 'slack_links': slack_links}


def replay_selection(messages, baseline):
    """Keep the original identities and order, never substitute thread roots."""
    def key(item):
        return item['channel'], item['ts']

    requested = {key(item) for item in baseline['sample'] + baseline['live']}
    missing = sorted(requested - messages.keys())
    if missing:
        raise RuntimeError('replay missing message IDs: ' + json.dumps(missing))
    return ([messages[key(item)] for item in baseline['sample']],
            [messages[key(item)] for item in baseline['live']])


def main():
    replay = len(sys.argv) > 1 and sys.argv[1] == '--replay'
    messages = {}
    scopes = []
    if replay:
        baseline = json.loads((EVIDENCE / 'baseline.json').read_text())
        for item in baseline['sample'] + baseline['live']:
            # Slack omitted two known replies with the original 10us windows.
            # A one-second margin retrieves them; exact identities below are
            # authoritative, so extra roots/replies cannot alter the sample.
            scopes.append((item['channel'], item['thread_ts'],
                           str(Decimal(item['ts']) - 1), str(Decimal(item['ts']) + 1)))
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
    if replay:
        latest, live = replay_selection(messages, baseline)
    else:
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
    assert len(sample) == 50 and {m['ts'] for m in output['live']} == LIVE_IDS
    if replay:
        output['replay_identity_check'] = {'sample_matched': len(sample),
                                           'live_matched': len(live), 'missing': []}
    name = 'after.json' if replay else 'baseline.json'
    (EVIDENCE / name).write_text(json.dumps(output, indent=2) + '\n')
    print(json.dumps({'sample_count': len(sample), 'live_count': len(live), 'by_host': counts,
                      'live': output['live']}))


if __name__ == '__main__':
    main()
