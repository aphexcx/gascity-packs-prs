#!/usr/bin/env python3
# 184th wake: Slack tail helper + TOKENS env (a token-presence test on withheld posts, never the text; DO-NOTs 257, 258) (DO-NOTs 94, 126, 240, 258, 278).
# usage: 174th-slack-tail.py CONV [THREAD_TS|-] [N] [LABEL]
# Prints the LAST N messages of a channel top or a thread as "ts | author | thread_ts | text[:420]".
# ALLOW (env, comma list of author substrings): when set, any author NOT matching prints only a mention test
#   (the peer-lane withhold of DO-NOT 278 as an allow-list; the author string itself is printed, never the text).
# WITHHOLD (env, comma list): authors matching print only the mention test (a deny-list; used when ALLOW is unset).
import json, os, subprocess, sys
from slack_redact import redact_text

conv = sys.argv[1]
th = sys.argv[2] if len(sys.argv) > 2 else '-'
n = int(sys.argv[3]) if len(sys.argv) > 3 else 3
label = sys.argv[4] if len(sys.argv) > 4 else ''
def read_page(oldest=None):
    # One gc slack read; returns (dict-or-None, stderr tail).
    # Channel mode: the newest 200 (the pack's documented semantics). Thread mode: 1000, because a 200-item thread page is
    # not contiguous (the 343rd measured 17 old replies and the newest one missing from it; pc_30cd63317fa1).
    cmd = ['gc', 'slack', 'read', '--conversation-id', conv, '--limit', ('1000' if th != '-' else '200'), '--json']
    if th != '-':
        cmd += ['--thread-ts', th]
    if oldest:
        cmd += ['--oldest', oldest]
    p = subprocess.run(cmd, cwd=os.environ.get('GC_CITY_PATH') or None, capture_output=True, text=True)
    out = p.stdout
    for start in (out.find('{'), out.find('[')):
        if start < 0:
            continue
        try:
            return json.loads(out[start:]), p.stderr
        except Exception:
            continue
    return None, p.stderr
d, err = read_page()
if d is None:
    print('=== %s (%s %s): NO JSON; stderr tail=%s' % (label, conv, th, redact_text(err).strip()[-240:].replace('\n', ' / ')))
    sys.exit(0)
msgs = d.get('messages', d) if isinstance(d, dict) else d
if not isinstance(msgs, list):
    msgs = []
if th != '-' and isinstance(d, dict):
    # 343rd (ci-fycdci item 1, pc_30cd63317fa1): a thread read returns the OLDEST 200 with has_more; page to the end
    # with --oldest (exclusive; the parent is re-included every page) and sort by ts, so 'the last N' is the thread's
    # true tail. Bounded to 25 pages; a page that adds nothing new ends the loop.
    uniq = {}
    for m in msgs:
        uniq.setdefault(str(m.get('ts')), m)   # the parent comes back once per internal page
    msgs = list(uniq.values())
    seen = set(uniq)
    pages = 1
    while d.get('has_more') and pages < 25 and msgs:
        newest = max(msgs, key=lambda m: float(m.get('ts') or 0))
        d, err = read_page(str(newest.get('ts')))
        if d is None:
            print('=== %s (%s %s): page %d NO JSON; stderr tail=%s' % (label, conv, th, pages + 1, redact_text(err).strip()[-240:].replace('\n', ' / ')))
            break
        page = d.get('messages', []) if isinstance(d, dict) else []
        fresh = [m for m in page if str(m.get('ts')) not in seen]
        if not fresh:
            break
        for m in fresh:
            seen.add(str(m.get('ts')))
        msgs.extend(fresh)
        pages += 1
    msgs.sort(key=lambda m: float(m.get('ts') or 0))
allow = [w.lower() for w in os.environ.get('ALLOW', '').split(',') if w]
withhold = [w.lower() for w in os.environ.get('WITHHOLD', '').split(',') if w]
me = os.environ.get('ME_ID', 'U0BHE265YNN')
print('=== %s (%s %s) total=%d, last %d ===' % (label, conv, th, len(msgs), n))
for m in msgs[-n:]:
    a = str(m.get('author') or m.get('user') or m.get('user_id') or '')
    t = str(m.get('text') or '')
    ts = m.get('ts')
    tts = m.get('thread_ts')
    al = a.lower()
    hidden = False
    if allow:
        hidden = not any(w in al for w in allow)
    elif withhold:
        hidden = any(w in al for w in withhold)
    if hidden:
        mention = 'yes' if (me in t or '@mayor-citadel' in t.lower()) else 'no'  # 175th: the literal handle counts too (Afik's tag rode a boomtown post that wrote it as text)
        tok = ','.join(w for w in os.environ.get('TOKENS', '').split(',') if w and w.lower() in t.lower())
        print('%s | %s | [withheld author; mention=%s; len=%d; tokens=%s]' % (ts, redact_text(a), mention, len(t), redact_text(tok) or '-'))
    else:
        print('%s | %s | thread_ts=%s | %s' % (ts, redact_text(a), tts, redact_text(t)[:420].replace('\n', ' / ')))
