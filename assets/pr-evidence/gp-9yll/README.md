# Google Chat sender fill verification

The tests use fake Chat, directory and receiving endpoints. P1–P5 are UNPROVEN. The message and membership fixtures model possible upstream answers. Verification on a running installation follows an adapter release and image update.

| Revision | Commit |
| --- | --- |
| START | `5f1ce902fce977f6b6d95da231652b0f753cce37` |
| Tests and inert declarations | `815a0268d19da0a11ff9878549abe8d9a0fdeee5` |
| Implementation | `303aa43f70d3f731ebc2696e3b1733c56ec7d860` |
| First reviewed commit | `40ae13df6c353741068914b311d3dad8a80d8eec` |
| Authentication status fix | `042245136478c34e28395a8b855a18a3e2cf36f1` |
| Credential isolation | `ead2603967a72b659a120ff10f8914739ab40a52` |
| Third-round fixes | `5a350c3bcead5725fd641f27230fd89765fbfa5f` |
| Last implementation commit | `e4ee496bfd706a315aca88daa9c68153c487e9af` |

`go version` reported `go version go1.27.3 darwin/arm64`. Test commands ran through the repository's offline verification function. The pull request's head identifies the evidence-only commit directly above the last implementation commit.

## Focused tests

```sh
verify -C gchat/adapter go test -count=1 -json -timeout=30m -run 'SenderFill|DirectoryCause' ./...
```

Counts include Go subtests and their enclosing tests. One package has no test files in each run. The transcripts preserve the output events from Go's JSON stream.

| Run | Test records passed | Failed | Skipped | Packages passed / failed / skipped | Exit |
| --- | ---: | ---: | ---: | --- | ---: |
| [A1 before implementation](red.txt) | 37 | 22 | 0 | 22 / 3 / 1 | 1 |
| [A2 answer validation mutation](red-trust-mutation.txt) | 51 | 8 | 0 | 24 / 1 / 1 | 1 |
| [A2 deadline mutation](red-deadline-mutation.txt) | 58 | 1 | 0 | 24 / 1 / 1 | 1 |
| [Credential regression before isolation](credential-red.txt) | 1 | 10 | 0 | 0 / 1 / 0 | 1 |
| Credential isolation | 80 | 0 | 0 | 25 / 0 / 1 | 0 |
| [A3 at the last implementation commit](green.txt) | 94 | 0 | 0 | 25 / 0 / 1 | 0 |

- The first scratch change let the fake source's non-nil answers reach the success path even when validation returned a cause. Message and member tests detected changed turns for another user, an anonymous user and a bot.
- The second scratch change gave the fake source a cancelable context without the fill deadline. The test's own time limit returned control and its deadline and logging assertions failed.

Both mutations were removed before the green run. The pre-implementation run failed on assertions in the framing, cache, logging, directory and application wiring tests. Its byte-comparison trust tests passed.

The first implementation passed all 59 focused test records. Review found one minor diagnostic issue. An individual run of `TestSenderFillAuthenticationStatus` reproduced a generic warning for both an OAuth 400 and a managed-mint 403 through local fake endpoints. Both classifiers now retain the status ahead of an attached deadline and disregard a plain-text lookalike. That revision passed 62 test and subtest records.

## Credential isolation

The class of failure is an optional lookup changing the credential used for subsequent required work. Before isolation, the five applicable permanent refusals and all three resource 401s failed the application test. The two enclosing tests also failed. The desktop directory control passed.

| Credential | Fake final refresh refusal | Optional requests exercised |
| --- | --- | --- |
| Desktop sign-in | HTTP 400 `invalid_grant` | Message and member |
| Key holder | HTTP 400 `grant_rejected` | Message, member and directory |

Each row delivers changes from two synthetic senders, repeats the first sender, checks the warning count and failed-entry cache, then makes a required send. The five optional refusals preserve ready health and deliver the fallback turn. The required send still begins the credential hold and raises one notice. Desktop directory lookup makes zero calls and emits zero warnings. A key holder’s literal `invalid_grant` retains its original transient behavior on both clients.

The fake application compares every wire field and the persisted event bytes against the same change replayed without a fill. It also compares the rest of the ledger row with the same receipt sequence, checking ordering for the three independently sampled persistence times. With a fixed clock, the trust matrix compares serialized ledger rows byte for byte, including those times.

