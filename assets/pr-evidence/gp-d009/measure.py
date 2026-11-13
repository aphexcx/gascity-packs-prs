"""Count the synthetic captures with tiktoken's o200k_base encoding.

Run from this directory: python measure.py
Refresh after.json with TestMentionOnlyDeliveryMeasurements and its
SLACK_DELIVERY_MEASURE_OUTPUT environment variable. before.json was captured
with the same test against the unchanged baseline renderer.
"""

import json
from pathlib import Path

import tiktoken


ROOT = Path(__file__).resolve().parent
MESSAGE = "Please review the invented test message."
ENCODING = tiktoken.get_encoding("o200k_base")


def measure(text):
    return {"bytes": len(text.encode("utf-8")), "tokens": len(ENCODING.encode(text))}


def report():
    result = {}
    for version in ("before", "after"):
        rows = json.loads((ROOT / f"{version}.json").read_text())
        measured = {}
        for name, text in sorted(rows.items()):
            assert text.count(MESSAGE) == 1
            measured[name] = {
                **measure(text),
                "message_bytes": len(MESSAGE.encode("utf-8")),
                "other_bytes": len(text.encode("utf-8")) - len(MESSAGE.encode("utf-8")),
            }
        # Forty synthetic replies under the same root, each with a distinct
        # message ts. A newline separates deliveries. No unseen prior context
        # is invented: this models the already-participating session's thread.
        thread = "\n".join(
            rows["own_thread"].replace("9000000000.000002", f"9000000000.{i:06d}")
            for i in range(2, 42)
        )
        measured["forty_reply_thread"] = measure(thread)
        result[version] = measured
    return result


if __name__ == "__main__":
    print(json.dumps(report(), indent=2, sort_keys=True))
