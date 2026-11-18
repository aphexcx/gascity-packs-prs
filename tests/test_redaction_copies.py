"""The secret redaction's copies (PR 104 round 2, gp-w3nw).

Slack's adapter owns the redaction: slack-full/adapter/redact holds the Go
package and its rules, and slack-full/tests/fixtures/redaction/cases.json the
shared cases. Google Chat carries byte copies of the package, the rules and
the cases; WhatsApp carries the rules and the cases, read by its port of the
span algorithm. Every copy must equal its Slack source byte for byte, as
tests/test_reaction_catalog.py holds the catalog's copies.
"""

from pathlib import Path

import pytest


REPO = Path(__file__).resolve().parents[1]
SLACK = REPO / "slack-full"

# (copy, source). A copy added later is listed here and held to its source.
COPIES = (
    (REPO / "gchat" / "adapter" / "internal" / "redact" / "redact.go", SLACK / "adapter" / "redact" / "redact.go"),
    (REPO / "gchat" / "adapter" / "internal" / "redact" / "patterns.json", SLACK / "adapter" / "redact" / "patterns.json"),
    (REPO / "gchat" / "adapter" / "internal" / "redact" / "testdata" / "cases.json", SLACK / "tests" / "fixtures" / "redaction" / "cases.json"),
    (REPO / "whatsapp" / "adapter" / "src" / "redact-patterns.json", SLACK / "adapter" / "redact" / "patterns.json"),
    (REPO / "whatsapp" / "adapter" / "test" / "fixtures" / "redaction-cases.json", SLACK / "tests" / "fixtures" / "redaction" / "cases.json"),
)


@pytest.mark.parametrize("copy,source", COPIES, ids=[str(c.relative_to(REPO)) for c, _ in COPIES])
def test_each_copy_is_its_source_byte_for_byte(copy: Path, source: Path) -> None:
    assert copy.read_bytes() == source.read_bytes(), f"{copy.relative_to(REPO)} differs from {source.relative_to(REPO)}"