A separate control substituted the original app wiring from START while running the managed directory refusal row. That row was already red at START. The generation entered its hold and raised a notice before all three changes could complete. No directory lookup exists for the desktop source.

The resource 401 rows cover member, message and directory GETs with a valid bearer. All three preserve the token for the required parent GET, with zero refreshes and zero notices. Whether Google returns this combination of endpoint responses is UNPROVEN. An optional refresh may install a successful mint. It does not start identity discovery, which can rebuild the generation. A mutation that ran identity discovery after the optional mint failed `TestSenderFillSuccessfulLookupRefresh`. A required read reused that mint and invalidated the token on its own 401.

## Worker review

| Round | Reviewed commit | Findings | Outcome |
| --- | --- | --- | --- |
| 1 | `40ae13df6c353741068914b311d3dad8a80d8eec` | 1 minor | Fixed at `04224513`. Both lookup classifiers preserve structured authentication status. |
| 2 | `042245136478c34e28395a8b855a18a3e2cf36f1` | 1 major, 1 minor | Fixed at `ead26039`. Optional refusals preserve the original credential state. Message lookup stops retrying a permanent refusal and the directory keeps its HTTP diagnostic. |
| 3 | `ead2603967a72b659a120ff10f8914739ab40a52` | 3 major, 2 minor | Four resolved. Moving lookup warnings off the delivery path addressed the original stalled-writer case. A shared-handler case remains open. [Regression transcripts](review-regressions.txt) |
| 4 | `5a350c3bcead5725fd641f27230fd89765fbfa5f` | 2 major, 1 minor | The directory classification and asynchronous warning assertions were fixed at `e4ee496b`. The shared-handler case was deferred. |

The five round-three outcomes at the last implementation commit follow.

- **Major, stalled warning sink.** A single writer handles lookup warnings across application generations. In the original fixture, two changes per source reach the fake receiver and commit while the writer stalls. Round four found a second path, described below.
- **Major, remaining bearer lifetime.** A bounded optional read yields if its current bearer cannot cover the request budget under the existing token validity test. With one second of usable bearer left and a two-second failed lookup, the required read still sends using the original bearer. Neither client’s refresh rules changed.
- **Major, resource HTTP status.** The optional transport retains the HTTP status and retry headers, and discards unsuccessful response bodies. A different status inside a fake JSON error cannot supply the cause for either message or member warnings.
- **Minor, directory negative-cache expiry.** The sender's classification uses the directory-cache eligibility time captured before the slow fill. In the paired message and member tests, crossing ten minutes during the fill preserves the same address and class.
- **Minor, non-final issuer status.** Optional token failures retain the numeric issuer status for their warning. A non-final managed refusal reports HTTP 400 and leaves the source ready.

Round four major 1 showed that a near-expiry bearer could suppress a directory read and change the sender's classification. The final change applies the bearer-lifetime check to message and member fills. All four directory classification rows passed. Round four's minor finding concerned two tests reading the log before its writer had finished. Both now wait for the expected warning before counting it.

**Open finding.** Round three major 1 and round four major 2 concern the same delivery delay. If the log sink stalls during a lookup warning, a later diagnostic sharing its handler can hold the delivery gate. Both fake runs delivered the pending change after the sink resumed. The reviewer graded this MAJOR. The mayor graded it MINOR by likelihood because it requires a stalled sink and both runs drained without loss. The shared-handler case remains open for separate work.

## Final suite results

At `e4ee496bfd706a315aca88daa9c68153c487e9af`, C3 and C4 ran consecutively. The race run covered every changed Go package. The end-to-end recovery fixture delivered all 3,000 messages with zero left in its backlog.

| Check | Result | Exit |
| --- | --- | ---: |
| C1 focused Go tests | 94 passed, zero failed or skipped. 25 packages passed and one had no tests. | 0 |
| C2 vet | Empty output | 0 |
| C3 full Go module | 2,689 passed, zero failed, two skipped. 25 packages passed and one had no tests. | 0 |
| C4 race | 1,043 passed, zero failed or skipped across six packages. | 0 |
| C5 gofmt | Empty output | 0 |
| C6 pack Python | 54 tests and 102 subtests passed, zero failed or skipped. | 0 |
| Repository Python repeat | 3,105 tests and 10,901 subtests passed, 132 skipped, two warnings. First-run failure and isolated result below. | 0 |
| Reachable | 218 passed, zero failed, one skipped by the repository command. | 0 |

