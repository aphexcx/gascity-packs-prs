# Linux proof: slack-full and wecom, 2026-10-27

Taylor requested: “Can you package both your slack and wecom/WeChat adapter
just like you did the Google chat adapter for us to match on Linux GC
@Sterling (Citadel Mayor)” (Slack `C0CACRS0162`, timestamp
`1793115369.506179`). Both adapters were already packs; this work measures
their Linux installation and fills the WeCom diagnostic/test gap.

The mayor confirmed that no Linux hostname or test credentials were available
and approved local readiness/test evidence with live round trips pending
(`ci-wisp-l3dzux5`). The mayor also amended delivery to **one draft PR on
branch `gp-g9nc`, with one commit per pack**, to preserve the single-branch
worker contract (`ci-wisp-w7iuj4j`). Merge and the final cross-model read remain
with the mayor and Afik.

## Host and isolation

| Fact | Measured value |
| --- | --- |
| Host | Disposable Docker container `gp-g9nc-proof`, OrbStack on citadel |
| OS | Debian GNU/Linux 12 (Bookworm), ARM64 |
| Kernel | `7.2.12-orbstack-00394-gbecdd6130068`, aarch64 |
| Docker server | 29.8.2 |
| gc source | `92a7f383ff69a2b9a995239a6612fe0735c6f09d` from the fork |
| gc build | Fork `make build`, native Linux; Go auto-selected 1.26.6 |
| Pack Go | `go1.26.5 linux/arm64` |
| Node | `pnpm exec node --version`: `v22.23.4` |
| Package managers | Corepack 0.36.0; pnpm 10.18.3 |
| Python | 3.11.2; pytest from `/usr/lib/python3/dist-packages/pytest` |
| systemd | 252 installed; user manager offline in this container |

The scratch city `/proof/linux-proof` has no agents or rigs and uses the
file beads provider, avoiding unrelated Dolt/bd requirements. Tests run as
the ordinary `node` user. Only source archives and this bead's evidence
directory were mounted into the container. No host credentials, production
Slack app, live WeCom bot, or live city were used.

The Slack fixture has fake credentials, Socket Mode disabled, and only a
loopback Events listener. WeCom has fake credentials and
`WECOM_WS_URL=ws://127.0.0.1:9`; its expected connection refusal is retained in
service logs. Both disable self-registration. These fixtures establish local
process/socket readiness; they do not establish provider authentication.

## Results and fixes

| Check | Before / after |
| --- | --- |
| Slack env doctor with explicit env file | Exit 2, four names falsely missing → exit 0 after honoring `GC_SLACK_ADAPTER_ENV` |
| Slack adapter | `adapter/run.sh` rebuilt the missing binary with Linux Go, then gc reported `ready` |
| Slack CLI | Native `go build -o gc-slack-cli .` succeeds |
| Slack Python suite | Real materialized Git cache: **858 passed, 2 skipped**; skips require root |
| Slack adapter race suite | **1,404 top-level tests pass**, one skipped; including subtests, 12,737 pass events |
| Slack CLI race suite | **199 top-level tests pass**; including subtests, 237 pass events |
| WeCom cold dependencies | `./run.sh --deps-only`: exit 0 in **1.252 s**; marker matches committed lockfile |
| WeCom Node suite | 269 pass, 2 fail, 100 cancelled, 14 skipped initially → **385 pass**, no failures/cancellations/skips |
| WeCom pack/doctor tests | **16 passed** |
| Individual pack doctors | Slack 5/5 and WeCom 4/4 checks exit 0; Slack Funnel check explicitly skips when Tailscale is absent |
| Both services | gc-supervised `proxy_process`, `ready`; direct Unix socket and gc-proxied `/healthz` both HTTP 200 |
| City doctor | 88 pass, zero failures, one advisory warning: no `bd` for `types.custom` in the file-backed scratch city |
| systemd unit | `systemd-analyze verify`: exit 0; user-service execution not available in this container |

The WeCom fixture cancellations came from production timers that call
`unref()`. A real WebSocket keeps the adapter alive; the isolated test fixtures
had no such handle. Bounded per-test handles now allow those timers to fire.
No adapter protocol or message-handling code changed. The two permission
failures disappeared when running as the ordinary user; installing `jq`
enabled the previously skipped publish CLI cases.

The initial Slack runs also exposed proof-harness errors: root bypasses
permission failures, and an archive without Git metadata cannot satisfy a
test that checks the launcher is tracked. The final Python run uses gc's
actual remote Git cache. These were harness corrections, not pack defects.

