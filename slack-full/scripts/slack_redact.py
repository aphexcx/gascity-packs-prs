"""Inbound/display redaction, sharing the adapter's embedded pattern list."""
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
            minimum = _RULES[priority].get("min_distinct", 0)
            candidate = text[start:stop].rstrip("=")
            # Only the generic base64 rule sets this floor. Specific secret
            # rules (including hex) still redact low-diversity credentials.
            if not minimum or (not candidate.isdigit() and len(set(candidate)) >= minimum):
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
    # Exempt only complete matches. Other rules and same-rule matches crossing
    # the allowed span invalidate it, so redacting a URL prefix cannot leave a
    # partial permalink on the next pass. Keep this sweep in sync with redact.go.
    exempt = set()
    for priority, allowlist in enumerate(_ALLOWLISTS):
        if not allowlist:
            continue
        for allowed in allowlist:
            b = 0
            for span in allowed.finditer(text):
                left, right = span.span()
                while b < len(matches) and -matches[b][1] <= left:
                    b += 1
                last, blocked = b, False
                while last < len(matches) and matches[last][0] < right:
                    start, neg_stop, rule = matches[last]
                    if rule != priority or start < left or -neg_stop > right:
                        blocked = True
                        break
                    last += 1
                if blocked:
                    continue
                exempt.update(range(b, last))
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
