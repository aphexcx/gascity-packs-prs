# gp-d009: compact mention-only deliveries

The default mention-only delivery now carries the sender, channel, message ts,
thread root, message, existing attachments/context, and one reply command. The
standing guidance lives in `slack-full/template-fragments/slack-v0.template.md`,
under **Mention-only rooms**. No live adapter or Slack workspace was used.

Measurement base: `52d25900455a8c734ba4b899224b4b39d6a50247`. All message text, names,
channel/user ids and timestamps in these captures are invented.

## Measurement boundary and baseline

The fixture is exactly 40 ASCII characters:
`Please review the invented test message.`

`TestMentionOnlyDeliveryMeasurements` captured the baseline before changing
production code, then captured the same fixtures after. Mention-only rows are
the complete adapter-generated system reminder. Shared-conversation and DM
rows are the adapter's `Text` captured by a fake gc server: gc adds its own
outer sender/channel/id/reply envelope outside this repository. That external
envelope is excluded, so these rows are controls for adapter text, not claims
that a complete shared/DM prompt has zero overhead. The two boundaries are not
directly comparable. Both cold and warm controls are shown because their
existing long help is already sent once per channel per adapter lifetime.

No attachment or unseen prior context is present in the byte fixture. Tests
separately preserve those payloads. Channel-name lookup uses the unresolved-id
fallback; display-name and channel-name lengths can change the header size.

| Delivery | Message bytes | Other bytes before | Other bytes after |
| --- | ---: | ---: | ---: |
| Reply in a thread the mention-only session posted in | 40 | 1,290 | 267 |
| Mention in a mention-only room | 40 | 1,232 | 267 |
| Shared conversation, first delivery | 40 | 397 | 397 |
| Shared conversation, subsequent delivery | 40 | 0 | 0 |
| Direct message, first delivery | 40 | 397 | 397 |
| Direct message, subsequent delivery | 40 | 0 | 0 |

The measured default mention-only overhead is below the requested 400-byte
bound. Its reply line is 111 characters, below 160. Full mode is the default;
the opt-in minimal renderer and its ids-only reply pointer are unchanged.
Authenticated company-room/DM turns retain their existing turn references.

## Bytes and tokens

Tokens are measured with `tiktoken` / `o200k_base`, not estimated from bytes.
Run `python measure.py` from this directory with `tiktoken` installed to
reproduce `measurements.json` from `before.json` and `after.json`.

| Scenario | Bytes before | Bytes after | Tokens before | Tokens after |
| --- | ---: | ---: | ---: | ---: |
| One mention-only mention | 1,272 | 307 | 289 | 84 |
| One own-thread reply | 1,330 | 307 | 307 | 84 |
| Forty own-thread replies | 53,239 | 12,319 | 12,280 | 3,360 |

The forty-message projection repeats the 40-character fixture under one
thread root with forty distinct synthetic message timestamps, joining
deliveries with a newline. It models a session already participating in the
thread, with no new context or attachments. The saved 8,920 tokens exclude the
one-time startup-fragment cost. The single-mention reduction is 205 tokens.
The raw startup fragment adds 1,482 bytes / 320 tokens relative to the base
(also unchanged relative to the integrated main at `a453efab`),
so the forty-reply example saves 8,600 tokens after that one-time cost. This
counts the fragment source; template interpolation can change the full prompt.

Refresh the after capture from `slack-full/adapter`:

```sh
SLACK_DELIVERY_MEASURE_OUTPUT=../../assets/pr-evidence/gp-d009/after.json go test -run '^TestMentionOnlyDeliveryMeasurements$' -count=1 -v .
```

## Before and after

Before (the exact capture is the `mention` entry in `before.json`):

```text
<system-reminder>
Slack mention-only room delivery: you were @mentioned in channel C_EXAMPLE (Slack ts 9000000000.000002) by user Example (U_EXAMPLE).

Message text:
Please review the invented test message.

You are bound to this room in mention-only mode: only @mentions of your bot user, messages addressed to your handle, and replies in threads you posted in reach you. Read the rest of the room on demand:
  gc slack read --conversation-id C_EXAMPLE

React to this message with writing_hand to signal you are actively working on it:
  gc slack react --conversation-id C_EXAMPLE --message-id 9000000000.000002 --emoji writing_hand

To reply in that thread, write your reply to a tmpfile and run:
  gc slack publish-to-channel \
    --conversation-id C_EXAMPLE \
    --thread-ts 9000000000.000002 \
    --body-file <tmpfile>

This posts directly through the slack adapter with your registered identity (you hold no channel binding here, and it is unaffected by any company-room pointer your session may carry). `gc slack react`, `reply-current --thread-current` and `upload --thread-current` resolve this delivery on their own once gc has confirmed it (moments after it reaches you); the explicit --conversation-id/--thread-ts form above always works.
</system-reminder>
```

After (the exact capture is the `mention` entry in `after.json`):

