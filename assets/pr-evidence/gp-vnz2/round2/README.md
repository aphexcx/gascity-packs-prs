# gp-vnz2 round 2 (bead gp-nkj7): the same reminder mail twice

Round 2 of PR 95, on the branch gp-vnz2, from round 1's head
`b04ae9380fc5472aa4900785866841b90201aaca`.

- The skill: `follow-through/skills/follow-through/SKILL.md`, Reminders section
- Its test: `follow-through/tests/test_follow_through_skill.py`

Everything below was produced at the code head
`5729f9062600d09b446a1f0297a96f1bef8bf69e`.

## The finding, by its class

The class: a mail the pack sends that can arrive twice, handled by the skill
as if it were new. The pack can send one mail a second time: a tick that
dies after its mail is sent and before that is saved sends it again on a
later tick, and gc drops the repeat only while the first mail is unarchived
(`follow-through/README.md`, the delivery paragraph). The skill's follow-up
section already said a repeat gets nothing. The Reminders section told the
mayor to give every reminder mail that was not cancelled, so an owner could
be told the same reminder twice.

The cases: the reminder mail on time, and the reminder mail marked late. A
retried mail is written again at the time of the retry
(`follow-through/src/engine.js`, the send loop), so a repeat of a reminder
that first came on time can arrive marked late. The follow-up mail is the
case that was already covered.

## For the founders to see

The new sentences, quoted from the skill exactly. The first four sit right
after the cancel sentence and end "as for a cancelled reminder", so the
sentence that follows them, "Otherwise tell the owner now", covers both
reasons to send nothing. The last one sits right after that sentence.

> The same mail can also reach you twice for one reminder. Look in that
> conversation, and on any other channel the WhatsApp section below may have
> sent you to, not in what you remember, because a session ends. If you
> already gave the owner this reminder there after the time it was due,
> naming that time, it is the same reminder arriving a second time, even if
> this mail says it is late. Send nothing for it, as for a cancelled
> reminder.

> Name the time the reminder was for, because two reminders can have the
> same words and you give both.

How the mayor tells a repeat: the mail gives the reminder's words and the
time it was for; the conversation, or the channel used while a WhatsApp
window was closed, shows whether the mayor already gave the owner that
reminder, naming that time, after it was due. Two reminders with the same
words stay apart because every giving names the time its reminder was for.

What changes for an owner: every reminder now names the time it was for.
Before this round only a late one did.

The follow-up rule ("There is no second follow-up, ever." and the sentences
after it) and every sentence round 1 put before the founders are unchanged.
In the paragraph the round touched, the sentences that were there keep every
word (`git diff --word-diff` shows only added words).

## The test, red first, then green

`test_each_mail_the_pack_sends_has_a_rule_for_arriving_twice` takes the
subject of every kind of mail from the pack's source (`follow-through/src/*.js`,
where each raise is made with its subject), finds the skill's section that
names that subject, and asks there for a rule for the same mail arriving a
second time that ends in sending nothing: one such rule for each kind of
mail the section names. A kind of mail added later fails it until the skill
covers it.

`test_every_mail_subject_in_the_pack_is_read` keeps the first test from
passing on nothing. There is at least one subject. Every subject in the
source is a plain quoted string. Every raise the pack makes has a subject
the test read.

Each run below is from the repository root with
`make test PYTEST_DIRS=follow-through/tests PYTEST_ARGS='-q -rA --tb=short'`.

- `red.txt`: the test as first written (commit 2fb7e8e5), with the skill as
  round 1 left it: 1 failed (the reminder case), 14 passed (the follow-up
  case among them), exit 2.
- `green.txt`: at `5729f9062600d09b446a1f0297a96f1bef8bf69e`: 15 passed, exit 0.

`mutations.txt` holds the skill test run in a copy of the pack at the head,
broken one way at a time:

| What was broken | Result |
| --- | --- |
| nothing | 10 passed |
| the skill as round 1 left it (red at the round's start, with the final test) | the reminder case fails; the follow-up case passes |
| the round's new sentences removed | the reminder case fails |
| a third raise with a subject the skill never names | that subject's case fails |
| a third raise whose subject is a variable passed by shorthand | that subject's case fails |
| a third raise whose fields come from elsewhere | `test_every_mail_subject_in_the_pack_is_read` fails |
| a subject built at run time | `test_every_mail_subject_in_the_pack_is_read` fails |
| a third raise described in the Reminders section with no rule of its own | its case and the reminder case fail |
| control: a third raise with its own section and rule | 11 passed |

`mutations-before-fold.txt` runs two of those cases against the test as it
stood at 68a34b14, before the self-review fold: both stay green there, which
is what the review found.

## The whole suite

`full-suite.txt`: `make test` from a clean clone at
`5729f9062600d09b446a1f0297a96f1bef8bf69e`, run as
`env -i PATH="$PATH" HOME="$HOME" TERM=dumb make test` so that nothing from
the lane's environment reaches it (the clone path is written `<clone>`):
2778 passed, 131 skipped, 10689 subtests passed, 0 failed, exit 0.

Round 1's run from a clean clone had 2772 passed and 3 failed (the three
tests/test_gastown_lint_findings.py tests that fail outside a city). This
round adds 3 tests, and those three pass here. This run differs from round
1's in that it strips the lane's environment. No other run was made to tell
whether that is the cause, and the round changes nothing they read.

## Review

The self-review (a fresh reader on the lane's own model) read the round at
68a34b14 against the bead, the round-1 spec and the public-repository rule:
0 BLOCKER, 2 MAJOR, 3 MINOR, 3 NIT. Every one is fixed in 5729f906.

- MAJOR: two reminders with the same words could collapse into one. The
  mail is marked late when the pack sends it, not when the mayor reads it
  (`follow-through/src/engine.js`, reminderBody), so a reminder given an
  hour late without its time stood in the conversation after a second
  reminder with the same words fell due, and the second was taken for a
  repeat. Shown by that sequence against the words at 68a34b14. FIXED: every
  giving names the time its reminder was for, and a giving counts only if
  it names this reminder's time.
- MAJOR: a reminder given on another channel while a WhatsApp window was
  closed was not looked for, so its repeat was given again. Shown by the
  WhatsApp section's own words. FIXED: the mayor looks on that channel too.
- MINOR: "Otherwise tell the owner now" no longer followed the cancel
  sentence, so read literally it could undo it. FIXED: the new rule ends
  "as for a cancelled reminder".
- MINOR: the test missed a subject given by shorthand from a variable.
  Shown in `mutations-before-fold.txt` (the test at 68a34b14 stays green).
  FIXED: it reads a subject given by "=" as well, and every raise must have
  a subject it read. Reading every string that starts with the subject's
  prefix was not taken: the pack's health line also starts with it and is
  not a mail.
- MINOR: a section naming two kinds of mail passed on one rule. Shown in
  `mutations-before-fold.txt`. FIXED: a section needs a rule for each kind
  it names.
- NIT: "If it shows" read as "a session". FIXED in the rewrite.
- NIT: a short ragged source line. FIXED: the paragraph is rewrapped.
- NIT: six sentences, one carrying two meanings, one repeating the mail.
  FIXED: the rule is five sentences now and those two are gone.

The codex round (class STANDARD, model gpt-6-astra, through the hub) on
b04ae938..5729f906: CLEAN, no finding at any severity.
