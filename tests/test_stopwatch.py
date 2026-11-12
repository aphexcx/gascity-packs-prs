"""Offline timing report: known samples, thread isolation, and release cuts."""
import json
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPTS = [ROOT / pack / "commands" / "stopwatch.py" for pack in ("gchat", "slack-full", "whatsapp")]


def fixture(directory):
    records = [
        {"event": "inbound", "channel": "gchat", "conversation": "space-example", "thread": "thread-example", "message": str(i), "release": "release-a", "message_at": f"2020-01-01T00:00:0{i}Z", "at": f"2020-01-01T00:00:0{i}Z"}
        for i in range(3)
    ]
    records += [
        {"event": "reaction", "channel": "gchat", "conversation": "space-example", "message": "0", "at": "2020-01-01T00:00:01Z"},
        {"event": "reaction", "channel": "gchat", "conversation": "space-example", "message": "1", "at": "2020-01-01T00:00:04Z"},
        {"event": "words", "channel": "gchat", "conversation": "space-example", "thread": "another-thread", "at": "2020-01-01T00:00:03Z"},
        {"event": "words", "channel": "gchat", "conversation": "space-example", "thread": "thread-example", "at": "2020-01-01T00:00:06Z"},
        {"event": "inbound", "channel": "gchat", "conversation": "space-example", "thread": "thread-example", "message": "later", "release": "release-b", "message_at": "2020-02-01T00:00:00Z", "at": "2020-02-01T00:00:00Z"},
    ]
    (directory / "stopwatch-2020-01.jsonl").write_text("\n".join(json.dumps(r) for r in records) + '\n{"event":')


def test_stopwatch_exact_text_json_and_copies(tmp_path):
    fixture(tmp_path)
    expected = [
        {"release": "release-a", "first_message": "2020-01-01T00:00:00Z", "messages": 3, "reaction": {"count": 2, "median_seconds": 1.0, "p90_seconds": 3.0}, "words": {"count": 3, "median_seconds": 5.0, "p90_seconds": 6.0}, "without_words_over_hour": 0},
        {"release": "release-b", "first_message": "2020-02-01T00:00:00Z", "messages": 1, "reaction": {"count": 0, "median_seconds": None, "p90_seconds": None}, "words": {"count": 0, "median_seconds": None, "p90_seconds": None}, "without_words_over_hour": 1},
    ]
    expected_text = "Release release-a\nFirst message: 2020-01-01T00:00:00Z\nMessages: 3\nFirst reaction: 2; median 1.000s; p90 3.000s\nFirst words: 3; median 5.000s; p90 6.000s\nWithout words after one hour: 0\n\nRelease release-b\nFirst message: 2020-02-01T00:00:00Z\nMessages: 1\nFirst reaction: 0; median -; p90 -\nFirst words: 0; median -; p90 -\nWithout words after one hour: 1\n"
    for script in SCRIPTS:
        assert script.read_bytes() == SCRIPTS[0].read_bytes()
        args = ["python3", str(script), "--state-dir", str(tmp_path)]
        assert json.loads(subprocess.check_output(args + ["--json"], text=True)) == expected
        assert subprocess.check_output(args, text=True) == expected_text
        cut = json.loads(subprocess.check_output(args + ["--json", "--since", "2020-01-01T00:00:01Z", "--until", "2020-01-01T00:00:03Z"], text=True))
        assert cut[0]["messages"] == 2
        assert cut[0]["words"] == {"count": 2, "median_seconds": 4.0, "p90_seconds": 5.0}


def test_go_runtime_copies_stay_identical():
    for package, files in {"ack": ("catalog.go", "choice.go", "service.go"), "stopwatch": ("stopwatch.go",)}.items():
        for file in files:
            assert (ROOT / "gchat/adapter/internal" / package / file).read_bytes() == (ROOT / "slack-full/adapter/internal" / package / file).read_bytes()
