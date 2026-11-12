"""The reaction catalog: every reaction with a fixed meaning, in one file (gp-bis1).

Google Chat, Slack and WhatsApp ship as separate packs, so each pack carries
the catalog as its own reactions.json. The source is gchat/reactions.json;
the other copies must equal it byte for byte. The channel texts show the
same rows (tests/test_texting_rules.py). An adapter's Jev choice can read
the rows an adapter may place, with the meaning as each option's
description; the option's name is the adapter's to choose.
"""

import json
import unicodedata
from pathlib import Path

import pytest


REPO = Path(__file__).resolve().parents[1]

SOURCE = REPO / "gchat" / "reactions.json"

# Every other copy of the catalog. A copy added later (an adapter that
# embeds the file, say) is listed here and held to the same bytes.
COPIES = (
    REPO / "slack-full" / "reactions.json",
    REPO / "whatsapp" / "reactions.json",
)

SLACK_EMOJI_NAMES = REPO / "slack-full" / "adapter" / "slack_emoji_names.go"

# The Jev choice limit, from the vendor's page
# https://docs.typesafe.ai/primitives/choice.md as read on 2026-11-12 at
# 16:40Z: "A Choice question accepts up to 255 options".
JEV_CHOICE_LIMIT = 255

FIELDS = ("emoji", "slack_name", "meaning", "adapter_may_place")

# The five of the first three rounds, with their meanings and their kind.
THE_FIVE = (
    {"emoji": "👀", "slack_name": "eyes", "meaning": "I have your message and I am on it.", "adapter_may_place": True},
    {"emoji": "🔍", "slack_name": "mag", "meaning": "I am looking this up.", "adapter_may_place": True},
    {"emoji": "👍", "slack_name": "+1", "meaning": "Understood. Nothing more needs saying.", "adapter_may_place": True},
    {"emoji": "✅", "slack_name": "white_check_mark", "meaning": "Done, and checked.", "adapter_may_place": False},
    {"emoji": "⏰", "slack_name": "alarm_clock", "meaning": "The reminder is set.", "adapter_may_place": False},
)

# Every row's kind, by its Slack name, decided here as well as in the file.
# A row true the moment a message lands may be placed by an adapter. A row
# that claims a finished fact is placed only by the mayor, once the fact is
# true. A new row must be named in one of the two sets.
TRUE_AT_ARRIVAL = frozenset(
    {
        "eyes",
        "mag",
        "+1",
        "thinking_face",
        "heart",
        "wave",
        "tada",
        "cry",
        "joy",
        "crossed_fingers",
    }
)
# An attachment can arrive unavailable (Google Chat's "unavailable" line,
# WhatsApp's "media store unavailable"), and an adapter's choice sees only
# the text, so "I have the attachment" is a claim the mayor checks first.
CLAIMS = frozenset(
    {
        "white_check_mark",
        "alarm_clock",
        "inbox_tray",
        "outbox_tray",
        "date",
        "credit_card",
        "memo",
        "warning",
    }
)

# Each Slack name's character, from Slack's own table: the "unified" field
# of iamcal/emoji-data emoji.json at commit
# 13ee711e222ea17fe537bfea953c687866f16411, the commit
# slack-full/adapter/slack_emoji_names.go is generated from. Slack places a
# reaction by its name and Google Chat and WhatsApp by its character, so a
# row whose character is not its name's would differ between the channels.
CODEPOINTS = {
    "eyes": "1F440",
    "mag": "1F50D",
    "+1": "1F44D",
    "thinking_face": "1F914",
    "heart": "2764-FE0F",
    "wave": "1F44B",
    "tada": "1F389",
    "cry": "1F622",
    "joy": "1F602",
    "crossed_fingers": "1F91E",
    "white_check_mark": "2705",
    "alarm_clock": "23F0",
    "inbox_tray": "1F4E5",
    "outbox_tray": "1F4E4",
    "date": "1F4C5",
    "credit_card": "1F4B3",
    "memo": "1F4DD",
    "warning": "26A0-FE0F",
}


def no_repeated_keys(pairs: list[tuple[str, object]]) -> dict:
    keys = [key for key, _ in pairs]
    repeated = sorted({key for key in keys if keys.count(key) > 1})
    if repeated:
        raise ValueError(f"a key appears twice in one object: {repeated}")
    return dict(pairs)


def load(path: Path = SOURCE) -> list[dict]:
    doc = json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=no_repeated_keys)
    assert isinstance(doc, dict) and isinstance(doc.get("reactions"), list), path
    return doc["reactions"]


