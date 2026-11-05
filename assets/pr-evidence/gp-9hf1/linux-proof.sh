#!/bin/bash
# Runs INSIDE the proof container as the ordinary node user, on a copy of the
# pack at /proof/whatsapp (copied from the read-only mount at /src).
set -uo pipefail
echo "== host"
uname -srm
. /etc/os-release && echo "$PRETTY_NAME"
echo "node $(node --version); python $(python3 --version 2>&1 | cut -d' ' -f2); pytest $(python3 -m pytest --version 2>&1 | head -1); jq $(jq --version); curl $(curl --version | head -1 | cut -d' ' -f2)"
echo "user $(id -un) uid $(id -u)"
cp -R /src/whatsapp /proof/whatsapp
cd /proof/whatsapp/adapter
echo
echo "== run.sh --deps-only (a fresh box, no env file)"
start=$(date +%s%N)
HOME=/tmp/fresh-home ./run.sh --deps-only; echo "exit=$? in $(( ($(date +%s%N) - start) / 1000000 )) ms"
echo
echo "== run.sh without its env file (supervised path)"
GC_SERVICE_SECRETS_DIR=/tmp/no-such-secrets ./run.sh >/dev/null 2>/tmp/run-stderr; echo "exit=$? ($(head -1 /tmp/run-stderr))"
echo
echo "== node --test 'test/*.test.js'"
node --test --test-reporter=spec --test-concurrency=4 test/*.test.js > /tmp/node-test.txt 2>&1; rc=$?
grep -E '^ℹ (tests|suites|pass|fail|cancelled|skipped|todo)' /tmp/node-test.txt
echo "exit=$rc"
grep -E '^✖' /tmp/node-test.txt | head -20
echo
echo "== python3 -m pytest whatsapp/tests -q"
cd /proof && python3 -m pytest whatsapp/tests -q 2>&1 | tail -3; echo "exit=${PIPESTATUS[0]}"
echo
echo "== doctor rows on a clean env (fail closed) and on a fixture city"
mkdir -p /tmp/city
for row in env dependencies runtime gateway deadletters held restore probes keeper; do
  out=$(env -i PATH="$PATH" HOME=/tmp GC_CITY_PATH=/tmp/city /proof/whatsapp/doctor/check-$row.sh 2>&1); rc=$?
  printf '%-12s exit=%s  %s\n' "$row" "$rc" "$out"
done
