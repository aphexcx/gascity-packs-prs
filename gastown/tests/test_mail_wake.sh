#!/usr/bin/env bash
set -euo pipefail

pack=$(cd "$(dirname "$0")/.." && pwd)
script="$pack/orders/scripts/mail-wake.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
export PATH="$tmp/bin:$PATH" MAIL_WAKE_NOW=1200
export BODY_MARKER='synthetic-body-must-stay-private'

cat > "$tmp/bin/gc" <<'STUB'
#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

args = sys.argv[1:]
with open("calls", "a") as log:
    print(json.dumps(args), file=log)
if "--city" not in args or Path(args[args.index("--city") + 1]).resolve() != Path.cwd():
    sys.exit(98)
del args[args.index("--city"):args.index("--city") + 2]
if args == ["mail", "inbox", "mayor", "--json"]:
    sys.stdout.buffer.write(Path("inbox").read_bytes())
    sys.exit(int(os.environ.get("INBOX_RC", 0)))
if args[:3] == ["mail", "send", "mayor"]:
    # A city order has no mail-wake session. GC accepts its reserved
    # controller sender without requiring the operator to configure a mailbox.
    if "--from" in args and args[args.index("--from") + 1] != "controller":
        sys.exit(1)
    print(os.environ.get("SEND_JSON", '{"ok":true,"count":1}'))
    sys.exit(int(os.environ.get("SEND_RC", 0)))
sys.exit(99)
STUB
chmod +x "$tmp/bin/gc"

fail() { echo "FAIL $case_name: $*" >&2; exit 1; }
start() {
    case_name=$1
    mkdir "$tmp/$case_name"
    cd "$tmp/$case_name"
    : > calls
}
# The default fixture is exactly 200 seconds old.
mail() {
    jq -nc --arg id "$1" --arg from "${2:-worker}" --arg ts "${3:-1970-01-01T00:16:40Z}" \
        --arg sub "${4:-update}" --argjson read "${5:-false}" --arg body "$BODY_MARKER" \
        '{id:$id,from:$from,subject:$sub,created_at:$ts,read:$read,body:$body}'
}
inbox() { jq -s '{messages:.,recipients:["mayor","local-session"]}' > inbox; }
run() {
    local rc=0
    : > calls
    "$script" "$@" >output 2>&1 || rc=$?
    [[ $rc -eq ${EXPECT_RC:-0} ]] || fail "exit $rc, output $(cat output)"
    if grep -Fq "$BODY_MARKER" calls output; then fail "body escaped the inbox"; fi
    [[ $(jq -sc 'map(select(.[:4] == ["mail","inbox","mayor","--json"])) | length' calls) == 1 ]] ||
        fail "expected one inbox read"
}
calls() { [[ $(wc -l <calls) -eq $1 ]] || fail "unexpected gc calls: $(cat calls)"; }
sent() {
    calls 2
    python3 - "$1" <<'PY'
import json
from pathlib import Path
import sys
args = json.loads(Path("calls").read_text().splitlines()[1])
assert args[:3] == ["mail", "send", "mayor"]
assert "--json" in args and "--notify" in args
for flag, value in [("--dedup", "mail-net"), ("--from", "controller"), ("-s", sys.argv[1]),
                    ("-m", "Please read your inbox with gc mail inbox.")]:
    assert args[args.index(flag) + 1] == value, args
PY
}
seen() { diff -u <(printf '%s\n' "$@" | sort) <(sort .gc/mail-wake.seen) || fail "wrong seen ids"; }
no_seen() { [[ ! -s .gc/mail-wake.seen ]] || fail "unexpected seen ids"; }

start repeat
mail one | inbox
run
sent '[mail-wake] 1 unread mail from worker'
seen one
run
calls 1
seen one

start age
mail recent worker 1970-01-01T00:18:20Z | inbox
run
calls 1
no_seen
MAIL_WAKE_NOW=1300 run
sent '[mail-wake] 1 unread mail from worker'
seen recent

start boundary
{ mail exactly worker 1970-01-01T01:17:00+01:00; mail young worker 1970-01-01T00:17:00.000000001Z; } | inbox
run
sent '[mail-wake] 1 unread mail from worker'
seen exactly

start excluded
{ mail own mayor; mail alias local-session; mail net mail-wake; mail tagged worker '' '[mail-wake] previous';
  mail read worker '' update true; } | inbox
run
calls 1
no_seen

start grouped
{ mail one alpha; mail two beta; mail three alpha; mail one alpha; } | inbox
run
sent '[mail-wake] 3 unread mail from alpha,beta'
seen one two three

start suppressed
mail one | inbox
SEND_JSON='{"ok":true,"already_done":true,"count":0}' run
sent '[mail-wake] 1 unread mail from worker'
seen one

start retry
mail one | inbox
for response in '' 'bad json' '{"ok":false}'; do
    SEND_JSON="$response" EXPECT_RC=1 run
    calls 2
    no_seen
    [[ $(wc -l <output) -eq 1 ]] || fail "expected one diagnostic"
done
SEND_RC=1 EXPECT_RC=1 run
calls 2
no_seen
run
sent '[mail-wake] 1 unread mail from worker'
seen one

start preview
{ mail old; mail young worker 1970-01-01T00:19:00Z; } | inbox
run --check
calls 1
grep -Fq 'unread=2 eligible=1' output || fail "wrong preview counts"
[[ ! -e .gc ]] || fail "preview wrote state"
run --seed
calls 1
seen old young
run --seed
calls 1
seen old young
MAIL_WAKE_NOW=1500 run
calls 1

start unusable
for response in '{"messages":[]}' '' 'not-json' '{"messages":{}}'; do
    printf '%s' "$response" > inbox
    run
    calls 1
    no_seen
    [[ $(wc -l <output) -eq 1 ]] || fail "expected one diagnostic"
done
mail one | inbox
INBOX_RC=1 run
calls 1
no_seen

start dependencies
mkdir no-jq
ln -s "$(command -v bash)" no-jq/bash
rc=0
PATH="$PWD/no-jq" "$script" >output 2>&1 || rc=$?
[[ $rc == 2 ]] && grep -Fq 'jq' output || fail "missing jq: exit $rc"
calls 0

start schedule
python3 - "$pack/orders/mail-wake.toml" <<'PY'
from pathlib import Path
import sys
import tomllib
order = tomllib.loads(Path(sys.argv[1]).read_text())["order"]
assert order["scope"] == "city" and order["enabled"] is False
assert order["trigger"] == "cooldown" and order["interval"] == "3m"
assert order["exec"] == '"$ORDER_DIR/scripts/mail-wake.sh"'
PY

echo '11 mail-wake cases passed'
