#!/usr/bin/env bash
set -euo pipefail
umask 077

case "$#:${1-}" in
    0:|1:--check|1:--seed) ;;
    *) echo 'usage: mail-wake.sh [--check | --seed]' >&2; exit 2 ;;
esac
if ! command -v jq >/dev/null; then
    echo 'mail-wake requires jq on PATH' >&2
    exit 2
fi

# Discard bodies in the pipeline, before storing anything in a shell variable.
if ! headers=$(gc mail inbox mayor --json 2>/dev/null | jq -ces '
    def identifier: type == "string" and length > 0 and (test("[[:cntrl:]]") | not);
    select(length == 1) | .[0] | select(.ok != false)
    | (.recipients // []) as $local_addresses
    | .messages | arrays | map(
        select(type == "object" and (.read == false or .read == null))
        | select((.id | identifier) and (.from | identifier))
        | select(.from != "mayor" and .from != "mail-wake")
        | select(.from as $from | $local_addresses | index($from) == null)
        | select((.subject // "" | startswith("[mail-wake]")) | not)
        | {id, from, created_at})
' 2>/dev/null); then
    echo 'mail-wake: inbox unavailable'
    exit 0
fi

# The order runs on Linux and macOS. Use the standard library for RFC 3339
# offsets and for a file lock that the kernel releases if the order times out.
exec python3 - "${1:-send}" 3<<<"$headers" <<'PY'
from datetime import datetime
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
import time

mode = sys.argv[1]
state = Path(".gc/mail-wake.seen")
with os.fdopen(3) as stream:
    unread = {row["id"]: row for row in json.load(stream)}


def old_enough(row, cutoff):
    try:
        ts = datetime.fromisoformat(row["created_at"].replace("Z", "+00:00"))
        return ts.tzinfo is not None and ts.timestamp() <= cutoff
    except (AttributeError, ValueError, OverflowError, OSError):
        return False


def send(rows):
    senders = ",".join(sorted({row["from"] for row in rows}))
    subject = f"[mail-wake] {len(rows)} unread mail from {senders}"
    result = subprocess.run(
        ["gc", "mail", "send", "mayor", "--notify", "--dedup", "mail-net", "--json",
         "--from", "mail-wake", "-s", subject, "-m", "Please read your inbox with gc mail inbox."],
        capture_output=True,
    )
    if result.returncode == 0:
        try:
            reply = json.loads(result.stdout)
        except (ValueError, UnicodeError):
            reply = None
        if isinstance(reply, dict) and reply.get("ok") is True:
            return "covered by existing wake" if reply.get("already_done") is True else "announced"
    sys.exit("mail-wake: send failed, will retry")


def process():
    now = float(os.environ["MAIL_WAKE_NOW"]) if "MAIL_WAKE_NOW" in os.environ else time.time()
    candidates = [row for row in unread.values() if mode == "--seed" or old_enough(row, now - 180)]
    if mode == "--check":
        seen = set(state.read_text().splitlines()) if state.exists() else set()
        print(f"mail-wake: unread={len(unread)} eligible={sum(row['id'] not in seen for row in candidates)}")
        return
    if not candidates:
        print("mail-wake: no eligible mail")
        return
    state.parent.mkdir(exist_ok=True)
    with state.open("a+", encoding="utf-8") as ledger:
        fcntl.flock(ledger, fcntl.LOCK_EX | fcntl.LOCK_NB)
        ledger.seek(0)
        seen = {line.rstrip("\n") for line in ledger}
        pending = [row for row in candidates if row["id"] not in seen]
        if not pending:
            print("mail-wake: no new mail")
            return
        outcome = "seeded" if mode == "--seed" else send(pending)
        ledger.write("".join(row["id"] + "\n" for row in pending))
    print(f"mail-wake: {outcome} {len(pending)}")


try:
    process()
except BlockingIOError:
    sys.exit("mail-wake: another run is active, retry later")
except OSError as error:
    sys.exit(f"mail-wake: {error.strerror}")
PY