No unsupported-engine warning occurred. `node --test .` failed before test
discovery on Node 22; the correct invocation is `pnpm exec node --test` from
the enumerated `adapter/test` directory.

## Reproduction

The complete commands, durations, exit codes, and outputs are in
[`assets/pr-evidence/gp-g9nc`](../../assets/pr-evidence/gp-g9nc/).
`completion.jsonl` is the completion manifest. Large test logs are retained
as `.log.gz`; use `gzip -dc` to read them. The Dockerfile, fixture config,
fake env files, recorder, and health/doctor runner are checked in there.

Build the disposable image and the gc source snapshot (the fork's own
Makefile handles ICU and version metadata):

```sh
docker build -t gp-g9nc-linux-proof -f assets/pr-evidence/gp-g9nc/Dockerfile .
# Inside the container, with the recorded gc snapshot at /proof/gascity:
cd /proof/gascity
make build COMMIT=92a7f383ff69a2b9a995239a6612fe0735c6f09d DIRTY=
```

Initialize with the checked-in `city.toml`, which selects file beads and the
measured supervisor API port, then import the PR commit from the city's
ordinary-user account:

```sh
gc init --file /evidence/city.toml --no-start --skip-provider-readiness /proof/linux-proof
cd /proof/linux-proof
PACKS_REV=<reviewed-PR-commit>
gc import add https://github.com/aphexcx/gascity-packs-prs/tree/main/slack-full \
  --name slack --version "sha:$PACKS_REV"
gc import add https://github.com/aphexcx/gascity-packs-prs/tree/main/wecom \
  --name wecom --version "sha:$PACKS_REV"
gc import install
gc pack fetch
```

The first published source revision measured was
`ba0add578dc98c0f574f93b698489e206d07ee86`, verified by `git rev-parse HEAD`
in the materialized cache. `gc import install` installed all three imports
(core, Slack, WeCom) successfully. `gc pack fetch` only handles legacy named
packs: before the imports were installed it requested `gc import install`;
afterwards it exited 0 with “No remote packs configured.” That command alone
is not an install proof for v2 imports.

The first STANDARD Codex gate returned `VERDICT: CLEAN` with three minor
findings. All were addressed: the Slack doctor now matches the launcher's
empty default when HOME/XDG_CONFIG_HOME are missing; socket test fixtures use
a short temporary path; and this proof/evidence is included in the records
commit. The resulting pack code revision is
`80159ec24438527a803265761bc79f201762c7bd`.

Set `SLACK_PACK` and `WECOM_PACK` to those materialized pack directories.
Pre-warm dependencies and binaries while the scratch supervisor is stopped:

```sh
(cd "$WECOM_PACK/adapter" && ./run.sh --deps-only)
(cd "$SLACK_PACK/adapter" && go build -o gc-slack-adapter .)
(cd "$SLACK_PACK/cli" && go build -o gc-slack-cli .)
GC_SLACK_ADAPTER_ENV=/evidence/slack-fixture.env gc start
gc service list
```

Place `wecom-fixture.env` at
`/proof/linux-proof/.gc/services/wecom/secrets/env` with mode 0600 and ownership
matching the city user before starting. The live Linux recipes in each README
explain the actual credential setup and default route. To reproduce only this
credential-free readiness proof, retain the fake env files unchanged.

Exact test invocations, from each respective directory:

```sh
# slack-full/
python3 -m pytest tests -q
# slack-full/adapter/ and slack-full/cli/, separately
go test -race -json ./...
# wecom/adapter/test/
pnpm exec node --test
# wecom/
python3 -m pytest tests -q
```

`python3 /evidence/measure.py LABEL "$SLACK_PACK" "$WECOM_PACK"` discovers
the current sockets from their startup logs, runs each pack doctor, checks
direct and proxied health, and captures `gc doctor --json`. It writes a result
for each command to the completion manifest. The scratch service manager is
gc's detached supervisor because no systemd user manager is running. The
concrete unit in the evidence directory passed static systemd validation.

## Remaining live proof

Slack has local HTTP fixture tests, but no runtime dry mode. A real inbound
and outbound round trip awaits a dedicated test workspace: import
`slack-full/manifest/app.json`, install the app, and put its bot token,
workspace ID, and signing secret in the env file. Socket Mode additionally
uses an app-level token; Events API mode needs a Request URL. The test suite
and healthy local service are reported separately from live delivery.

WeCom's live round trip awaits a test bot's `WECOM_BOT_ID` and
`WECOM_BOT_SECRET`. Its SDK fixture suite plus socket `/healthz` is the
explicitly approved fallback. Taylor's eventual Linux host, x86-64, and
systemd user-service execution remain unmeasured by this ARM64 container run.
