# gp-vnz2 round 4 (bead gp-tmci): the stopped reminder and the follow-up's repeat check

Round 4 of PR 95, on the branch gp-vnz2, from round 3's head
`ab0b2944611da80d2720588bdfa2908ec9dc8f6f`.

- The skill: `follow-through/skills/follow-through/SKILL.md`
- Its test: `follow-through/tests/test_follow_through_skill.py`

The green run, the breaks and the Node suite below were produced at the
code head `dceea4628458192536797544988b99859c2df0fa`. The red run was
produced at `9cd6ad0f`, the commit of the tests alone, with the skill as
round 3 left it. The pack's code does not change in this round. This
directory is the record at the new head.

## The blocking finding, by its class

**The class: a recovery rule that finds its record by attributes that are
not unique, on pages that carry no link.** The mayor's gate and the second
model's read of round 3 both found it.

**The case.** Round 3's Health bullet for a stopped reminder said to find it
in `gc follow-through remind --list` as "a reminder of that conversation
still shown as set after its time". Two reminders can fit that. Status names
a stopped mail by the raise's own id, a random id the pack makes for each
mail (`follow-through/src/engine.js`, `makeRaise`), and never by the
reminder's id (`status` in the same file maps no reminder id). The reminder
list shows a reminder as set until its mail is delivered
(`engine.js`, `delivered`), so a reminder still being tried is also set. A
mayor who picked the wrong reminder left the stopped one ungiven, and the
pack mails nobody about a stopped mail.

This is the only case in the skill. A stopped `follow-up` sends nothing, and
a conversation has at most one hours notice, so neither bullet has a record
to find.

**The fix, in words.** The bullet now says that status does not say which
reminder stopped, and has the mayor apply the Reminders section's rule for a
late reminder to every reminder of that conversation still shown as set
after its time. That rule already sends nothing for a reminder the owner
cancelled there, or one already given there after its time, so taking every
candidate gives no reminder twice.

**The test:**
`test_the_stopped_reminder_rule_takes_every_overdue_reminder_of_its_conversation`.

- It drives the pack's own tick in a temp directory: two reminders in one
  invented conversation, `slack/invented`, A due at 2026-01-15 15:00 UTC and
  B ten minutes later, a sender that always fails, and a tick each minute
  from 15:00 to 15:18. The run never looks in the mail history and never
  starts gc.
- The pack's half reads the pages a mayor has: `commands/status.sh`,
  `status.sh --json` and `commands/remind.sh --list`. It checks that status
  prints the stopped line, that one reminder of the conversation is stopped
  and one is still being tried, that the list shows both reminders as set
  and due before the last tick, and that neither reminder's id appears on
  either status page. If status one day names the reminder by its id, this
  half fails and says the bullet must then use that id. It checks the id
  only: a status that named the reminder some other way, by its due time
  for example, would not fail it.
- The skill's half checks that the Health section holds two phrases word
  for word: "Status does not say which reminder stopped." and "for every
  reminder of that conversation still shown as set after its time". They
  are two constants beside the test's other patterns.

The pages the test reads, from the same run, whole, with the ids replaced
by placeholders:

```
$ status
Owners: 1; consent on: 0; consenting without zone: 0
Owners without zone: slack/invented
Items: 0 open, 0 raised, 0 closed
Reminders set: 2
Next wake slack/invented: none
Last tick: 2026-01-15T15:18:00.000Z
Failing raises: 2
Mail delivery stopped: 1; operator action required
<raise id 1>: reminder slack/invented stopped; attempts 5; invented failure
<raise id 2>: reminder slack/invented pending; attempts 4; invented failure

$ remind --list
<reminder id A> set slack/invented: Invented reminder A (due 2026-01-15T15:00:00.000Z)
<reminder id B> set slack/invented: Invented reminder B (due 2026-01-15T15:10:00.000Z)
```

## The minor finding and its clause

The follow-up section's repeat check said "Look at the conversation before
you write: a follow-up you already sent is there." The WhatsApp section can
send that follow-up to another channel. The same mail read a second time
would then find nothing in the named conversation, and a second follow-up
could go out. The check now also looks at any other channel the WhatsApp
section may have sent the mayor to, as the Reminders section's check already
did.

The clause is not pinned by a test. The read graded it MINOR. A class test
over every "look before you write" sentence would also reach the No daytime
hours section, which this round keeps byte for byte, and that section is not
a case: the mayor never writes first on that mail, and the line it looks for
is said where the owner gave the hours.

## Entry 25 of the founders' rules

