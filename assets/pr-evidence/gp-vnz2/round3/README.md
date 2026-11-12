# gp-vnz2 round 3 (bead gp-j6vs): the third mail, the stopped mail and the founders' rules pinned

Round 3 of PR 95, on the branch gp-vnz2, from
`3962819653f88e1386a516e004e55f3e3adc1cb4`, which is round 2's head as it
reads after the PR was rebased onto main.

- The skill: `follow-through/skills/follow-through/SKILL.md`
- Its test: `follow-through/tests/test_follow_through_skill.py`

Everything below was produced at the code head
`2f96198daf134d6ed59e00f2ccc96e6e5073fc2d`. This directory is the record
at the new head.

## What moved under the PR

PR 100 changed how the pack delivers mail. It added a third mail, "follow-through:
no daytime hours", and a stopped state for a mail that fails five times. The
skill and its test were unchanged by the rebase, so the three findings below
are the skill left behind by the pack, not faults of rounds 1 and 2.

## The three findings, by class

**1. A mail the pack sends that the skill has no rule for.** The case: the
mail "follow-through: no daytime hours", planned for a conversation whose
recorded hours leave no time between 08:00 and 20:00
(`follow-through/src/engine.js`, `plan`). The skill named two subjects and
had no section for the third, so round 2's own class test was red at the
round's start. The tests:

- `test_the_opening_names_every_mail_the_pack_sends` (new): the skill's
  opening, the text before its first section, names the subject of every
  mail the pack sends, read from the pack's source.
- `test_each_mail_the_pack_sends_has_a_rule_for_arriving_twice` (round 2's,
  unchanged but for its docstring): the section for each mail says a repeat
  gets nothing. Its docstring now says why a repeat is rare: the pack looks
  for a mail in the city's mail history before it sends that mail again.
- Entry 24 of the founders' rules below carries Afik's rule for this mail.

**2. A reminder the owner was told is set, which the system then never
gives, with no sentence for it.** The cases: a reminder whose mail stopped
after its last try (`engine.js`, `failed`, and the send loop skips a stopped
raise for good); and, on WhatsApp, a reminder held for a closed window and
lost with the session (MINOR 5 of the read, fixed by the set-time sentence
below). The test:

- `test_the_health_section_says_what_a_stopped_mail_of_each_kind_means`
  (new), one case per kind of mail. The Health section names the line status
  prints for a stopped mail, read from `follow-through/src/cli.js` (today
  "Mail delivery stopped"), and each kind of mail as status prints it, read
  from the second argument of each `makeRaise` call (today `follow-up`,
  `hours-notice` and `reminder`). A kind added later fails until the section
  says what its stopped mail means.
- `test_every_mail_kind_and_the_stopped_line_are_read` (new) keeps the first
  from passing on nothing: every raise has a kind the test reads, and status
  has exactly one stopped line it reads.

**3. A founder's rule in the skill that no test pins.** The test:

- `test_each_founders_rule_stands_word_for_word_in_its_section` (new), one
  case per entry of `FOUNDERS_RULES`. Each sentence must stand word for word,
  after whitespace is joined, inside the section with its heading. A removed
  sentence, a changed word, a sentence moved to another section and a removed
  section each fail, and the failure names the rule. It replaces
  `test_the_sentences_the_founders_rule_on_are_there`, whose two sentences
  are now entries 5 and 23.

## For the founders to see

Each passage is quoted from the skill at the head exactly.

### The opening names all three mails

> Read this before you open an item, before you write any follow-up, and
> whenever a mail with the subject "follow-through: follow-up due",
> "follow-through: reminder due" or "follow-through: no daytime hours" reaches
> you.

### The new section for the third mail

