# Inbound hold notice verification

The base made two Slack requests from inbound recovery. The initial notice used the held message's thread. The outcome updated that post by its returned timestamp. Both now check the current room policy, and an incident reported only to the operator has no Slack post to edit.

## Red controls

- On the unchanged implementation, all eight cases in `TestInboundRecoveryNoticeRoutes` failed. The unlisted channel produced one `chat.postMessage` request, and no case wrote the expected operator mail. The original DM timing, post and edit case passed.
- With both internal reporting destinations unavailable, the first implementation lost the opening report if the hold settled before the next attempt. `TestInboundRecoveryNoticeReportingFailureSurvivesSettlement` observed zero reports after restart. It now observes the opening report followed by its outcome.
- Self-review found that a failed operator report suppressed a permitted Slack post and its edit, and that an uncertain mail acknowledgement created a second record on disk. Both controls failed on the reviewed revision. Independent operator retries preserve both Slack calls, and uncertain mail retries under its original deduplication key.

## Local checks

These commands ran from `slack-full/adapter` with Go 1.27.3 in the repository's verification environment.

```sh
go test -count=1 -json -run 'InboundRecovery|Round2|Notice|NeverSilentHold|IngestionMail' .
go vet ./...
```

All 286 selected tests and subtests passed. Vet passed. The routing cases use a local Slack HTTP server and a fake mail executable. They check posts, edits, internal record counts, original message and thread timestamps, and exclusion of the held message body. Other cases exercise a removed room, newly allowed room, legacy snapshot, unavailable inbox and reporting recovery after restart.

The repository's Reachable check passed 218 tests on Node 24.21.0 and skipped the archive replay case specified in the verification guide. The repository Python run passed 3,105 tests and 10,901 subtests, with 132 skips and two warnings. Hosted CI runs the complete Slack adapter suite.
