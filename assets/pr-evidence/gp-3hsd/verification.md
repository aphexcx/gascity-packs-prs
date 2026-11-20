# Inbound hold notice verification

The base made two Slack requests from inbound recovery. The initial notice used the held message's thread. The outcome updated that post by its returned timestamp. Both now check the current room policy, and an incident reported only to the operator has no Slack post to edit.

## Red controls

- On the unchanged implementation, all eight cases in `TestInboundRecoveryNoticeRoutes` failed. The unlisted channel produced one `chat.postMessage` request, and no case wrote the expected operator mail. The original DM timing, post and edit case passed.
- With both internal reporting destinations unavailable, the first implementation lost the opening report if the hold settled before the next attempt. `TestInboundRecoveryNoticeReportingFailureSurvivesSettlement` observed zero reports after restart. It now observes the opening report followed by its outcome.
- Self-review found that a failed operator report suppressed a permitted Slack post and its edit, and that an uncertain mail acknowledgement created a second record on disk. Both controls failed on the reviewed revision. Independent operator retries preserve both Slack calls, and uncertain mail retries under its original deduplication key.
- The next review reproduced three failures. A later hold disappeared behind an earlier failed edit, a definite refusal after restart duplicated a possibly accepted mail, and a concurrent fallback writer could replace the first record. Pending edits now retain their original targets independently of later incidents. The adapter checkpoints possible mail acceptance before invoking the mail command, and the fallback claims its file with a create-once hard link.

## Successive holds in the same channel

At `b5d415fc`, the first Slack post's backoff also postponed the outcome after the hold settled. The next hold could reuse the old incident. Recovery now queues the outcome on the settlement tick, even if Slack refused the opening post. Only a posted notice has a closing edit to retry.

`TestInboundRecoveryNoticeSuccessiveIncidents` acknowledges the first message, starts the second before the retry deadline, then delivers the second. It checks the four operator reports in order, including their original channel, thread, message timestamp, start time and distinct outcome text. It also checks every Slack post and edit target.

| First incident | Second message | Conversations | At `b5d415fc` | After fix |
| --- | --- | --- | --- | --- |
| Retryable opening post failure | Same thread, different thread, top level | DM and listed room | 6 failed | 6 passed |
| Permanent opening post failure | Same thread, different thread, top level | DM and listed room | 6 failed | 6 passed |
| Closing edit failure | Same thread, different thread, top level | DM and listed room | 6 passed | 6 passed |
| Operator reports only | Top level | Unlisted room | Passed, zero Slack requests | Passed, zero Slack requests |

Every failed row reported `first settlement tick: got 1 reports, want held and outcome` and `operator reports = 2, want 4`. In the different-thread case a retryable failure sent the second post to the first thread. A permanent failure suppressed the second post altogether. The adjacent TSV records all 19 rows on both revisions.

The reaction-anchor regression now requires exactly one Slack call for the held message in a listed room. The incomplete-history regression uses a listed room too and requires zero calls for manual recovery.

## Local checks

These commands ran from `slack-full/adapter` with Go 1.27.3 in the repository's verification environment.

```sh
go test -count=1 -json -run 'InboundRecovery|Round2|Notice|NeverSilentHold|IngestionMail' .
go test -race -count=1 -json -run '^TestInboundRecoveryNotice' .
go vet ./...
```

All 309 selected tests and subtests passed. All 40 notice tests and subtests passed with the race detector. Vet passed. The routing cases use a local Slack HTTP server and a fake mail executable. They check posts, edits, internal record counts, original message and thread timestamps, and exclusion of the held message body. Other cases exercise a removed room, newly allowed room, legacy snapshot, unavailable inbox and reporting recovery after restart. The adjacent TSV contains the notice cases' status events from the Go runner.

The repository's Reachable check passed 218 tests on Node 24.21.0 and skipped the archive replay case specified in the verification guide. Hosted CI runs the complete Slack adapter suite.

The first full Python run on this change passed 3,104 tests and 10,901 subtests, with 132 skips, two warnings and one failure. The Darwin ACL fixture invoked GNU `chmod`, which rejects the macOS `+a` option. The same fixture passed in isolation with the platform's `chmod`. The PR reports the full-suite repeat with that environment.

Run both checks from the repository root using its verification function.

```sh
verify -C reachable/test node --test --test-skip-pattern '^review 4: archive replay advances in bounded chunks without dropping rows$'
verify python3 -m pytest tests contributing-to-gascity/tests gascity/tests discord/tests github/tests oversight-rig/tests \
  slack-full/tests slack-channel/tests pr-pipeline/tests profiler/tests whatsapp/tests gchat/tests scripts/tests follow-through/tests -q -p no:cacheprovider
```
