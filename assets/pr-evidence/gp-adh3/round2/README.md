# gp-1v3u: PR 91 round 2 (the five reactions, bead gp-adh3)

The mayor's codex gate r1 on `a59a65c33c966b1db77074b4fbd7e8dedddc26fd`
blocked on one MAJOR: the Slack fragment showed `gc slack react` with no ids
first, and that form cannot see a company room, DM or group DM turn. The
mayor's read on another model made the miss happen with fakes, and added two
MINORs: the WhatsApp skill never says what a thanks gets there, and the rules
test missed a fifth rule added after a blank line. This round fixes all
three, each by its class. No command or adapter code changes.

- Round base: `a59a65c33c966b1db77074b4fbd7e8dedddc26fd` (PR 91's head after round 1)
- Code head: `a3ba794f98cf95eed613bedf479e888e4ea75a76`. The commits after it
  touch only this directory.
- Commits:
  - `4b12f74c` the tests (red)
  - `d7d14222` the rules run to the end of their section
  - `3d9fc865` the texts and changelogs
  - `5917d03a` the self-review's findings
  - `a3ba794f` a heading is hashes then a space

## The sentence the founders were shown that changed

Round 1's "For the founders to see" listed, among Slack's own lines:

- old: "The two forms of `gc slack react`."
- new: "One form of `gc slack react`, which names the message by the channel
  id and the timestamp from its reminder."

The Slack text, old above new:

```
Place a reaction by its Slack name:

gc slack react [--emoji NAME] [--session SID]
gc slack react --conversation-id Cxxx --message-id 1234.5678 [--emoji NAME]

The first form reacts to the latest message routed to your session, with
`eyes` when you give no name. The second names one message by its channel
id and its timestamp. Use the second when another message may have reached
you since.
```

```
Place a reaction by its Slack name, on the message you are answering:

gc slack react --conversation-id Cxxx --message-id 1234.5678 --emoji NAME

Take both values from the reminder of the message you are answering: the
channel id, and that message's own timestamp (`origin_ts`, `Slack ts`,
`Message id` or `ts=`), never its thread root. In a batch of several
messages, a message's own timestamp is the one in brackets at the start of
its line; `Message id` names only the newest. Never react without both
values, even where a reminder prints a react command that leaves them out:
the command then reacts to the latest message it can find for your session,
which may be another one, and it never finds a company turn (a company
room, DM or group DM). Its receipt can still say delivered. When the
reminder does not show both values, do not react: say the same thing in
words.
```

WhatsApp gains one sentence, at the end of the paragraph just above the
rules:

> Until you can, a message that only thanks, confirms, agrees or closes a
> topic still gets an answer from you in words (a reaction from them still
> rarely needs one).

The bracket keeps it from pulling against an older line of the skill: "A
reaction arrives as `[reaction <emoji> to <message id>]` and rarely needs an
answer." It adds no rule about length.

Unchanged: the table of five, the four rules, the reply-length lines, the
receipt sentence, and the whole Google Chat skill.

## Item 1: the class, and every case of it

**The class.** A text tells the mayor to react with a command form whose own
lookup cannot see the turn being answered. The reaction lands on another
message, the command says delivered, and rule 2 ("No text follows it")
leaves the person with nothing.

**Where the Slack form with no ids looks.** `slack_chat_react.py:88-96`
calls `find_latest_inbound_message_id_for_session`
(`slack_intake_common.py:977-993`). That reads two sources and no third
(`:582-614`): gc's `extmsg.inbound` events for the session, and the
adapter's mention-only delivery log. From the newest event it takes the
newest inbound of that event's conversation (`:1035-1065`), which the code
itself calls a deliberate anchor caveat (`:1015-1029`).

**Every kind of Slack turn, against that form.** All file references are
under `slack-full/` at the head.

| Kind of turn | How it reaches the session | Can the form with no ids miss? | What its reminder shows |
| --- | --- | --- | --- |
| Ordinary channel (a bound room), top-level | gc's extmsg inbound (`adapter/main.go:6888`) | Yes. It takes the newest inbound of the conversation the session last heard from, so a newer message there, or in another of the session's conversations, takes the reaction | gc's reminder names `slack/<channel id>` and `Message id: <ts>` (`ts=` in the short envelope); the minimal mode prints `ts=` and a read pointer with `--conversation-id` (`adapter/injection_mode.go:37-62`) |
| A thread reply in one | the same | Yes. The newest inbound can sit in another thread | `Message id: <ts> (in thread <root>)`; the reply line's `--reply-to` is the root, never the message |
| A coalesced batch | one gc inbound for the batch, its newest member (`adapter/inbound_coalescer.go:3231`) | Yes, for every member but the newest | each member's ts in brackets at the start of its line (`:3136-3150`); `Message id` is the newest only |
| Mention-only room | an adapter session message, logged in memory (`adapter/mention_only.go:1092-1102`) | Yes. A record gc has not confirmed never outranks a gc inbound or an older confirmed record (`scripts/slack_intake_common.py:603-614`, `:853-877`); the log is lost when the adapter restarts (`adapter/mention_only.go:531-532`) | `in channel ... (Slack ts <ts>, a reply in thread <root>)` and the reminder's own react line with both ids (`adapter/mention_only.go:783-800`) |
| Address-by-handle | an adapter session message (`adapter/main.go:6706`) | Yes. It is logged for the lookup only when the session also holds a mention-only binding there (`adapter/mention_only.go:982-1000`) | `from channel ... (Slack ts <ts>)` and `--conversation-id` in its publish line; its own react line has no ids (`adapter/main.go:6661-6662`) |
| Company room: `ambient`, `thread_ambient`, `targeted`, the peer kinds | a gc session message (`adapter/main.go:3597-3599`, `postCompanyMessage` at `adapter/company_delivery.go:1542-1558`) | Always. It is in neither source: an older message elsewhere takes the reaction with delivered true, or the command exits 1 | `channel_id:`, `origin_ts:`, `thread_root_ts:` (`adapter/company_hydration.go:449-468`) |
| Company DM | a gc session message (`adapter/company_dm.go:345-346`, through `postCompanyMessage`) | Always | the same three; Slack refuses even the named form there (the switchboard token, round 1), the receipt says `"delivered": false`, and the mayor says it in words |
| Company group DM | a gc session message (`adapter/company_mpim.go:397-398`, the same) | Always | the same three |

No kind of turn is safe for the form with no ids, so the text shows only
the named form and says never to leave the ids out.

**Google Chat.** `gc gchat react` has no form without `--message`. The
script exits 1 with "--message and --emoji are required"
(`gchat/commands/react.sh:16`), and the adapter answers 400 to an empty
message (`gchat/adapter/internal/server/server.go:1232-1234`). The message is
named by its full name, `spaces/<id>/messages/<id>`, which carries its
space. So the command picks nothing on its own and cannot reach another
space. No case. The skill is unchanged.

**WhatsApp.** No react verb (`whatsapp/commands/`), so no case.

**What a mayor loses.**

- The one-flag form: every reaction now carries two values copied from its
  reminder.
- The default `eyes`: the text now names the emoji each time.
- A reaction where the reminder shows no timestamp of the message itself,
  for example a gc build older than the reminder's `Message id` line. There
  the mayor answers in words.
- Nothing on a company DM: Slack refuses the named form there too, so it
  was words either way.

**Not in this round: the command's defect.**

1. `slack_chat_react.py:88-96` resolves the form with no ids through
   `slack_intake_common.py:977-993` and `:582-614`, which read gc's
   `extmsg.inbound` events and the mention-only log only.
2. A company turn (room, DM, group DM) reaches the session as a session
   message (`adapter/company_delivery.go:1543-1558`, `adapter/main.go:3597-3599`).
   No such event exists for it, so the lookup never sees it.
   `slack_chat_reply_current.py:399-440` reads the company turn pointer
   first; react does not.
3. Even on ordinary turns, the pick is the newest inbound of a conversation
   (`slack_intake_common.py:1015-1029`), not the turn being answered.
4. A fix needs the form with no ids to read the same turn pointer
   reply-current reads, and to refuse (exit 1, nothing placed) when it
   cannot prove the turn. Or it drops that form. Either way, a test over
   stale ordinary history, stale mention-only history and no history.
5. Four places still print the form with no ids and should name both ids:
   - the address-by-handle reminder (`adapter/main.go:6662`)
   - the coalescer's how-to (`adapter/inbound_coalescer.go:3470`)
   - bind-room (`scripts/slack_chat_bind_room.py:42`, `:89`)
   - the command's help (`commands/react/help.md:10`, `:48-53`)

## Item 3: the rules run to the end of their section

The rules are now every bullet of the section they sit in, from the heading
above the first rule to the next heading, outside code blocks. A bullet
after a blank line or after a paragraph is still in that section. The test
names the other bullets that share Google Chat's "When to speak" section, so
any bullet it does not name counts as a rule.

The section reader also:

- reads an unindented line straight after a rule into that rule, as
  Markdown does
- refuses any line that stands between two rules
- counts every list marker (`-`, `*`, `+`, `1.`)

The probes are tests:

- a fifth rule in three places in each text (after a blank line at the end
  of the list, at the end of the section, before the first rule)
- four lines slipped among the rules in each text

At `a59a65c3` a fifth rule after a blank line passes that head's own 15
tests (`mutation-probes-a3ba794f.txt`, B1 and B2).

## Tests, red first

The invocation, from the tree's root, under `env -i HOME PATH`:

```
uv run --offline --with pytest python -m pytest -p no:cacheprovider tests/test_texting_rules.py slack-full/tests/test_slack_prompt_contract.py -q -rA
```

- `red-tests-4b12f74c.txt`: the first test commit over round 1's texts.
  **12 failed, 22 passed**: the nine fifth-rule probes (old helper), the
  react check on Slack, the WhatsApp sentence, the Slack contract test.
- `red-at-a59a65c3.txt`: the head's two test files over the texts at
  `a59a65c3`. **3 failed, 51 passed**: the react check on Slack, the
  WhatsApp sentence, the Slack contract test.
- `green-at-a3ba794f.txt`: **54 passed**.
- `fold-red-tests.txt`, `fold-green-tests.txt`: the self-review fold.
  Red: the WhatsApp sentence, then the Slack contract test. Green: 54
  passed.
- `mutation-probes-a3ba794f.txt`: 15 changes at the head, each in a fresh
  tree, all caught.
- `review-probes-a3ba794f.txt`: the self-reviewer's own 28 probes, rerun at
  the head. Four are not caught:
  - R5 is a legitimate reference that should pass.
  - R8 names no command.
  - S10 makes the same change in all three texts, which is held only text
    against text, as in round 1.
  - S14 is a blockquote, and no text uses one.
- `slack-fragment-go-template-render-a3ba794f.txt`: the fragment parsed
  and rendered with Go's text/template (round 1's program,
  `../slack-fragment-go-template-render.go.txt`), 8508 bytes.

