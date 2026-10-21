"""Inbound/display redaction, sharing the adapter's embedded pattern list."""
from bisect import bisect_left
import json
from pathlib import Path
import re

_RULES = json.loads(
    (Path(__file__).resolve().parents[1] / "adapter/redact/patterns.json").read_text()
)
_PATTERNS = [re.compile(rule["pattern"], re.ASCII) for rule in _RULES]
_ALLOWLISTS = [
    [re.compile(pattern, re.ASCII) for pattern in rule.get("allowlist", [])]
    for rule in _RULES
]


def redact_text(text: str) -> str:
    """Remove secret-shaped tokens before any truncation, storage or printing."""
    matches = []
    for priority, pattern in enumerate(_PATTERNS):
        token = "token" in pattern.groupindex
        offset = 0
        while m := pattern.search(text, offset):
            start = m.start("token") if token else m.start()
            stop = m.end("token") if token else m.end()
            matches.append((start, -stop, priority))
            # Resume at a JWT's first separator to include overlapping
            # candidates with another eyJ header in a later segment.
            if _RULES[priority]["kind"] == "bearer":
                offset = start + len("Bearer")
            else:
                offset = text.find(".", start, stop) if token else stop
            if offset < 0:
                offset = stop
    matches.sort()
    # Exempt only complete matches, and invalidate an exception if another
    # rule touches it. A redacted URL prefix must not leave a partial permalink
    # that changes on the next pass. Keep this sweep in sync with redact.go.
    exempt = set()
    starts = [m[0] for m in matches]
    for priority, allowlist in enumerate(_ALLOWLISTS):
        if not allowlist:
            continue
        blockers = [m for m in matches if m[2] != priority]
        for allowed in allowlist:
            b = 0
            for span in allowed.finditer(text):
                left, right = span.span()
                while b < len(blockers) and -blockers[b][1] <= left:
                    b += 1
                if b < len(blockers) and blockers[b][0] < right:
                    continue
                i = bisect_left(starts, left)
                while i < len(matches) and matches[i][0] < right:
                    if matches[i][2] == priority and -matches[i][1] <= right:
                        exempt.add(i)
                    i += 1
    matches = [m for i, m in enumerate(matches) if i not in exempt]
    out, end, i = [], 0, 0
    while i < len(matches):
        start, neg_end, priority = matches[i]
        stop = -neg_end
        i += 1
        while i < len(matches) and matches[i][0] < stop:
            stop = max(stop, -matches[i][1])
            i += 1
        out.append(text[end:start])
        out.append(f"[REDACTED:{_RULES[priority]['kind']}:...{text[start:stop][-4:]}]")
        end = stop
    out.append(text[end:])
    return "".join(out)