```sh
verify -C gchat/adapter go test -count=1 -json -timeout=30m -run 'SenderFill|DirectoryCause' ./...
verify -C gchat/adapter go vet ./...
verify -C gchat/adapter go test -count=1 -json -timeout=30m ./...
verify -C gchat/adapter go test -race -json -count=1 -timeout=45m gchat-adapter/internal/principal gchat-adapter/internal/inbound gchat-adapter/internal/app gchat-adapter/internal/chat gchat-adapter/internal/token gchat-adapter/test/e2e
verify -C gchat/adapter gofmt -l .
verify python3 -m pytest gchat/tests -q -p no:cacheprovider
```

The public check covered the branch history, changed files, fixtures, evidence, and pull request title and body.

## Earlier module and repository runs

```sh
verify -C gchat/adapter go vet ./...
verify -C gchat/adapter go test -count=1 -json -timeout=30m ./...
verify -C gchat/adapter go test -race -json -count=1 -timeout=45m gchat-adapter/internal/principal gchat-adapter/internal/inbound gchat-adapter/internal/app gchat-adapter/test/e2e
verify -C gchat/adapter go test -p 1 -race -json -count=1 -timeout=45m gchat-adapter/internal/principal gchat-adapter/internal/inbound gchat-adapter/internal/app gchat-adapter/test/e2e
verify -C gchat/adapter gofmt -l .
verify python3 -m pytest gchat/tests -q -p no:cacheprovider
verify -C reachable/test node --test --test-skip-pattern '^review 4: archive replay advances in bounded chunks without dropping rows$'
verify python3 -m pytest tests contributing-to-gascity/tests gascity/tests discord/tests github/tests oversight-rig/tests slack-full/tests slack-channel/tests pr-pipeline/tests profiler/tests whatsapp/tests gchat/tests scripts/tests follow-through/tests -q -p no:cacheprovider
```

| Check | Result | Exit |
| --- | --- | ---: |
| C2 vet | Clean | 0 |
| C3 full module | 2,654 test records passed, two skipped, zero failed. 25 packages passed and one had no test files. | 0 |
| C5 formatting | Empty output | 0 |
| C6 pack Python | 54 tests and 102 subtests passed, zero failures or skips | 0 |
| Repository Python | 3,105 tests and 10,901 subtests passed, 132 skipped, two warnings | 0 |
| Reachable, Node 24.21.0 | 218 passed, one skipped, zero failed on the full repeat | 0 |

The first Reachable run under concurrent Go verification had 215 passes, three failures and one skip (exit 1). The failures were the warm collection-cap budget, complete-hook CPU budget and deferred-call fence. All three passed individually. The collection-cap case required a second individual run, which passed at a one-minute host load average of 29.56. The repeat of the full prescribed suite passed at a starting load of 28.26.

The first C4 run had 793 passing and two failing test records, zero skips, three passing packages and one failing package (exit 1). Both failures were timeouts in existing app tests. The end-to-end pagination test passed with its collection-only request count. These were the failing assertions.

```text
TestTokenReloadDrainsPublishBeforeRebuilding
app_test.go:439: health did not become ready in 5s: {State:catching-up Reason:recovering retained history}
TestFirstStartReplaysNothing
history_floor_test.go:140: Get "http://adapter/read?space=spaces/room": context deadline exceeded (Client.Timeout exceeded while awaiting headers)
```

Each test passed its individual race run (one pass, exit 0 in each run).

```sh
verify -C gchat/adapter go test -race -json -count=1 -timeout=2m -run '^TestTokenReloadDrainsPublishBeforeRebuilding$' gchat-adapter/internal/app
verify -C gchat/adapter go test -race -json -count=1 -timeout=2m -run '^TestFirstStartReplaysNothing$' gchat-adapter/internal/app
```

The following intermediate runs predate the final verification below.

