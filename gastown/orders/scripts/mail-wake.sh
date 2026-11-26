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
if ! command -v gc >/dev/null || ! command -v python3 >/dev/null; then
    echo 'mail-wake requires gc and python3 on PATH' >&2
    exit 2
fi

# Discard bodies in the pipeline, before storing anything in a shell variable.
if ! headers=$(gc mail inbox mayor --json --city "$(pwd -P)" 2>/dev/null | jq -ces '
    def identifier: type == "string" and length > 0 and (test("[[:cntrl:]]") | not);
    select(length == 1) | .[0] | select(.ok != false)
    | (.recipients // []) as $local_addresses
    | .messages | arrays | map(
        select(type == "object" and (.read == false or .read == null))
        | select((.id | identifier) and (.from | identifier))
        | if (.subject // "" | startswith("[mail-wake]")) then {wake: .id}
          else select(.from != "mayor" and .from != "mail-wake")
            | select(.from as $from | $local_addresses | index($from) == null)
            | {id, from, created_at}
          end)
' 2>/dev/null); then
    headers=null
fi

# The order runs on Linux and macOS. Use the standard library for RFC 3339
# offsets and for a file lock that the kernel releases if the order times out.
exec python3 -I - "${1:-send}" 3<<<"$headers" <<'PY'
from datetime import datetime
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

mode = sys.argv[1]
state = Path(".gc/mail-wake.seen")
confirmation = state.with_name("mail-wake.confirmed")


def skipped(reason):
    outcome_file = os.environ.get("GC_ORDER_OUTCOME_FILE")
    if mode == "send" and outcome_file:
        Path(outcome_file).write_text(json.dumps({"outcome": "skipped", "reason": reason}), encoding="utf-8")
    print(f"mail-wake: {reason}")


with os.fdopen(3, encoding="utf-8") as stream:
    headers = json.load(stream)
if headers is None:
    skipped("inbox unavailable")
    sys.exit(0)
unread = {row["id"]: row for row in headers if "id" in row}
unread_wakes = {row["wake"] for row in headers if "wake" in row}


def old_enough(row, cutoff):
    # Go emits RFC3339Nano. datetime alone truncates the last three digits.
    stamp = re.fullmatch(r"([0-9-]+T[0-9:]+)(?:\.(\d{1,9}))?(Z|[+-]\d\d:\d\d)", str(row["created_at"]))
    if not stamp:
        return False
    seconds, fraction, zone = stamp.groups()
    try:
        ts = datetime.fromisoformat(seconds + zone.replace("Z", "+00:00"))
        ns = int(ts.timestamp()) * 10**9 + int((fraction or "").ljust(9, "0"))
        return ns <= cutoff
    except (ValueError, OverflowError, OSError):
        return False


def send(rows, confirmed_id):
    senders = ",".join(sorted({row["from"] for row in rows}))
    subject = f"[mail-wake] {len(rows)} unread mail from {senders}"
    if len(subject.encode()) > 500:
        subject = subject.encode()[:497].decode("utf-8", errors="ignore") + "..."
    result = subprocess.run(
        ["gc", "mail", "send", "mayor", "--notify", "--dedup", "mail-net", "--json", "--city", os.getcwd(),
         "--from", "controller", "-s", subject, "-m", "Archive this wake notice, then read your inbox with gc mail inbox."],
        capture_output=True,
    )
    if result.returncode:
        sys.exit("mail-wake: send failed, will retry")
    try:
        reply = json.loads(result.stdout)
    except ValueError:
        sys.exit("mail-wake: unreadable send reply, will retry")
    if not isinstance(reply, dict) or reply.get("ok") is not True:
        sys.exit("mail-wake: send failed, will retry")

    wake_id = reply.get("id")
    if not isinstance(wake_id, str) or not wake_id:
        sys.exit("mail-wake: no wake ID in send reply, will retry")
    # A timeout can leave an unread notice behind without an acknowledged
    # turn. Only the notice whose confirmation we saved can cover more mail.
    if reply.get("already_done") is True:
        if wake_id in unread_wakes and wake_id == confirmed_id:
            return wake_id, "covered by existing wake"
        skipped(f"still pending, archive wake {wake_id} to release its dedup key")
        return None
    if reply.get("notified") is True:
        return wake_id, "announced"
    sys.exit("mail-wake: turn unconfirmed, archive the wake to release its dedup key")


def read_confirmation():
    try:
        return json.loads(confirmation.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {"wake": None, "ids": []}


def append_ids(ledger, ids):
    ledger.write("".join(mail_id + "\n" for mail_id in ids))
    ledger.flush()
    os.fsync(ledger.fileno())


def process():
    try:
        now = int(os.environ["MAIL_WAKE_NOW"]) * 10**9 if "MAIL_WAKE_NOW" in os.environ else time.time_ns()
    except ValueError:
        sys.exit("mail-wake: MAIL_WAKE_NOW must be an integer Unix timestamp")
    cutoff = now - 180 * 10**9
    candidates = [row for row in unread.values() if mode == "--seed" or old_enough(row, cutoff)]
    if mode == "--check":
        seen = set(state.read_text(encoding="utf-8").split("\n")) if state.exists() else set()
        seen.update(read_confirmation()["ids"])
        print(f"mail-wake: unread={len(unread)} eligible={sum(row['id'] not in seen for row in candidates)}")
        return
    if not candidates:
        print("mail-wake: no eligible mail")
        return
    state.parent.mkdir(exist_ok=True)
    with state.open("a+", encoding="utf-8") as ledger:
        fcntl.flock(ledger, fcntl.LOCK_EX | fcntl.LOCK_NB)
        ledger.seek(0)
        receipt = read_confirmation()
        # A confirmed send and a ledger append span two stores. Replay the
        # saved batch under the lock before replacing it with a later batch.
        lines = ledger.read().split("\n")
        tail = lines.pop()
        seen = set(lines)
        if tail:
            full_id = next((mail_id for mail_id in receipt["ids"] if mail_id.startswith(tail)), tail)
            ledger.write(full_id[len(tail):] + "\n")
            seen.add(full_id)
        missing = [mail_id for mail_id in receipt["ids"] if mail_id not in seen]
        if tail or missing:
            append_ids(ledger, missing)
            seen.update(missing)
        pending = [row for row in candidates if row["id"] not in seen]
        if not pending:
            print("mail-wake: no new mail")
            return
        ids = [row["id"] for row in pending]
        outcome = "seeded"
        if mode == "send":
            sent = send(pending, receipt["wake"])
            if sent is None:
                return
            wake_id, outcome = sent
            staged = confirmation.with_suffix(".confirmed.tmp")
            with staged.open("w", encoding="utf-8") as handle:
                json.dump({"wake": wake_id, "ids": ids}, handle)
                handle.flush()
                os.fsync(handle.fileno())
            staged.replace(confirmation)
        append_ids(ledger, ids)
    print(f"mail-wake: {outcome} {len(pending)}")


try:
    process()
except BlockingIOError:
    sys.exit("mail-wake: another run is active, retry later")
except OSError as error:
    sys.exit(f"mail-wake: {error.strerror}")
PY
