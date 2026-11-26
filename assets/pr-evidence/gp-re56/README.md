# Slack dead-letter alarms

START at `e0b06b76d1d87493ae632651f5b5c24898c99ff2`. Red-first tests at `83d5a59ef196a693d15cba6db5426d58706f4a33`. Verified implementation at `d2fbd3e6d6a187cda81323f098ef8a77fb5d0d83`. The evidence commit follows the implementation.

## Results

Go reported `go version go1.27.3 darwin/arm64` for the builds, tests and vet. The repository’s verify function ran each command offline with a temporary home. The Go 1.26.10 formatter reproduces the checkout’s formatting. Go 1.27.3 lists the unchanged `company_thread_ambient.go` on START because it indents a multiple-result literal differently.

```text
C1 verify -C slack-full/adapter go test -count=1 -json -run 'DeadLetterAlarm' . exit=0
51 passed, 0 failed, 0 skipped

C2 verify -C slack-full/adapter go vet ./... exit=0
no diagnostics

C3 verify -C slack-full/adapter go test -count=1 -json -run 'InboundRecovery|Round2|Notice|NeverSilentHold|IngestionMail|HeldAnswer|HeldClock|DeadLetterAlarm' . exit=0
431 passed, 0 failed, 0 skipped

C4 verify -C slack-full/adapter go test -race -count=1 -json -run 'DeadLetterAlarm' . exit=0
51 passed, 0 failed, 0 skipped

C5 verify -C slack-full/adapter gofmt -l . exit=0
empty output (Go 1.26.10)

C6 verify -C reachable/test node --test --test-skip-pattern '^review 4: archive replay advances in bounded chunks without dropping rows$'
First run under load: exit 1, 217 passed, 1 failed, 1 skipped. The paired-hook case passed in isolation (exit 0).
Full isolated rerun: exit 0, 218 passed, 0 failed, 1 skipped. Node v24.21.0.

C6 verify python3 -m pytest tests contributing-to-gascity/tests gascity/tests discord/tests github/tests oversight-rig/tests slack-full/tests slack-channel/tests pr-pipeline/tests profiler/tests whatsapp/tests gchat/tests scripts/tests follow-through/tests -q -p no:cacheprovider
exit 0, 3118 passed, 0 failed, 136 skipped, 10932 subtests passed, 2 warnings.
```