| Revision or stage | Check | Passed / failed / skipped test records | Package pass / fail / skip | Exit |
| --- | --- | --- | --- | --- |
| First four-package serial retry, interrupted | C4 | 767 / 0 / 0 | 3 / 1 / 0 | 1 |
| First review fix, interrupted | C3 | 2,465 / 2 / 2 | 22 / 3 / 1 | 1 |
| First review fix, interrupted | C4 | 652 / 0 / 0 | 2 / 1 / 0 | 1 |
| In-development credential test fixture | C1 | 77 / 1 / 0 | 24 / 1 / 1 | 1 |
| Same fixture, stopped to rerun the corrected test | C3 | 1,882 / 1 / 2 | 19 / 1 / 1 | 143 |

The interrupted C3 recorded these assertions. Each passed an isolated non-race run.

```text
TestNineArrivalsDeliverWhileJevIsHeld
ack_test.go:75: maximum choices=3
TestRevokedTokenReplacementResumes
app_test.go:346: health did not become ready in 5s: {State:initializing Reason:connecting and reconciling subscriptions}
```

The in-development desktop token-file test omitted one local endpoint from its fixture. Its diagnostic, with the generated local path omitted, was `auth_uri and token_uri or endpoint overrides are required`. The test now supplies both endpoints. All credential tests use local fakes.

At the fourth reviewed commit, the full C3, its repeat and C4 overlapped. The runs ended before the next source edits and have no successful suite exit. C3 recorded 2,547 passes, two skips and two failures including the enclosing test. Its repeat recorded 1,266 passes and one failure. C4 recorded 900 passes and two failures. A serial C4 repeat was interrupted at 365 passes. Each failing test passed in isolation.

```text
TestExpiredDirectCommandFailureSurvivesRestart/upload
server_test.go:1925: deadline result: 500 {ConversationID: SpaceName: Thread: InReplyTo: Delivered:false Recorded:false MessageID: Key: Reason: FailureKind: RetryAfter:0}
TestRevokedTokenReplacementResumes
app_test.go:346: health did not become ready in 5s: {State:initializing Reason:connecting and reconciling subscriptions}
TestMissingTokenReplacementResumes
app_test.go:235: health did not become ready in 5s: {State:initializing Reason:connecting and reconciling subscriptions}
TestFreshTokenIdentityDeliversFirstDM
app_test.go:517: condition did not become true
```

The first fourteen-directory Python handoff at the last implementation commit had 3,104 tests and 10,901 subtests pass, 132 skips, two warnings and one failure (exit 1). The unchanged Follow-through suite failed its large-wake mail ordering assertion, `body.indexOf('0000') < body.indexOf('0024')`. The exact Node case passed in isolation (exit 0). Repeating the full fourteen-directory command passed all 3,105 tests and 10,901 subtests, with 132 skips and two warnings (exit 0).

```sh
verify -C follow-through/test node --test --test-name-pattern '^9 ruling 2: large wakes send one bounded mail and list exactly every raised item$'
```

## Scope checks at the last implementation commit

```sh
git merge-base --is-ancestor 5f1ce902fce977f6b6d95da231652b0f753cce37 HEAD
git grep -n 'Principals.Speaker(' HEAD -- gchat/adapter/internal/inbound ':!*_test.go'
```

A0 exited 0 for both commands. At START the calls were in `history.go:310` and `inbound.go:936` and `1026`. At the last implementation commit, grep printed these three calls.

```text
HEAD:gchat/adapter/internal/inbound/history.go:311:	speaker := p.opts.Principals.Speaker(speakerCtx, historyUser(c), l.IdentityDomainID)
HEAD:gchat/adapter/internal/inbound/inbound.go:939:	speaker := p.opts.Principals.Speaker(speakerCtx, m.Sender, l.IdentityDomainID)
HEAD:gchat/adapter/internal/inbound/inbound.go:1030:	speaker := p.opts.Principals.Speaker(speakerCtx, r.User, l.IdentityDomainID)
```