> ## No daytime hours
>
> A mail with the subject "follow-through: no daytime hours" names one
> conversation whose recorded hours leave no time between 08:00 and 20:00. No
> follow-up will come in that conversation, and the owner's reminders are not
> affected. The mail can come at any hour, whether the owner's consent is on,
> off or never recorded. It comes from the pack, not from the owner, and you
> never reply to it.
>
> Never write to the owner because of this mail: wait until they next write to
> you. When they do, look in the conversation the mail names. If it shows you
> already told them what the Consent, hours and zone section above says to
> tell an owner whose hours are all at night, there is nothing more to say.
> Otherwise tell them that, once, if `gc follow-through status` still shows
> that conversation with no recorded hours between 08:00 and 20:00.
>
> This mail can reach you twice for one conversation, and the second is the
> same notice arriving a second time. Send nothing for it.

The rule of the second paragraph is Afik's, of 2026-11-12: on this mail the
mayor does not write to the owner, and it brings the matter up the next time
the owner writes. The Consent, hours and zone section already has the mayor
tell such an owner, when they give those hours, that it will not come back to
things they leave open. So the owner is told at most once. The last condition
("if `gc follow-through status` still shows ...") came from the self-review:
an owner whose hours have since gained the daytime is not told something
false.

### The three corrected sentences

In Reminders, a reminder comes soon after its time unless its mail stopped:

> A reminder comes at any hour and needs no consent and no zone. It comes soon
> after the time the owner chose, unless its mail delivery stopped. The Health
> section below says what you do then.

In Reminders, a refused cancel:

> Tell the owner it is cancelled only when the verb says so. If it answers
> that the reminder has already been raised, the pack had already tried to
> send you that reminder. Its mail may already have reached you, may still
> come, or may never come if its delivery stopped. Tell the owner it was
> already on its way to you and that you will not pass it on, and keep to that
> in every case, a stopped reminder included.