C-7 passed on draft head `b55d630808bc027744e7b4214250a1c4e4c2654b`. The hosted Check job ran `go test ./...` in `slack-full/adapter` and finished successfully. [Adapter step in Check](https://github.com/aphexcx/gascity-packs-prs/actions/runs/45803870085/job/139853851368#step:15:1)

The transcripts preserve Go’s per-test outcomes and assertion lines. Counts include named subtests and parent tests, and exclude the package outcome. The fake mailbox cases cover the retained local dedup key, archive between a lost result and a retry, and arrival before a retry.

## Fault injection

Only the tests and compile-time declarations existed at the first commit. The baseline recovery run passed 380 tests and subtests. The empty worker passed 13 and failed 38 on assertions (A1). The five temporary edits below were reverted independently.

| Edit | Assertion exercised | Failed |
| --- | --- | ---: |
| Treat every entry as dead and remove the age limit | The alarm set excludes delivered, held, nonhuman and expired messages. | 17 |
| Reuse the pass snapshot before remote mail | An arrival during local mail leaves the remote batch. | 2 |
| Allow a remote send with an empty context | An unconfigured operator receives zero commands. | 2 |
| Hold the recovery mutex across the command | Recovery can offer and poll while mail is open. | 1 |
| Have local mail call the recovery sender | Only recovery offers and pastes the message. | 4 |

## File boundaries

```text
A0 $ git merge-base --is-ancestor 5f1ce902fce977f6b6d95da231652b0f753cce37 e0b06b76d1d87493ae632651f5b5c24898c99ff2
exit 0
A0 $ git grep -c 'func TestHeldClock' e0b06b76d1d87493ae632651f5b5c24898c99ff2 -- slack-full/adapter
e0b06b76d1d87493ae632651f5b5c24898c99ff2:slack-full/adapter/held_clock_test.go:9
exit 0
A0 $ git grep -n '"mail", "send"' e0b06b76d1d87493ae632651f5b5c24898c99ff2 -- slack-full/adapter ':!*_test.go'
e0b06b76d1d87493ae632651f5b5c24898c99ff2:slack-full/adapter/ingestion_observer.go:741:	cmd := exec.CommandContext(ctx, "gc", "--city", cityPath, "mail", "send", "mayor", "--dedup", key, "-s", subject, "-m", body)
e0b06b76d1d87493ae632651f5b5c24898c99ff2:slack-full/adapter/internal/neversilent/engine.go:152:			cmd := exec.CommandContext(ctx, opts.GC, "mail", "send", "--context", opts.OperatorContext, opts.OperatorAgent, "--dedup", m.Key, "-s", m.Subject, "-m", m.Body)
e0b06b76d1d87493ae632651f5b5c24898c99ff2:slack-full/adapter/never_silent.go:75:			cmd := exec.CommandContext(ctx, bin, "mail", "send", "mayor", "--dedup", key, "-s", "slack: owner delivery notice", "-m", body)
exit 0
A0 $ git grep -c 'func recoveryCountClass' e0b06b76d1d87493ae632651f5b5c24898c99ff2 -- slack-full/adapter/inbound_recovery.go
e0b06b76d1d87493ae632651f5b5c24898c99ff2:slack-full/adapter/inbound_recovery.go:1
exit 0
A0 $ git grep -c 'func TestDeadLetterAlarm' e0b06b76d1d87493ae632651f5b5c24898c99ff2 -- slack-full/adapter
exit 1
A4 $ git diff --numstat e0b06b76d1d87493ae632651f5b5c24898c99ff2 HEAD -- slack-full/adapter/inbound_recovery.go slack-full/adapter/main.go
62	0	slack-full/adapter/inbound_recovery.go
1	0	slack-full/adapter/main.go
exit 0
A4 $ git diff --quiet e0b06b76d1d87493ae632651f5b5c24898c99ff2 HEAD -- slack-full/adapter/internal slack-full/adapter/go.mod slack-full/adapter/go.sum slack-full/adapter/ingestion_observer.go slack-full/adapter/never_silent.go slack-full/adapter/never_silent_recovery.go slack-full/adapter/inbound_dead_letter.go slack-full/adapter/delivery_receipt.go slack-full/adapter/receipt_follow_up.go slack-full/adapter/inbound_coalescer.go slack-full/pack.toml slack-full/commands slack-full/scripts slack-full/reactions.json slack-full/docs
exit 0
A5 $ git diff --numstat e0b06b76d1d87493ae632651f5b5c24898c99ff2 HEAD -- slack-full/adapter
433	0	slack-full/adapter/dead_letter_alarm.go
619	0	slack-full/adapter/dead_letter_alarm_test.go
62	0	slack-full/adapter/inbound_recovery.go
1	0	slack-full/adapter/main.go
exit 0
```

## Records, commands and notices

The read-only recovery method is `deadLetterAlarmSet`. The file next to the hold queue’s state is `dead-letter-alarms.json`. An unreadable generation is retained, and subsequent restarts load its latest replacement.

- `alarmRowHorizon = 24 * time.Hour`
- `alarmRetention = 48 * time.Hour`
- `alarmCommandLimit = 120 * time.Second`
- `alarmPassInterval = 10 * time.Second`
- `alarmRecordsFile = "dead-letter-alarms.json"`

The local subject is `slack: a message did not reach the session` and the key prefix is `slack-dead:`. The operator subject is `slack: <city>: <N> message(s) did not reach the session` and the key prefix is `slack-dead-operator:`.

```text
channel: <room>
thread_ts: <thread or message ts>
message_ts: <message ts>
held_since: <UTC RFC 3339>
dead_lettered_at: <UTC RFC 3339>
state: <retrying|waiting|hand_check>
composer: <stored state|none>
operator: <configured|not_configured>
look: gc slack status
redrive: gc slack inbound-redrive --conversation-id <room>
by hand: gc slack inbound-redrive --conversation-id <room> --acknowledge <message ts>
```

```text
city: <city>
channel: slack
messages: <count>
conversations: <count>
oldest_age_seconds: <seconds>
retrying: <count>
waiting: <count>
hand_check: <count>
composer: <oldest composer>
local_wake: <asked|not_asked|not_known>
status: gc slack status --json
```

The worker’s log messages are shown below. The local cause is `mail_refused`, `mail_timeout` or `mail_no_result`. The remote cause is `operator_refused` or `operator_not_known`.

```text
dead-letter alarm: operator=%s
dead-letter alarm: channel=%s message_ts=%s try=%d cause=%s
dead-letter alarm: messages=%d try=%d cause=%s
dead-letter alarm: records unreadable; using a new file
dead-letter alarm: records write failed
```

## Documentation and fixtures

For each message that gets a dead-letter row, one mail in the local mayor's mailbox and one request to gc for a turn of the local mayor; where an operator is named, one count-only mail to the operator's city for each pass that has new such messages; a mail command may run up to 120 seconds; a road that is down is tried again on the hold ladder for as long as the message is outstanding and less than a day old. The alarm works independently of `NEVER_SILENT`, checks for arrival before each send and leaves replay to the hold queue. An unknown local result is retried with the same key. If the first mail was created and archived before that retry, the mayor can receive a second mail for a message still outstanding. An ambiguous operator send is recorded and never retried. Remote mail and dedup behavior (Q3) are UNPROVEN, an operator turn (Q4) is UNPROVEN, and local mail and wake behavior (Q5) are UNPROVEN.

- **Dead-letter alarms.** For each message that gets a dead-letter row, one mail in the local mayor's mailbox and one request to gc for a turn of the local mayor; where an operator is named, one count-only mail to the operator's city for each pass that has new such messages; a mail command may run up to 120 seconds; a road that is down is tried again on the hold ladder for as long as the message is outstanding and less than a day old. Arrival closes the alarm. An ambiguous operator send ends its retry. Remote mail behavior (Q3) is UNPROVEN, an operator turn (Q4) is UNPROVEN, and local mail and wake behavior (Q5) are UNPROVEN.

- `slack-full/adapter/dead_letter_alarm_test.go:17` UNPROVEN Q3/Q5: this mailbox models local unarchived-key dedup, a requested wake and a remote mailbox without dedup. It never contacts either city.
- `slack-full/adapter/dead_letter_alarm_test.go:67` UNPROVEN Q2/Q4/Q6: operator configuration, reading remote mail, and the production command deadline are premises. This fixture models settings and mailbox writes only, and timeout cases use a shorter limit.
- `slack-full/adapter/dead_letter_alarm_test.go:208` UNPROVEN Q5: archiving between a lost result and a retry releases the fake dedup key.
- `slack-full/adapter/dead_letter_alarm_test.go:496` UNPROVEN Q7: compare the normal held clock and vouch paths against the same fake receipts with the alarm enabled.
