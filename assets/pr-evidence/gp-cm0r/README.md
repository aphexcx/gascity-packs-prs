# Authenticated Google client redirects

Base (START) `96dfa21b6f988c934fe434260f031e2e3277ce10`. Final code and tests `adf5c2fcdbb09f8a9c0508f7fd77c98dc4a334eb`. The evidence commit follows the tested code. All endpoint tests used fakes. Toolchain `go version go1.27.3 darwin/arm64`.

## Redirect and caller checks

| Case | Coverage | Result |
| --- | --- | --- |
| T1 | Required and lookup clients, cross-origin GET, statuses 301, 302, 303, 307 and 308 | All 10 rows returned the initial status and Location after exactly one request. |
| T2 | Same clients and statuses, same-origin GET to another path | All 10 rows returned the initial response without requesting the second path. |
| T3 | Both clients, both origins, replayable POST body, statuses 307 and 308 | All 8 rows sent the body once. |
| T4 | Required client PUT, HTTP 308 with a Range and no Location | The status and Range reached the caller. The existing `TestResumableUploadPUTChunks` also covers a partial upload, replay of a chunk and subsequent completion. |
| T5 | Optional message and member lookups, required message-space, reaction-parent and reaction-space reads | A 302 left the incoming frame intact and produced `http_302` on lookup. Each required read returned a transient failure, left the event unconsumed and requested redelivery. |

T1–T4 run in `TestAuthenticatedClientRefusesRedirect`. T5 runs in `TestSenderFillResourceStatus` and `TestLookupHoldCarriesCredentialKind`. A failed media download yields an unavailable attachment note. A redirect during replacement credential validation keeps the current credential and schedules another validation attempt. These two outcomes were traced in source.

## Baseline comparison

The initial test commit was `14d57511a813de43179af6fb995c54e75ad427b1`. Its 28 redirect rows failed on the missing policy before the first request. After review, the revised tests ran against START's production file through a Go overlay. Both fake origins exist only inside the in-memory transport. A guard rejects any attempted request to the target before authentication or delivery.

With the START constructor, all 28 redirect rows attempted a follow-up and failed at the guard. The location-free 308 row passed. With the fixed constructor, all 29 rows passed. The suite makes no observation of credentials at a redirect target.

## Verification

The commands below ran with the repository's Bash `verify` function and provisioned local Go cache. Go test counts include named subtests. `BASE_OVERLAY` mapped only `gchat/adapter/internal/token/token.go` to the file from START.

| Command | Result |
| --- | --- |
| `verify -C gchat/adapter go test -count=1 -json -run '^TestPublishRateLimitAndDeadline$' ./internal/server` at START | Exit 0. The existing plain-client redirect fixture passed. |
| `verify -C gchat/adapter go test -count=1 -json -run '^TestAuthenticatedClientRefusesRedirect$' ./internal/token` at the test-only commit | Exit 1 as expected. All 28 policy checks failed. |
| `verify -C gchat/adapter go test -overlay "$BASE_OVERLAY" -count=1 -json -run '^TestAuthenticatedClientRefusesRedirect$' ./internal/token` | Exit 1 as expected. 28 redirect rows failed, one upload row passed. |
| `verify -C gchat/adapter go test -count=1 -json -run '^(TestAuthenticatedClientRefusesRedirect\|TestSenderFillResourceStatus\|TestLookupHoldCarriesCredentialKind)$' ./internal/token ./internal/inbound` | First run exited 1, with 51 passes and four failures (three required-read rows and their enclosing test). Supplying the fake OAuth endpoints fixed the fixture. The final run exited 0 with 55 passes across two packages. |
| `verify -C gchat/adapter go vet ./...` | Exit 0. No diagnostics. |
| `verify -C gchat/adapter go test -count=1 -json -timeout=30m ./...` at the first fix commit | Exit 1. 2,804 named tests and subtests passed, two skipped, three failed (two cases plus an enclosing test). Two packages failed. |
| `verify -C gchat/adapter go test -count=1 -json -run '^TestLookupHoldCarriesCredentialKind$/^reaction_space$/^408$' ./internal/inbound` | Exit 0. The existing 100 ms case had timed out before receiving an HTTP status in the first full run. |
| `verify -C gchat/adapter go test -count=1 -json -run '^TestFirstStartHistoryIsReadableAndNeverATurn$' ./internal/app` | Exit 0. The existing history read had exceeded its client deadline in the first full run. |
| `verify -C gchat/adapter go test -count=1 -json -timeout=30m ./...` at the final code commit | Exit 1. 2,807 named tests and subtests passed, two live checks skipped, one test failed. Of 26 packages, 24 passed, one had no tests and one failed. Both earlier timeouts passed in this run. |
| `verify -C gchat/adapter go test -count=1 -json -run '^TestRestartWithALedgerStillDeliversTheOutage$' ./internal/app` | Exit 0. The final full run's only failure missed its five-second readiness deadline while catching up. It also passed in the first full run. |
| `gofmt -l .` in `gchat/adapter` | Exit 0 with no output at both code revisions. |

The existing `TestPublishRateLimitAndDeadline` plain-client redirect fixture passed on START and in both full runs. The fake end-to-end package passed in both full runs.

## Review

The first DEEP review at `ad241e1cbbc36762371036f2a0361d0cc28a05f8` reported one minor finding about the baseline test and two notes about upload and required-read coverage. The final code includes all three test changes. The second DEEP review at `adf5c2fcdbb09f8a9c0508f7fd77c98dc4a334eb` reported zero findings.