```text
<system-reminder>
Slack mention-only room delivery: Example (U_EXAMPLE) in C_EXAMPLE (ts=9000000000.000002; thread=9000000000.000002)

Please review the invented test message.

Reply: gc slack publish-to-channel --conversation-id C_EXAMPLE --thread-ts 9000000000.000002 --body-file <file>
</system-reminder>
```

## Where each removed instruction went

All destinations below refer to **Mention-only rooms** in the startup fragment.

| Removed sentence or text | Destination or reason for removal |
| --- | --- |
| “You are bound ... only @mentions ... handle ... replies in threads you posted in reach you.” | First paragraph preserves the three delivery triggers. |
| “Read the rest of the room on demand” and its command | First paragraph includes `gc slack read --conversation-id <channel>`. |
| “React ... with writing_hand ... actively working on it” and its command | Reaction paragraph and explicit channel/message command. |
| “To reply ... write your reply to a tmpfile and run” and four-line command | Reply paragraph and single-line `publish-to-channel` command; the filled-in command also stays in every full-mode delivery. |
| “This posts directly ... registered identity ... no channel binding ... unaffected by any company-room pointer” | Final paragraph preserves identity, binding and pointer rules. |
| “react, reply-current --thread-current and upload --thread-current resolve ... once gc has confirmed ... moments after it reaches you” | Final paragraph names all three commands and confirmation timing. |
| “the explicit --conversation-id/--thread-ts form above always works” | Final paragraph says the explicit destination needs no confirmation wait. |
| Reason prose (“you were @mentioned”, addressed handle, own-thread reply) | Dropped from presentation; routing reasons remain in delivery records. Timestamps identify the thread. |
| “Message text:” label | Removed as redundant framing; the message itself remains verbatim after existing redaction/markup neutralization. |

The broader reply section now scopes `turn_ref` guidance to company-room/DM
turns so it does not contradict the explicit mention-only command.

## Validation

Red before green: all six new compact-format cases failed on the old renderer
(overhead 1,232–1,290 bytes, no single reply line); the new startup-fragment
test failed because mention-only rules were absent. Both pass after the change.
The new context/attachment test passes with both renderers, verifying retention.

Only expected rendering strings in existing delivery assertions and fake
interceptors changed; routing-reason assertions now inspect delivery metadata.
Routing selection, acknowledgments, retries, journals, redaction and attachment
handling have no production changes. The original reminder-type header remains
so the company-room fake dispatcher recognizes the same deliveries.

Python 3.15.0: `python -m pytest tests -q`, from `slack-full`, passed 976 tests
and 10 subtests, with 3 existing skips and 2 existing `os.fork` deprecation
warnings. Go 1.27.3: `go test -race -count=1 -json ./...`, from `slack-full/cli`,
passed 237 test/subtest entries (199 top-level tests). No Node tests are involved.

From `slack-full/adapter`, `go test -race -count=1 -timeout=20m -json ./...`
passed 13,100 test/subtest entries (1,491 top-level tests), with 3 existing
skips and zero failures, at `015182f7`. The only subsequent test changes
preserve the two handle-reason checks through delivery metadata; the unchanged
production code and those final assertions were then exercised by
`go test -race -count=1 -run 'Test(MentionOnly|FormatMentionOnly|CompanyRoomMentionOnly|MinimalDirectSessionPaths)' -json .`:
61 entries passed, zero failures. Both the full and focused runs use fakes.

Self-review round 1 (STANDARD, code commit `fa50efb3`): 0 critical, 1 major,
0 minor. Class: incomplete format migration in test assertions/fake interceptors.
The full adapter run reproduced all four stale sites (handle, bot mention,
mapped user-group mention, and retry timestamp). The compact contract test
covers all three admission reasons and both root/reply cases. All stale sites
were updated, the retry fake still rejects its selected message, and the
former reason-text assertions retain their checks through delivery metadata.
The focused delivery suite then passed all 61 cases under the race detector.
Outcome: **FIXED** in `51078dfd` (including the text updates in `015182f7`).

Self-review round 2 (STANDARD, `51078dfdb19fd7f209dca43471e946bbd6823e7c`):
**CLEAN**, 0 critical / 0 major / 0 minor / 0 nit. The reviewer read the full
delta against the base in a separate read-only checkout and did not run tests
in its sandbox.

Before handoff, main advanced to `a453efabfd87e25f2cccde2707bebebae0b06d92`.
The merge retained both independently added prompt-contract tests and the
upstream reaction catalog unchanged. The adapter implementation did not change.
From `slack-full`, `python -m pytest tests -q -k 'prompt_contract or reply_template_contract'`
then passed 7 cases (973 deselected), including both the new reaction-catalog
contract and this bead's mention-only contract. The shared reaction section was
also compared byte-for-byte with main. This bead does not edit that catalog.

Public-repo check: all added fixtures and captures contain invented values;
no live messages, credentials or host-specific paths are included.
