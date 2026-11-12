# gp-bis1: PR 91 round 4 (a catalog of reactions, bead gp-adh3)

Afik, 11/12, on the merge card for this PR: "For 91 we should support more
reactions because Jev can support up to 256 reactions." Five was too few.
This round turns the five into a catalog of reactions with fixed meanings,
held in one data file that the three texts show word for word and that an
adapter's Jev choice can read later. No adapter or command code changes,
and no change to how a reaction is placed.

- Round base: `ce0187006b0f216a487173b2eefe789998f6cf29` (PR 91's head after round 3)
- Code head: `763bca67b1debea6d53fee712fd3b5e48c9a1ef3`. The commits after it
  touch only this directory.
- Commits:
  - `d6ff03e4` the tests (red)
  - `bf972c92` the catalog, `reactions.json` in three packs
  - `2feaed1b` the texts and the changelogs
  - `93f56ba6` the self-review's tests (red)
  - `763bca67` the self-review's fixes
- No merge: main (`c546802d`, PRs 93 and 94) merges onto the head without a
  conflict (`git merge-tree --write-tree` exits 0).

## The limit, as read

The vendor's page https://docs.typesafe.ai/primitives/choice.md, read
2026-11-12 at 16:40Z, says: "A Choice question accepts up to 255 options".
The founder said 256; the test holds the catalog at 255 or fewer
(`JEV_CHOICE_LIMIT` in `tests/test_reaction_catalog.py`). The catalog has
18 rows, so the limit is far off; it would matter only for a catalog grown
by an order of magnitude.

## The catalog

`gchat/reactions.json` is the source. `slack-full/reactions.json` and
`whatsapp/reactions.json` are the same bytes, held so by a test: the three
packs ship separately, as `whatsapp/media-types.json` is one file read by
the WhatsApp adapter and its gateway. The file is JSON, so the Go adapters
and the Node adapter read it with their standard libraries.

Each row has four fields, in this order: `emoji` (the character),
`slack_name`, `meaning` (what the owner reads the reaction as) and
`adapter_may_place` (true for a row that can be true the moment a message
arrives; false for a row that claims a finished fact). An `about` line
says the same in prose.

| Reaction | Slack name | What the owner reads it as | Who may place it |
| --- | --- | --- | --- |
| 👀 | `eyes` | "I have your message and I am on it." | the adapter, or the mayor |
| 🔍 | `mag` | "I am looking this up." | the adapter, or the mayor |
| 👍 | `+1` | "Understood. Nothing more needs saying." | the adapter, or the mayor |
| 🤔 | `thinking_face` | "I am thinking this over before I give you my view." | the adapter, or the mayor |
| ❤️ | `heart` | "You are welcome; I am glad it helped." | the adapter, or the mayor |
| 👋 | `wave` | "Hello to you too, or goodbye for now." | the adapter, or the mayor |
| 🎉 | `tada` | "That is great news." | the adapter, or the mayor |
| 😢 | `cry` | "I am sorry; that is sad news." | the adapter, or the mayor |
| 😂 | `joy` | "That made me laugh." | the adapter, or the mayor |
| 🤞 | `crossed_fingers` | "Good luck; I hope it goes well." | the adapter, or the mayor |
| ✅ | `white_check_mark` | "Done, and checked." | the mayor, once true |
| ⏰ | `alarm_clock` | "The reminder is set." | the mayor, once true |
| 📥 | `inbox_tray` | "I have the attachment you sent, and it opens." | the mayor, once true |
| 📤 | `outbox_tray` | "It is sent." | the mayor, once true |
| 📅 | `date` | "It is booked." | the mayor, once true |
| 💳 | `credit_card` | "It is paid." | the mayor, once true |
| 📝 | `memo` | "Noted, and saved where I will find it again." | the mayor, once true |
| ⚠️ | `warning` | "It did not work; my reply says why." | the mayor, once true |

**18 rows: 10 an adapter may place, 8 claims.** The five of rounds 1 to 3
keep their meanings and their kind.

Each emoji is the character Slack's own table gives its name
(iamcal/emoji-data at `13ee711e`, the commit the Slack adapter's name list
is generated from): `catalog-characters-763bca67.txt`. ❤️ and ⚠️ carry
U+FE0F, as that table has them. A test pins each name to its codepoints.

### How the rows were chosen

The measure for a row: an owner reads it right without a legend, and it
means something no other row means. What people send a working assistant,
and the row that answers it:

| A message that is | The row |
| --- | --- |
| a request | 👀, then ✅ or a narrower claim once it is done |
| a question to look up | 🔍 |
| a question that asks for a view, or a decision to make | 🤔 |
| a decision or a correction that needs nothing more | 👍; 👀 when it changes the work |
| a deadline | 👍, then ⏰ once a reminder is stored |
| thanks or praise | ❤️ |
| an apology | 👍 |
| good news | 🎉 |
| bad news | 😢 |
| something coming up (a pitch, an interview) | 🤞 |
| a joke | 😂 |
| a greeting or a sign-off | 👋 |
| a file, a photo or a voice note | 👀 when it asks for work; 📥 once the mayor has opened it |
| something to remember | 📝 once it is saved |
| a request that fails | ⚠️ with the reply that says why |

Where a narrow claim fits (the reminder, the attachment, sent, booked,
paid, noted), it is the answer, and ✅ is for finished work that none of
them names. Round 1's ⏰ beside ✅ was the same.

**Left out, and why.**

- An emoji an owner reads two ways: 🙏 is "thank you" and "please"; ❌ is
  "no", "wrong" and "failed"; ↩️ looks like "reply".
- A meaning another row already has: 💯, 👌, 🤝 and 🫡 beside 👍; 🙌, 👏 and
  🔥 beside 🎉. Two rows with one meaning give two best answers.
- A row that needs words to be any use: ❓ for a question back, 👎 for a
  refusal.
- An opinion the adapter cannot know on arrival: 💡 "good idea", 😮 "that is
  surprising".
- 🗑️ for "deleted": on someone's message it can read as "this is rubbish".
  Cancelled and deleted are ✅ once done.
- The hourglass: the Slack adapter already uses it as its busy mark
  (`BUSY_REACTION`, `slack-full/adapter/busy_reaction.go:36`).

### True on arrival, or a claim

A row an adapter may place says something that can be true the instant a
message lands. A row that claims a finished fact is placed only by the
mayor, only once the fact is true. Round 1's rule "✅ and ⏰ are yours, and
never a promise" is now rule 3 for every row of that kind.
`test_no_claim_is_placeable_by_an_adapter` fails when a claim is marked
placeable, and `test_every_row_has_its_kind_named_here_and_in_the_file`
makes every new row's kind a decision written in the test as well as in
the file.

📥 was first a row an adapter may place. The self-review moved it: an
attachment can arrive unavailable (Google Chat's `unavailable` line,
WhatsApp's "media store unavailable"), and an adapter's choice sees only
the text, so "I have the attachment" is a claim the mayor checks by
opening it.

**Found on the way, not changed here.** With `SLACK_COMPANY_VISIBLE_ACKS`
set (off by default), the Slack adapter's company-room acknowledgement
swaps its 👀 for ✅ when a message is delivered to the agent, and for ⚠️
when delivery fails (`slack-full/adapter/company_acks.go:125-128` and
`:149`). In such a room ✅ means "delivered", not "Done, and checked", and an
adapter places two of the claims. This is adapter code, outside this round.

## The texts

The Google Chat skill, the Slack prompt fragment and the WhatsApp skill
show the whole catalog in two tables, built from the file, then the four
rules. Round 2's sentences on naming the message, each channel's own lines
and the reply-length lines are unchanged. Every sentence that changed, old
above new (the same in all three texts unless named):

The intro and the table leads:

> Five reactions have fixed meanings, the same on Slack, Google Chat and WhatsApp:

> These reactions have fixed meanings, the same on Slack, Google Chat and
> WhatsApp. Each one in the first table can be true the moment a message
> arrives, so the adapter may place it, and so may you on a channel where
> you can react:

and, new, between the tables:

> Each one in the second table states a finished fact, so it is yours to
> place, and only once the fact is true:

Rule 1:

> **A reaction comes first.** On a channel where you can react, your first
> answer to a message addressed to you is 👀, 🔍 or 👍, placed before the work
> starts, and never a sentence that only says the message arrived. The
> adapter may already have placed one of these three when the message came
> in. A second one from you does no harm.

> **A reaction comes first.** On a channel where you can react, your first
> answer to a message addressed to you is the reaction from the first table
> that fits it, placed before the work starts, and never a sentence that
> only says the message arrived. The adapter may already have placed one
> from that table when the message came in. A second one from you does no
> harm.

Rule 2:

> **A reaction alone.** On a channel where you can react, a message to you
> that only confirms, thanks, agrees or closes a topic gets 👍 as the whole
> answer. No text follows it.

> **A reaction alone.** On a channel where you can react, a message to you
> that only confirms, thanks, agrees or closes a topic gets the reaction
> from the first table that fits it as the whole answer. No text follows it.

Rule 3:

> **✅ and ⏰ are yours, and never a promise.** ✅ goes on a request only after
> the work is done and checked. ⏰ goes on a request for a reminder only
> after the reminder is really stored.

> **The second table is yours, and never a promise.** A reaction from it
> goes on a request only after what it says is true: ✅ once the work is
> done and checked, ⏰ once the reminder is really stored, and so on for
> each.

Rule 4:

> **Fixed means fixed.** Use the five only for their meanings. Other emoji
> are still fine for warmth or for a topic, but never in place of one of the
> five.

> **Fixed means fixed.** Use these reactions only for their meanings. Other
> emoji are still fine for warmth or for a topic, but never in place of one
> of these.

WhatsApp only:

> The adapter may place one of the first three when a message arrives.

> The adapter may place one from the first table when a message arrives.

The changelog bullet of this PR in each pack says catalog in place of
five, and names the file.

## How much a text carries inline

Each text is read whenever its channel is in play. Two options, measured
against the texts at the round's base (`inline-options-763bca67.txt`;
tokens by tiktoken 0.14.0 `o200k_base`, an OpenAI encoding: Claude's own
tokenizer is not public, so read the tokens as a scale):

| Option | Google Chat skill | Slack fragment | WhatsApp skill |
| --- | --- | --- | --- |
| (a) the whole catalog inline (built) | +1,149 chars, +351 tokens | +1,149 chars, +352 tokens | +1,151 chars, +352 tokens |
| (b) the five inline, the file's path for the rest | +718 chars, +165 tokens | +680 chars, +156 tokens | +720 chars, +166 tokens |

Both options rewrite the intro, the table leads and the rules; that is most
of (b)'s growth. (a) costs about 430 characters and 186 tokens more per
text than (b). The texts as built are (a). (b) was built from them by a
script (`inline-options-make-b.py.txt`) that keeps the five rows and adds
one paragraph after the second table; what it changes is
`inline-option-b-vs-a.diff.txt`.

Why (a):

- The mayor can keep "Fixed means fixed" only for the rows it has read. Under
  (b) it would use 🎉 or ❤️ freely, not knowing they are catalog rows.
- A reaction comes first. Under (b), any reaction outside the five waits on
  a file read.
- The Slack fragment is rendered into the prompt and has no path the mayor
  can open. A skill can say `../../reactions.json`, but the skill directory
  a session sees is a symlink into the pack cache, so a tool that resolves
  `..` by the text of the path misses the file.

## Tests

New in `tests/test_reaction_catalog.py`:

- each row has the four fields, in order; the loader refuses a key given
  twice in one row
- emoji, Slack names and meanings are unique
- every Slack name is one the Slack adapter knows
  (`slack-full/adapter/slack_emoji_names.go`)
- each emoji is the character Slack's table gives its name
- the count is above five and at most 255
- the five keep their meanings
- no claim may be placed by an adapter; every row's kind is named in the
  test
- the copies are the source, byte for byte

New or changed in `tests/test_texting_rules.py`:

- each text carries the whole catalog in its two tables, word for word and
  in the catalog's order
- the tables are the same in every text
- every reaction a text shows is a catalog row; outside the tables, only
  the reactions the test names (rule 3's two examples, and in Google Chat
  an inbound reaction's line and the react example)
- outside the tables, no backticked Slack emoji name, except Google Chat's
  `thread`, which is a message field
- the intro and the two leads come in order, each right above its table
- no text counts the reactions
- the rules' tests read rule 3's new lead; the WhatsApp sentence is the new
  one

`slack-full/tests/test_slack_prompt_contract.py` reads rule 2's new
sentence.

Runs:

- Red at `d6ff03e4` (the base with the round's tests): 50 failed, 34 passed
  (`red-at-d6ff03e4.txt`). The catalog tests fail on the missing
  `gchat/reactions.json`. The text tests fail on one table where two are
  wanted, on "Five", on the old intro, on rule 3's old lead and on
  WhatsApp's "first three". The tests of the tests' own helpers pass there,
  as they should. The scan's probe test fails there only because the
  catalog is missing; the mutation probes show its own red.
- Red at `93f56ba6` (the self-review's tests over the texts before its
  fixes): 5 failed, 97 passed (`fold-red-at-93f56ba6.txt`).
- Green at `763bca67`: 102 passed (`green-at-763bca67.txt`).
- 22 mutation probes, each writing one defect into a fresh copy of the
  files the tests read, each caught (`mutation-probes-763bca67.txt`; the
  script is `mutation-probes.py.txt`). The unchanged copy passes 100. The
  repo has 102 because the copy has no Slack prompt-contract test.
- The Slack fragment parses and renders as a Go template
  (`slack-fragment-go-template-render-763bca67.txt`).
- Suites from a clean clone at `763bca67` under `env -i`:
  - `make test`: 2796 passed, 126 skipped, 2 warnings (the fork
    DeprecationWarning in `test_slack_company_outbound.py`, outside this
    delta), 10734 subtests passed, rc 0 (`suite-make-test-763bca67.txt`)
  - `make test PYTEST_DIRS=whatsapp/tests`: 57 passed, rc 0
    (`suite-make-test-whatsapp-763bca67.txt`)
  - At `2feaed1b`, before the self-review's fixes, the same two runs gave
    2778 passed, 126 skipped, and 57 passed, both rc 0.

## Review

**Self-review** (a fresh reviewer on the builder's model, on
`ce018700..2feaed1b`): BLOCKER 0, MAJOR 2, MINOR 7, NIT 4. Each finding
that changed a file has its test first, in `93f56ba6`, red before the fix.

- M1, 📥 was placeable by an adapter but is not true on arrival: FIXED in
  `763bca67` (now a claim, "I have the attachment you sent, and it
  opens."). Test: the kind sets in `tests/test_reaction_catalog.py`.
- M2, rows can tie for a classifier that sees only a meaning and a
  message: FIXED IN PART in `763bca67` (❤️ says it answers thanks, 😢 says
  bad news, so a thank-you and an apology no longer pull the wrong row).
  The rest is a design point for the adapter bead: a sentence written as
  the owner reads it says what the reaction answers back, not what kind of
  message it answers, and a message that mixes two kinds (thanks and a
  goodbye) fits two rows.
- m1, the Slack adapter's company acknowledgement already places ✅ and ⚠️
  with other meanings: NOT CHANGED (adapter code, out of this round);
  written above under "Found on the way".
- m2, nothing tied a row's emoji to its Slack name: FIXED, the codepoint
  pin (`93f56ba6`); probe 18 is the reviewer's case.
- m3, the scan missed a catalog emoji given another meaning in prose, a
  skin tone, a Slack name in prose, and a selector-made emoji: FIXED
  (`93f56ba6`); probes 19 to 21 and the scan's probe test.
- m4, the WhatsApp text said the mayor may place a reaction and then that
  it cannot yet: FIXED in `763bca67` (the first lead says "and so may you
  on a channel where you can react").
- m5, an apology had no row and pulled 😢; a voice note was not a "file
  or photo": FIXED in `763bca67` (😢 says bad news, so an apology takes 👍;
  📥 says attachment).
- m6, the model reads option names too, and some Slack names mislead
  (`joy`, `cry`, `date`): FIXED IN PART. The test's docstring no longer
  makes the Slack name the option name (`93f56ba6`); `bf972c92`'s commit
  message still says it would be, and the history is not rewritten. The
  option name is the adapter bead's choice.
- m7, a narrow ❤️ leaves the usual warmth emoji with a fixed meaning:
  INTENDED. Rule 4 keeps every emoji outside the catalog free for warmth,
  and a broad ❤️ would tie with 🎉 and 😂.
- n1, two changelog sentences pointed at the wrong thing: FIXED in
  `763bca67`.
- n2, "is true" on arrival said more than it meant, and so did the
  `about` line: FIXED in `763bca67`.
- n3, the count check caught only "five": FIXED (`93f56ba6`); probe 22.
- n4, some new tests are not red at the base for their own reason: said
  above under Runs; DOES NOT MATTER beyond that.

**Codex round** (STANDARD, on `ce018700..3a1144b3`, the whole round with
this directory): CLEAN, no finding. Record:
`codex-round1-3a1144b3.md`.

## For the adapter bead

What an adapter's Jev choice needs from the file, when a later bead has it
read the catalog:

1. Path: `reactions.json` at the root of each pack (`gchat/`, `slack-full/`,
   `whatsapp/`); the source is `gchat/reactions.json`, and every copy is
   listed in `COPIES` in `tests/test_reaction_catalog.py`.
2. Format: a JSON object; `reactions` is an array of rows, each with
   `emoji`, `slack_name`, `meaning` and `adapter_may_place`; `about` is
   prose.
3. The choice offers only the rows with `adapter_may_place` true, with
   `meaning` as each option's description. The option's name is the
   adapter's to choose: the model reads names too, and some Slack names
   mislead (`joy`, `cry`, `date`). The instructions should say how to pick
   between two rows that both fit a mixed message.
4. Placing: Slack by `slack_name`; Google Chat and WhatsApp by `emoji`
   (❤️ and ⚠️ carry U+FE0F).
5. Reading it: `go:embed` cannot reach above a Go module (`gchat/adapter`,
   `slack-full/adapter`), so a Go adapter reads the pack's file at runtime
   or keeps a copy inside its module, added to `COPIES`; the WhatsApp
   adapter can read `../../reactions.json` as it reads `media-types.json`.