```bash
start=5f1ce902fce977f6b6d95da231652b0f753cce37
diff <(git show "$start:gchat/adapter/internal/principal/principal.go" | awk '/^func \(r \*Resolver\) Speaker\(/,/^}/') <(git show HEAD:gchat/adapter/internal/principal/principal.go | awk '/^func \(r \*Resolver\) Speaker\(/,/^}/')
git diff --quiet "$start" HEAD -- gchat/adapter/internal/{provenance,token,ledger,puller,config} gchat/adapter/{go.mod,go.sum} gchat/{pack.toml,commands,skills,doctor,city-fragment.toml}
git diff --numstat "$start" HEAD -- gchat/adapter
```

A4 exited 0 with empty output. The original A5 command returned 1 for the two token-package paths explicitly admitted by the credential ruling. With that ruling applied, the following A5 command exited 0 with empty output.

```bash
git diff --quiet "$start" HEAD -- gchat/adapter/internal/{provenance,token,ledger,puller,config} gchat/adapter/{go.mod,go.sum} gchat/{pack.toml,commands,skills,doctor,city-fragment.toml} ':!gchat/adapter/internal/token/token.go' ':!gchat/adapter/internal/token/lookup_client_test.go'
```

A6 exited 0 with the following output.

```text
2	0	gchat/adapter/internal/app/app.go
381	0	gchat/adapter/internal/app/sender_credential_test.go
43	0	gchat/adapter/internal/app/sender_fill_test.go
11	3	gchat/adapter/internal/app/workers.go
3	2	gchat/adapter/internal/chat/chat.go
2	1	gchat/adapter/internal/inbound/history.go
9	5	gchat/adapter/internal/inbound/inbound.go
160	0	gchat/adapter/internal/inbound/sender_fill.go
248	0	gchat/adapter/internal/inbound/sender_fill_auth_test.go
245	0	gchat/adapter/internal/inbound/sender_fill_cache_test.go
281	0	gchat/adapter/internal/inbound/sender_fill_test.go
82	0	gchat/adapter/internal/principal/directory_cause_test.go
107	10	gchat/adapter/internal/principal/principal.go
81	0	gchat/adapter/internal/principal/warnings.go
185	0	gchat/adapter/internal/token/lookup_client_test.go
87	2	gchat/adapter/internal/token/token.go
9	1	gchat/adapter/test/e2e/e2e_test.go
```

The only replaced test line was the pagination assertion in `test/e2e/e2e_test.go:966` at START. The replacement at lines 966–974 counts that room's collection requests in the fake's history log. A GET of an individual message can no longer satisfy the two-page assertion.

## Fixtures and output

- `internal/inbound/sender_fill_test.go:51` marks the synthetic message and membership GET fields UNPROVEN. Line 71 marks the live and catch-up event shape UNPROVEN.
- `internal/inbound/sender_fill_cache_test.go:201` marks a matching domain without a display name UNPROVEN.
- `internal/app/sender_fill_test.go:23` marks the bare live event and fuller GET answer UNPROVEN.

- `internal/app/sender_credential_test.go:42` marks the bare actor premise UNPROVEN. Line 301 marks the endpoint-specific 401 premise UNPROVEN. Line 359 marks the stalled-writer fixture’s bare message UNPROVEN.

The two warning messages are `sender lookup incomplete` with `source` and `cause`, and `directory lookup incomplete` with `cause`. The sender sources are `message` and `member`. The sender causes are `credential`, `http_<status>`, `timeout`, `error`, `no_user`, `other_user`, `not_a_person`, `no_domain_id` and `no_display_name`. The directory causes are `http_<status>`, `not_found`, `no_address`, `timeout`, `bad_id` and `error`.

The README adds these two sentences.

> Filling an incomplete sender costs one GET for each new sender, bounded at 3 seconds, inside the gate (no call for a message or a reaction of a sender the cache holds), and a sender is asked again after 1 hour, a failed one after 10 minutes. Whether the message and membership GETs return both fields is UNPROVEN, and the rule can classify a sender whose name is still missing.

The final Unreleased entry reads as follows.

> Fill incomplete senders before classification and log each lookup failure cause once per source. The cost is one GET for each new sender, bounded at 3 seconds, inside the gate (no call for a message or a reaction of a sender the cache holds), and a sender is asked again after 1 hour, a failed one after 10 minutes. Whether the message and membership GETs return both fields is UNPROVEN, and the rule can classify a sender whose name is still missing.
