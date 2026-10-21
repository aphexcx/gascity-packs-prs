"""Inbound/display redaction, sharing the adapter's embedded pattern list."""
import json
from pathlib import Path
import re

_RULES = json.loads(
    (Path(__file__).resolve().parents[1] / "adapter/redact/patterns.json").read_text()
)
_PATTERNS = [re.compile(rule["pattern"], re.ASCII) for rule in _RULES]


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
            # candidates such as settings.jwt.<actual JWT>.
            offset = text.find(".", start, stop) if token else stop
            if offset < 0:
                offset = stop
    matches.sort()
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