- name: no making up for a stopped follow-up
- source: contract item 5 (a follow-up goes out only on the pack's mail, and
  a stopped follow-up's mail never came)
- section: Health
- sentence: "For a stopped `follow-up`, send nothing, and never make up for
  it by writing to the owner yourself."

The sentence was already in the skill and its case is green at both ends of
the round. Before this round no test pinned it: the read reversed it and the
skill test stayed green. B1 and B2 below are its proof.

## For the founders to see

Each passage is quoted from the skill at the head exactly.

The stopped-reminder bullet, in the Health section. It is still the mayor's
proposal, not a founder's word:

> - For a stopped `reminder`, you still give it, because the owner was told it
>   is set. Status does not say which reminder stopped. Run
>   `gc follow-through remind --list`, and for every reminder of that
>   conversation still shown as set after its time, do what the Reminders
>   section says for a reminder mail that says it is late, as if that mail had
>   just come.

The follow-up's repeat check, in The one follow-up:

> Look at the conversation before you write, and at any other channel the
> WhatsApp section below may have sent you to: a follow-up you already sent
> is there.

Entry 25's sentence, unchanged and now pinned:

> - For a stopped `follow-up`, send nothing, and never make up for it by
>   writing to the owner yourself.

Still the founders' question, unchanged from round 3: whether a late or
stopped reminder is given at night or held for the morning.

## The test, red first, then green

- `red.txt`: the tests at `9cd6ad0f` with the skill as round 3 left it, in a
  clean clone. 1 failed, 47 passed, make exit 2. The one failure is the new
  test, on its skill's half: the Health section holds neither phrase. Its
  pack's half passed there, so the premise held. Entry 25 passed.
- `green.txt`: the same suite at the code head. 48 passed, make exit 0.

## The breaks

`breaks.py` makes each break in a fresh copy of the pack and runs the skill
test alone in it (`breaks.txt`). The copy with no break: 43 passed. Each
break was red on exactly the cases expected and no other (5 of 5):

| break | red on |
| --- | --- |
| B1 entry 25's sentence removed from the Health bullet | [no making up for a stopped follow-up], and the Health case [follow-up], because that sentence alone names the kind |
| B2 entry 25 reversed to "For a stopped `follow-up`, make up for it by writing to the owner yourself." | [no making up for a stopped follow-up] |
| B3 "Status does not say which reminder stopped." removed | the stopped-reminder test |
| B4 "for every reminder of that conversation" changed to "for one reminder of that conversation" | the stopped-reminder test |
| B5 status names the reminder a stopped raise carries, in a copy of the pack | the stopped-reminder test, on its pack's half, with the message that the bullet must then use that id |

B5 changes two lines in the copy: `status` in `engine.js` maps the raise's
`reminder_id`, and the stopped line in `cli.js` prints it. The `cli.js` line
alone has nothing to print, because `status` carries no reminder id: with it
alone, the stopped line reads "reminder undefined" and the test stays green
(1 passed). B2 is the read's fifth break, green at round 3's head.

Round 3's 48 breaks of the founders' rules stand: no pinned sentence changed
in this round. Its `breaks.py` checks its reversals against the test's list
one for one, so it no longer runs as it is against this head, which has
entry 25.

## The suites

- `env -u GC_BIN make test PYTEST_DIRS=follow-through/tests PYTEST_ARGS='-q -rA --tb=short'`
  at the code head, in a clean clone: 48 passed, make exit 0 (`green.txt`).
  The skill test has 43 cases, up from 41.
- The pack's Node suite as CI runs it, `cd follow-through/test && env -u GC_BIN node --test`,
  at the code head on Node v26.12.1: 108 tests, 108 passed, 0 failed, exit 0
  (`node.txt`). The pack's code does not change; this is a guard.
- The repository's whole Python suite was not run here, because one of its
  tests starts a real gc binary. CI runs it on the push.

## Review

A fresh self-review on the lane's own model read this round's delta,
`ab0b2944..dceea462`, this directory's draft and the PR body's draft,
against the round's contract, the founders' list, the words rule and the
public-repository rule. On its own it re-ran the green run, the red run, the
Node suite and B5 with the `cli.js` line alone. Its counts: 0 BLOCKER, 0
MAJOR, 0 MINOR, 5 NIT. The code commits did not change. The outcomes:

- NIT: the test's pack half checks only that neither reminder's id is on
  the status pages, while the body's draft said the test checks that status
  does not say which reminder stopped. Shown: a copy whose stopped line also
  printed the reminder's due time passed. FIXED in the body, which now says
  the test checks that status does not name either reminder by its id, and
  in this README, which states the limit. The test is INTENDED as it is:
  the round's contract names the id check, and the pack's likely cure,
  printing the id, is caught (B5).
- NIT: this README said everything was produced at the code head, while the
  red run was at `9cd6ad0f`. FIXED: the head lines say which run is where.
- NIT: the status page quoted here dropped four lines without saying so.
  FIXED: it is quoted whole.
- NIT: the body's draft said the mayor "gives" every reminder that could be
  the stopped one, which a founder could read as sending each again. FIXED:
  it says the mayor applies the late-reminder rule to each, and that the
  rule sends nothing for one cancelled or already given.
- NIT: this README said the body "was rewritten", before the edit. FIXED:
  it says the body is rewritten as the round's last step.

The review could not check round 1's review counts in the body against a
file: round 1's evidence holds none. They come from the body round 1 wrote,
read live before the edit (self-review 0 blocker, 3 major, 5 minor, 3 nit;
codex 0 blocker, 1 major, 2 minor, 1 nit).

## What the PR body changed

The body is rewritten after the push, as the round's last step, and read
back. Each of these is stale in the body round 3 left, and none is in the
new one:

- "Stacked on #93", the base `gp-91f7` at `8e78e97c`, and "Retarget to main
  when #93 merges". PR 93 is on main, and the PR's base is main.
- "The skill follows the pack at #93's head". It now follows the pack on
  main, after PR 100.
- "(7 tests)", "one of the two sentences the founders rule on", "6 failed",
  "12 passed", the 15 breaks of round 1's `mutations.txt` and the whole-suite
  run of round 1.
- A list for the founders of round 1's rules alone, and a Review section of
  round 1 alone.
- Commit ids from before the rebase, which are not on gp-vnz2.
- The trailers "Codex-Reviewed-By: codex round 1 at ..." and
  "Fable-Reviewed-By: pending".
