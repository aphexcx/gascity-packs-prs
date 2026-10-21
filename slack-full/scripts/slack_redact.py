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
    matches = sorted(
        (m.start("token") if "token" in pattern.groupindex else m.start(),
         -(m.end("token") if "token" in pattern.groupindex else m.end()), priority)
        for priority, pattern in enumerate(_PATTERNS)
        for m in pattern.finditer(text)
    )
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