In Health, the stopped mail. The first paragraph keeps its first three
sentences; its last sentence ("Never make up for it by writing to the owner
yourself.") is replaced by one rule per kind:

> `gc follow-through status` shows the pack's health and never shows a summary
> or a reminder's text. Its count of failing raises also counts a mail planned
> a moment ago and not yet tried, so a count above zero is not a fault by
> itself. If the same raise is still there later, with attempts and an error,
> tell the operator.
>
> A `Mail delivery stopped` line counts mails the pack will never send again,
> and the pack will not tell you about them, so tell the operator at once.
> Each such mail also has a line of its own, with its kind, its conversation
> and the word stopped. What you do depends on its kind:
>
> - For a stopped `follow-up`, send nothing, and never make up for it by
>   writing to the owner yourself. Its items stay open, and the pack will not
>   raise them again.
> - For a stopped `hours-notice`, do what the No daytime hours section above
>   says, as if its mail had come.
> - For a stopped `reminder`, you still give it, because the owner was told it
>   is set. Find it with `gc follow-through remind --list`: it is a reminder of
>   that conversation still shown as set after its time. Then do what the
>   Reminders section says for a reminder mail that says it is late, as if
>   that mail had just come.

### The stopped reminder

The last bullet above is the mayor's proposal, not yet a founder's word. It
points to the Reminders section rather than repeating it. That section has
the mayor look in the conversation, and on any channel the WhatsApp section
sent it to, not in what it remembers. If the owner cancelled the reminder
there, or the mayor already gave it there after its time, naming that time,
it sends nothing. Otherwise it gives the reminder now, says plainly that it
is late with the time the owner asked for, and does not explain why. A
reminder whose cancel was refused is one the owner cancelled in the
conversation, so it is never given.

### The rows of the read's minor findings ruled to be fixed here

MINOR 4 is the refused-cancel sentence above.

MINOR 7, in Consent, hours and zone (entry 15 below takes these words):

> When the owner says stop, record off at once for every conversation you have
> with them, before anything else in that turn.

MINOR 7, in WhatsApp, added after "Say it on another channel the owner uses
with you, or hold it and say it when the owner next writes.":

> On another channel, a follow-up goes only to a conversation where the
> owner's consent is on.

MINOR 5, in WhatsApp, a new paragraph:

> When you set a reminder on WhatsApp for a time after the owner's current
> window closes, the short line that tells them it is set also says how it
> will reach them if they have not written to you in the day before it falls
> due: on the other channel you will use, or when they next write.

MINOR 9, in Reminders. Before: "Name the time the reminder was for, because
two reminders can have the same words and you give both." Now:

> Name the time the reminder was for, because two reminders can have the
> same words. They are still two reminders, and you give each one at its own
> time.

MINOR 9, in The one follow-up: "Never write about those items again." is
removed. "There is no second follow-up, ever." states the rule, and the
removed line also forbade answering the owner about those items. The
paragraph now reads:

> When at least one item goes in, send one short message in that conversation
> that covers every item that goes in, the way that channel's skill says.
> When no item goes in, send nothing.

## The founders' rules, as the test holds them

24 entries. The source "contract item N" is the item of the skill's own
contract (round 1) that the sentence carries.

| # | name | source | section | sentence |
| --- | --- | --- | --- | --- |
| 1 | never for work you owe | contract item 2 | When to open an item | Never open one for work you owe. |
| 2 | never for small talk | contract item 2 | When to open an item | Never open one for small talk. |
| 3 | unsure means out | contract item 3 | The one follow-up | If you are unsure, it stays out. |
| 4 | none in, nothing sent | contract item 3 | The one follow-up | When no item goes in, send nothing. |
| 5 | one follow-up | contract item 3 | The one follow-up | There is no second follow-up, ever. |
| 6 | no blame | contract item 4 | What a follow-up sounds like | Write nothing that blames, such as "you forgot" or "still waiting on you". |
| 7 | no "just checking in" | contract item 4 | What a follow-up sounds like | Never write "just checking in". |
| 8 | nothing added | contract item 4 | What a follow-up sounds like | Add no offer, greeting or news to it. |
| 9 | only on the pack's mail | contract item 5 | When you may not write | Write a follow-up only because a follow-through mail asked for it. |
| 10 | never with consent off | contract item 5 | When you may not write | Never write a follow-up to an owner whose consent is off. |
| 11 | the daytime bound | contract item 5 | When you may not write | Write one only between 08:00 and 20:00 in the owner's time, and inside the hours they gave you. |
| 12 | outside the hours, nothing | contract item 5 | When you may not write | If it is now outside those hours, send nothing for those items. |
| 13 | unrecorded consent is off | contract item 6 | Consent, hours and zone | Consent that was never recorded counts as off. |
| 14 | ask once, in their conversation | contract item 6 | Consent, hours and zone | Ask the owner once, in a conversation they started, whether they want you to come back to things they leave open. |
| 15 | stop at once | contract item 6 | Consent, hours and zone | When the owner says stop, record off at once for every conversation you have with them, before anything else in that turn. |
| 16 | the zone never guessed | contract item 6 | Consent, hours and zone | Never guess it, and never use the box's own. |
| 17 | a reminder never only in the session | contract item 7 | Reminders | Never keep a reminder only in your own session. |
| 18 | told set only once stored | contract item 7 | Reminders | Only then tell the owner it is set: react with ⏰ where the channel lets you react, and otherwise answer with one short line that says when. |
| 19 | cancelled only on the verb's word | contract item 7 | Reminders | Tell the owner it is cancelled only when the verb says so. |
| 20 | no free text after the window | contract item 8 | WhatsApp | A follow-up or a reminder that comes due after that window has closed is not sent there as free text. |
| 21 | never the update template | contract item 8, Afik's WhatsApp rule | WhatsApp | It is never sent as the update template either: by Afik's rule that template is only for an actual update, and a follow-up or a reminder is not one. |
| 22 | no unasked news | contract item 9 | News about yourself | Never write to the owner, unasked, about an update to yourself. |
| 23 | a major feature only | contract item 9, Afik's word on release notes | News about yourself | A note about a new ability goes out only when you were given one for a major feature. |
| 24 | no message on the no-daytime-hours mail | Afik, 2026-11-12 | No daytime hours | Never write to the owner because of this mail: wait until they next write to you. |

Entry 15 takes MINOR 7's new words. Entry 24 is this round's new sentence.
Every other entry is the sentence as it read at the round's start.

## The test, red first, then green

Each run is from the repository root with
`env -u GC_BIN make test PYTEST_DIRS=follow-through/tests PYTEST_ARGS='-q -rA --tb=short'`.

- `red.txt`: the tests at the head, with the skill as `39628196` has it, in a
  clean clone: **7 failed, 39 passed**, make exit 2. The failing cases:
  - `test_each_founders_rule_stands_word_for_word_in_its_section[stop at once]`
    (entry 15 holds MINOR 7's new words)
  - `test_each_founders_rule_stands_word_for_word_in_its_section[no message on the no-daytime-hours mail]`
    (no section headed No daytime hours)
  - `test_the_opening_names_every_mail_the_pack_sends[follow-through: no daytime hours]`
  - `test_the_health_section_says_what_a_stopped_mail_of_each_kind_means[follow-up]`,
    `[hours-notice]` and `[reminder]` (the Health section names no stopped line)
  - `test_each_mail_the_pack_sends_has_a_rule_for_arriving_twice[follow-through: no daytime hours]`
    (round 2's test, red at the round's start already)
- `green.txt`: at the head: **46 passed**, make exit 0.

The same tests were first committed alone (`cc3257f6`) and run there against
the unchanged skill, with the same 7 failures. Entry 24's words were changed
by the self-review after that, so `red.txt` is recorded again with the tests
of the head.

## The breaks

`breaks.txt` holds one block per break: its name, the failing cases, the
counts and the exit code. `breaks.py` made them: each break is a fresh copy
of the pack with one edit to the skill, and the skill's test run in it. The
unchanged head is green (41 passed).

**The 48 breaks of the founders' rules.** For each entry: (a) its sentence
removed, and (b) its sentence replaced by its reversal, as the round's
contract lists them. **48 of 48 are red on that entry's own case**, and on
nothing else.

**The read's breaks M01 to M16**, replayed with the read's own edits (matched
with any whitespace between words, because this round rewrapped some lines):

| break | result | failing case |
| --- | --- | --- |
| M01 the consent sentence removed | RED | never with consent off |
| M02 20:00 changed to 22:00 in both places | RED | the daytime bound |
| M03 a second follow-up allowed, the pinned sentence kept | GREEN | none (see below) |
| M03b "There is no second follow-up, ever." removed | RED | one follow-up |
| M04 the update template allowed after the window | RED | never the update template |
| M05 a flag renamed (--summary to --title) | RED | test_every_flag_in_the_skill_is_one_its_verb_takes |
| M06 "If it is now outside those hours, send nothing" removed | RED | outside the hours, nothing |
| M07 the section When you may not write removed | RED | its four entries |
| M08 the WhatsApp section removed | RED | its two entries |
| M09 the zone may be guessed from the box | RED | the zone never guessed |
| M10 the major-feature sentence removed | RED | a major feature only |
| M11 a follow-up may be written from the list, without a mail | RED | only on the pack's mail |
| M12 the reminder's arriving-twice rule removed | RED | the reminder case of the arriving-twice test |
| M13 ask for consent every week, in any conversation | RED | ask once, in their conversation |
| M14 consent never recorded counts as on | RED | unrecorded consent is off |
| M15 the pinned sentence kept and contradicted by the next sentence | GREEN | none (the limit below) |
| M16 a rote section for the third mail in place of this round's | RED | no message on the no-daytime-hours mail |

Two replays needed this round's text:

- M03: the read's edit rewrote "Never write about those items again." into
  "Write about those items again after a week if the owner has not
  answered." This round removed that sentence (MINOR 9), so the replay puts
  the read's words where it stood. Nothing pinned is removed or changed, so
  it stays green. It is the same limit as M15: an added sentence that
  contradicts a pinned one.
- M16: at the read's head the skill had no section for the third mail, so
  the read's rote section was the only one. The replay puts it in place of
  this round's section, and entry 24 is red. The read's edit as written, the
  rote section added beside this round's, is recorded too as
  `M16-as-written`. It removes nothing and stays green.

### The limit of a text test

The test pins presence and place, not meaning. A pinned sentence kept, with
a sentence beside it that says the opposite, stays green (M15, and M03 at
this head). Only a reader can see that. The founders' list makes every
removal, rewording or move of a founder's sentence red. It does not stop a
sentence from being added.

## The suites

- `env -u GC_BIN make test PYTEST_DIRS=follow-through/tests PYTEST_ARGS='-q -rA --tb=short'`
  at the head: 46 passed, make exit 0 (`green.txt`). At the round's start the
  same suite had 1 failed, 15 passed.
- The pack's Node suite as CI runs it, `cd follow-through/test && env -u GC_BIN node --test`,
  at the head on Node v26.12.1: 108 tests, 108 passed, 0 failed, exit 0
  (`node.txt`). The pack's code does not change in this round; this is a guard.
- The repository's whole Python suite was not run here, because one of its
  tests starts a real gc binary. CI runs it on the push.

## Review

A fresh self-review on the lane's own model read this round's delta,
`39628196..8c9a5f4e`, against the round's contract, the founders'
list, the words rule and the public-repository rule. It re-ran the red and
green runs and the 48 breaks on its own, and four more breaks of its own:
the stopped `hours-notice` bullet removed, the stopped line renamed, the
third subject dropped from the opening, and the new section's repeat rule
removed. Each turned only the expected case red. Its counts: 0 BLOCKER, 0
MAJOR, 2 MINOR, 4 NIT. The outcomes, all in `2f96198d` where fixed:

- MINOR: the WhatsApp set-time sentence said "if they have not written again
  by then", which is not the window's rule: an owner can write and still have
  a closed window when the reminder falls due. FIXED: it takes the ruling's
  condition, "if they have not written to you in the day before it falls
  due", and no longer reads "it is set how".
- MINOR: an owner whose hours gained the daytime after the third mail came
  would be told follow-ups will not come. FIXED: the owner is told only while
  status still shows that conversation with no recorded hours between 08:00
  and 20:00 (the words status prints, `follow-through/src/cli.js`).
- NIT: "bring it up only the next time they write to you" could read as
  permission. FIXED: "wait until they next write to you"; entry 24 takes the
  new words.
- NIT: "the section above" did not name its section. FIXED: "the Consent,
  hours and zone section above".
- NIT: two reminders with the same words and the same time are given once
  under the repeat rule, while the new sentence says they are two. INTENDED:
  the words are the ruling's, and the read graded the same-time case
  harmless.
- NIT: no verb shows consent for one conversation, or which conversations
  belong to one owner, so a mayor without its session checks MINOR 7's two
  rules from the conversation. DOES NOT MATTER for this round: it is the same
  gap as the read's MINOR 6, which is a follow-up on the pack.

## What the rebase made stale in rounds 1 and 2

The PR was rebased onto main after PR 100 merged, so the files of rounds 1
and 2 describe a branch that no longer exists. Round 1's `README.md` says
the PR is stacked on PR 93's branch (`gp-91f7` at `8e78e97c`); it now sits on
main. Their counts are the old suite's: round 1's `green.txt` has 12 passed
and round 2's `green.txt` has 15 passed, while the same suite now has 46.
Their whole-suite counts are from before the rebase. The commit ids they name
were rewritten by the rebase, with the same patches (same patch ids):
`b810d773` is now `242580e4`, `b04ae938` is `9f0259c2`, `2fb7e8e5` is
`3591cc15`, `68a34b14` is `d17edc8f`, and `5729f906` is `e46d3fdf`. Those
files are left as they are. This directory is the record at the new head.
