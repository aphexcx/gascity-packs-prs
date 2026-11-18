# Test directories

At the reviewed revision, 70 directories contain tracked test files. Each row names its runner or the reason it is outside the local checks.

| Test directory | Execution |
| --- | --- |
| `.` | Local Go test in `.` and CI |
| `assets/pr-evidence/gp-2awj/round2/base-hooks/gchat/adapter/internal/inbound` | Archived comparison fixture. Exclusion in skill. |
| `assets/pr-evidence/gp-2awj/round2/base-hooks/slack-full/adapter` | Archived comparison fixture. Exclusion in skill. |
| `assets/pr-evidence/gp-g02t/minor1` | Archived comparison fixture. Exclusion in skill. |
| `board-watcher/tests` | Local pack pytest |
| `board-watcher/watcher` | Local Go test in `board-watcher/watcher` |
| `contributing-to-gascity/tests` | CI pytest |
| `discord/tests` | CI pytest |
| `follow-through/test` | Local Node test and CI |
| `follow-through/tests` | CI pytest |
| `gascity/tests` | Gas City Make pytest and CI / Shell entry invokes the Python fixture collected by Make and CI |
| `gastown/tests` | Gastown Bash loop and CI |
| `gchat/adapter/cmd/gc-gchat-adapter` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/ack` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/app` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/catchup` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/chat` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/config` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/events` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/fakes/fakegc` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/fakes/fakegoogle` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/gcclient` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/inbound` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/ledger` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/markdown` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/media` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/neversilent` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/principal` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/provenance` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/puller` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/redact` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/server` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/stopwatch` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/token` | Local Go test in `gchat/adapter` |
| `gchat/adapter/internal/wire` | Local Go test in `gchat/adapter` |
| `gchat/adapter/test/contract` | Local Go test in `gchat/adapter` |
| `gchat/adapter/test/e2e` | Local Go test in `gchat/adapter` |
| `gchat/tests` | CI pytest |
| `github/tests` | CI pytest |
| `oversight-rig/assets/scripts` | Local pack pytest |
| `oversight-rig/skills/city-executive-status/tests` | Local pack pytest |
| `oversight-rig/tests` | CI pytest |
| `pr-pipeline/tests` | CI pytest |
| `profiler/tests` | CI pytest |
| `reachable/test` | Excluded locally (archive replay timeout in isolation), CI omits suite |
| `runtime-cloudflare/runtime` | Local Go test in `runtime-cloudflare/runtime` and CI |
| `scripts/tests` | CI pytest |
| `slack-channel/adapter` | Local Go test in `slack-channel/adapter` and CI |
| `slack-channel/tests` | CI pytest |
| `slack-full/adapter` | CI only (isolated registration child selects production base). |
| `slack-full/adapter/internal/ack` | CI only (isolated registration child selects production base). |
| `slack-full/adapter/internal/neversilent` | CI only (isolated registration child selects production base). |
| `slack-full/adapter/internal/stopwatch` | CI only (isolated registration child selects production base). |
| `slack-full/adapter/redact` | CI only (isolated registration child selects production base). |
| `slack-full/cli` | Local Go test in `slack-full/cli` and CI |
| `slack-full/cli/cmd` | Local Go test in `slack-full/cli` and CI |
| `slack-full/cli/internal/state/apps` | Local Go test in `slack-full/cli` and CI |
| `slack-full/cli/internal/state/blockkit` | Local Go test in `slack-full/cli` and CI |
| `slack-full/cli/internal/state/channels` | Local Go test in `slack-full/cli` and CI |
| `slack-full/cli/internal/state/rigs` | Local Go test in `slack-full/cli` and CI |
| `slack-full/cli/internal/state/rooms` | Local Go test in `slack-full/cli` and CI |
| `slack-full/cli/internal/state/workspace` | Local Go test in `slack-full/cli` and CI |
| `slack-full/tests` | CI pytest |
| `slack-mini/adapter` | Local Go test in `slack-mini/adapter` and CI |
| `tests` | Gas City Make pytest and CI |
| `wecom/adapter/test` | Excluded locally (SDK absent), CI omits suite |
| `wecom/tests` | Local pack pytest |
| `whatsapp/adapter/test` | Local Node test using tracked filenames |
| `whatsapp/gateway/test` | Local Vitest |
| `whatsapp/tests` | CI pytest |
