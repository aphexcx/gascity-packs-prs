"""Check replay identity selection without reading Slack."""
import importlib.util
from pathlib import Path

path = Path(__file__).resolve().parents[1] / 'measure.py'
spec = importlib.util.spec_from_file_location('measurement', path)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
baseline = {'sample': [{'channel': 'C1', 'ts': '2'}, {'channel': 'C2', 'ts': '2'}],
            'live': [{'channel': 'C1', 'ts': '1'}]}
messages = {(ch, ts): (ch, {'ts': ts}) for ch, ts in
            [('C1', '2'), ('C2', '2'), ('C1', '1'), ('C1', '99'), ('C2', '1')]}
sample, live = module.replay_selection(messages, baseline)
assert sample == [messages['C1', '2'], messages['C2', '2']]
assert live == [messages['C1', '1']]
del messages['C2', '2']
try:
    module.replay_selection(messages, baseline)
except RuntimeError as error:
    assert '["C2", "2"]' in str(error)
else:
    raise AssertionError('missing baseline identity was replaced by a different message')
print('PASS: exact channel/ts pairs and ordering; extra roots excluded; missing identity reported')
url = 'https://x.com/sampleuser/status/1234567890123456789'
record = module.record('C1', {'ts': '1', 'user': 'synthetic', 'text': '<' + url + '|' + url + '>'})
assert len(record['urls']) == 1 and record['urls'][0]['occurrences'] == 2
assert record['urls'][0]['working_redacted'] == 1
assert record['urls'][0]['pinned_redacted'] == 2
assert record['slack_links'] == [{'host': 'x.com', 'working_target_readable': True,
                                  'working_label_changed': True}]
print('PASS: grouped counts retain one readable target and one redacted label')
reversed_record = module.record('C1', {'ts': '1', 'user': 'synthetic',
    'text': '<https://example.com|' + url + '> ' + url})
x_group = next(item for item in reversed_record['urls'] if item['host'] == 'x.com')
assert x_group['occurrences'] == 2 and x_group['working_redacted'] == 1
assert x_group['pinned_redacted'] == 2
print('PASS: label-first and target-first duplicate orders have the same grouped counts')