## Suites

From a fresh clone at `a3ba794f`, under `env -i HOME PATH`:

- `make test`: **2744 passed, 126 skipped**, 10689 subtests passed
  (`suite-make-test-a3ba794f.txt`). Round 1 had 2705 passed at `7a7faffd`;
  this round takes the two files above from 15 tests to 54.
- `make test PYTEST_DIRS=whatsapp/tests`: **57 passed**
  (`suite-make-test-whatsapp-a3ba794f.txt`).

## Review

### Self-review

A fresh reviewer on the same model read the round's delta against the
bead's contract and both records: BLOCKER 0, MAJOR 0, MINOR 5, NIT 3. Each
outcome:

| Finding | Outcome |
| --- | --- |
| MINOR 1: in a coalesced batch the label list could put the reaction on another member | FIXED `5917d03a`: the batch sentence |
| MINOR 2: reminders that print the form with no ids | FIXED `5917d03a`: "even where a reminder prints a react command that leaves them out"; the places are listed under the command's defect |
| MINOR 3: an unindented line in a rule, or a paragraph between rules, passed | FIXED `5917d03a`, with probe tests |
| MINOR 4: the react check read one line at a time | FIXED `5917d03a`: paragraphs and code-block commands read whole; only the reference sentence's own span is exempt |
| MINOR 5: the WhatsApp sentence pulled against the older line about an owner's reaction | FIXED `5917d03a`: the bracket |
| NIT 1: legitimate forms flagged | FIXED `5917d03a`: `--flag=value`, a backslash continuation, "When `gc slack react` fails". A new bullet in Google Chat's "When to speak" still fails: INTENDED, the test names that section's other bullets |
| NIT 2: a fifth rule with `*` or `1.` passed | FIXED `5917d03a` |
| NIT 3: a company room's mention-only lane is found by the lookup | WRONG for the sentence: such a session gets the mention-only reminder (`adapter/mention_only.go:783`), not a company turn |
| Its probe S11: a wrapped line starting with `#` ended the section | FIXED `a3ba794f` |

### Codex round

One round, class STANDARD, on the round's delta `a59a65c3..99b3f8be`
(the code head plus this directory): **VERDICT: CLEAN**, no finding at any
severity (`codex-round1-99b3f8be.md`). The reviewer ran no tests (read-only
sandbox) and read the transcripts here.

## Public repository

Before the push I read the round's whole delta, every commit message of
the round, and PR 91's title and body against the public-repository rule. The pattern
covered home and temp paths, the customer names on the rule's list, Slack,
phone, user and message ids, email addresses, keys and tokens, hosts and
ports, and the gate tool's route line.

- The texts, tests and README carry only the help page's placeholders
  (`Cxxx`, `1234.5678`, `spaces/<id>/messages/<id>`).
- The pytest transcripts quote the WhatsApp skill's own description line,
  which is on main.
- The commit authors are the noreply addresses already on main.

Four things were changed in the evidence copies:

1. The codex record's route line, which carried a detail of the gate tool's
   own setup, now reads "the hub".
2. The codex record's lane worktree path is now "the lane worktree".
3. The codex record's link into the reviewer's temp directory now points at
   README.md.
4. The suite transcript's clone path is now `<clone>`.