def is_one_emoji(text: str) -> bool:
    """One pictographic symbol, with the emoji presentation selector or without.

    The same test the channel texts' scan uses to find a reaction.
    """
    return (
        len(text) in (1, 2)
        and unicodedata.category(text[0]) == "So"
        and ord(text[0]) >= 0x2190
        and text[1:] in ("", "️")
    )


def slack_names() -> set[str]:
    names = set()
    for line in SLACK_EMOJI_NAMES.read_text(encoding="utf-8").splitlines():
        if line.startswith('\t"') and line.endswith('",'):
            names.add(line[2:-2])
    return names


def test_the_copies_are_the_source_byte_for_byte() -> None:
    source = SOURCE.read_bytes()
    for copy in COPIES:
        assert copy.read_bytes() == source, f"{copy.relative_to(REPO)} differs from {SOURCE.relative_to(REPO)}"


def test_every_row_has_the_four_fields() -> None:
    for row in load():
        assert isinstance(row, dict), row
        assert tuple(row) == FIELDS, row
        assert is_one_emoji(row["emoji"]), row
        assert isinstance(row["slack_name"], str) and row["slack_name"], row
        assert isinstance(row["adapter_may_place"], bool), row
        meaning = row["meaning"]
        assert isinstance(meaning, str) and meaning == " ".join(meaning.split()), row
        assert meaning[:1].isupper() and meaning.endswith("."), row
        # The texts quote the meaning inside a table cell.
        assert not set(meaning) & set('"|`'), row


def test_emoji_slack_names_and_meanings_are_unique() -> None:
    rows = load()
    for field in ("emoji", "slack_name", "meaning"):
        values = [row[field] for row in rows]
        repeated = sorted({value for value in values if values.count(value) > 1})
        assert not repeated, f"{field} repeated: {repeated}"


def test_every_slack_name_is_one_the_slack_adapter_knows() -> None:
    known = slack_names()
    assert len(known) > 1000, "the adapter's table did not parse"
    unknown = [row["slack_name"] for row in load() if row["slack_name"] not in known]
    assert not unknown, f"Slack names the adapter does not know: {unknown}"


def test_each_emoji_is_the_character_slack_gives_its_name() -> None:
    rows = load()
    assert {row["slack_name"] for row in rows} == set(CODEPOINTS), "name each row's codepoints here"
    for row in rows:
        expected = "".join(chr(int(part, 16)) for part in CODEPOINTS[row["slack_name"]].split("-"))
        assert row["emoji"] == expected, row


def test_the_count_is_above_five_and_within_the_jev_choice_limit() -> None:
    rows = load()
    assert len(THE_FIVE) < len(rows) <= JEV_CHOICE_LIMIT, len(rows)


def test_the_five_stay_with_their_meanings() -> None:
    rows = load()
    for row in THE_FIVE:
        assert row in rows, row


def test_no_claim_is_placeable_by_an_adapter() -> None:
    """A row that claims a finished fact goes on only once the fact is true.

    An adapter places a reaction when a message lands, before anything is
    done, so it never places a claim (gp-bis1 item 3).
    """
    placeable = [row["slack_name"] for row in load() if row["adapter_may_place"] and row["slack_name"] in CLAIMS]
    assert not placeable, f"claims an adapter may place: {placeable}"


def test_every_row_has_its_kind_named_here_and_in_the_file() -> None:
    assert not TRUE_AT_ARRIVAL & CLAIMS
    rows = load()
    names = {row["slack_name"] for row in rows}
    assert names == TRUE_AT_ARRIVAL | CLAIMS, (
        f"not named here: {sorted(names - TRUE_AT_ARRIVAL - CLAIMS)}; "
        f"named here but not in the file: {sorted((TRUE_AT_ARRIVAL | CLAIMS) - names)}"
    )
    for row in rows:
        assert row["adapter_may_place"] == (row["slack_name"] in TRUE_AT_ARRIVAL), row


@pytest.mark.parametrize(
    ("bad", "why"),
    [
        ('{"reactions": [{"emoji": "👀", "emoji": "👍"}]}', "repeated key"),
    ],
)
def test_the_loader_refuses_a_repeated_key(tmp_path: Path, bad: str, why: str) -> None:
    path = tmp_path / "reactions.json"
    path.write_text(bad, encoding="utf-8")
    with pytest.raises(ValueError, match="twice"):
        load(path)


@pytest.mark.parametrize(
    ("text", "one"),
    [
        ("👀", True),
        ("❤️", True),
        ("⚠️", True),
        ("❤", True),
        ("👀👀", False),
        ("e", False),
        ("™", False),
        ("", False),
    ],
)
def test_one_emoji_means_one_symbol(text: str, one: bool) -> None:
    assert is_one_emoji(text) is one
