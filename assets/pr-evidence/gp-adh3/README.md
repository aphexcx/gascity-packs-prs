# gp-adh3: five reactions with fixed meanings in the three channel texts

The texting rules, the words. On 11/12, a founder said "we can move
forward with reactions" and "i dont want to limit reply length". The Google
Chat skill, the Slack prompt fragment and the WhatsApp skill now carry the
same table of five reactions and the same four rules. The rules: a reaction
comes first, a reaction alone answers a confirmation, ✅ and ⏰ are never a
promise, and the five keep their meanings. No rule about reply length is
added, and none is removed.

- Base: aphexcx/gascity-packs-prs main `28918336555abbeb4f52af3c05029e801c59cb6f`
- Code head: `7a7faffd366950000fe7ea2286b94bd4dca71757`. The commits after it
  touch only this directory.
- Commits:
  - `df86b614` the tests (red)
  - `03e46996` the texts and changelogs
  - `6ca512e1` the self-review fold
  - `7a7faffd` the codex round 1 fold

## For the founders to see

The five and their meanings are a proposal for the founders to rule on. The
same table goes into specs 1a and 1c, so a ruling changes all three.

| Reaction | Slack name | What the owner reads it as |
| --- | --- | --- |
| 👀 | `eyes` | "I have your message and I am on it." |
| 🔍 | `mag` | "I am looking this up." |
| 👍 | `+1` | "Understood. Nothing more needs saying." |
| ✅ | `white_check_mark` | "Done, and checked." |
| ⏰ | `alarm_clock` | "The reminder is set." |

The four rules, word for word the same in all three texts:

- **A reaction comes first.** On a channel where you can react, your first
  answer to a message addressed to you is 👀, 🔍 or 👍, placed before the
  work starts, and never a sentence that only says the message arrived. The
  adapter may already have placed one of these three when the message came
  in. A second one from you does no harm.
- **A reaction alone.** On a channel where you can react, a message to you
  that only confirms, thanks, agrees or closes a topic gets 👍 as the whole
  answer. No text follows it.
- **✅ and ⏰ are yours, and never a promise.** ✅ goes on a request only
  after the work is done and checked. ⏰ goes on a request for a reminder
  only after the reminder is really stored.
- **Fixed means fixed.** Use the five only for their meanings. Other emoji
  are still fine for warmth or for a topic, but never in place of one of the
  five.

Each channel also has its own lines:

- **WhatsApp:**
  - "You cannot place a reaction on WhatsApp yet. The adapter may place one
    of the first three when a message arrives."
  - Its "Answer promptly, even if it is only "on it"" lines stay. On WhatsApp
    a short line of text is still the mayor's only acknowledgement.
- **Slack:**
  - "On an `ambient` or `thread_ambient` turn, the rules under When to respond
    still decide whether you answer at all, with a reaction or in words."
  - The two forms of `gc slack react`.
  - "The command prints a receipt. When it shows `"delivered": false`, the
    reaction was not placed, so say the same thing in words."
- **Google Chat:**
  - "When `gc gchat react` fails, the reaction was not placed, so say the
    same thing in words."
  - The react example now uses 👀.

Questions this PR leaves for a founder (the dispatch note):

1. **Short-reply lines.** Two older lines still ask for short replies:
   - "Keep replies short and plain; Chat is conversation, not a report." in
     the Google Chat skill
   - the WhatsApp section "Write for a phone"

   This PR leaves both exactly as they are. Whether they go is the
   founder's call.
2. **Other emoji.** Other emoji stay allowed beside the five, because one
   city's hand-patched adapter picks a reaction by topic. A founder may
   prefer the five and nothing else.

Every sentence added, removed or changed, old above new, is in
`sentences-old-new.txt`.

## The contract, item by item

1. **DONE.** The table is one table, the same in all three texts.
   - `test_the_five_rows_are_the_same_in_every_text` checks it in every
     character.
   - `test_each_text_carries_the_five_in_order` checks the order.
2. **DONE.** A reaction first. The rule says that the adapter may already
   have placed one of the three, and that a second one does no harm.
3. **DONE.** A reaction alone: 👍 is the whole answer, and no text follows
   it.
4. **DONE.** ✅ and ⏰ go on only after the work is done and checked, or after
   the reminder is really stored. Neither is ever a promise.
5. **DONE.** The five are fixed. Other emoji are allowed, but never in place
   of one of the five.
6. **DONE.** No length rule is added and none is removed.
   `test_the_older_short_reply_lines_stay` pins:
   - the Google Chat line
   - WhatsApp's "on it" lines
   - WhatsApp's "Write for a phone" opening
7. **DONE.** Google Chat:
   - The bullet that allowed "whatever emoji fits" is replaced. The bullet's
     first sentence ("Acknowledge with a reaction, not a message.") and its
     last ("Never send a message only to acknowledge, agree, or repeat.")
     stay as the block's lead.
   - The react example uses 👀.
   - Tested by `test_google_chat_skill_has_the_table_and_no_free_choice`.
   - The edit stays inside the reaction bullet and the react example, per the
     dispatch comment (gp-l49l edits the same file).
8. **DONE.** Slack:
   - A Reactions section after "When to respond", with both forms of the
     react verb as its help page gives them.
   - The sentences pinned before stay untouched, and so does the define
     block.
   - `test_reaction_vocabulary_contract` is new in
     `slack-full/tests/test_slack_prompt_contract.py`.
9. **DONE.** WhatsApp:
   - The table and the two sentences.
   - Lines 37 to 39 are unchanged.
   - Rules 2 and 3 apply only "on a channel where you can react". Rule 4
     says what goes on a request, which the owner sees, and needs no react
     verb.
10. **DONE.** `tests/test_texting_rules.py` is new. It compares the rows and
    the whole list of rules between the three texts.
11. **DONE.** The copy rules:
    - No added line has an em dash (checked over the diff).
    - The mayor is "you".
    - The WhatsApp skill never says "the user" (tested).
12. **DONE.** Each of the three changelogs gains one line under its top
    section. No pack.toml is touched.

Two calls the lane made:

- **"The adapter", not "the box".** Items 2 and 9 say "the box". The texts
  say "the adapter", the word the Google Chat and WhatsApp skills already use
  for the part that places it.
- **Rules word for word.** The rules are word for word the same in the three
  texts, not only the table, and a test holds them so.

## Tests, red first

| Step | Commit | Result | File |
| --- | --- | --- | --- |
| The new tests over the base texts | `df86b614` | 11 failed, 2 passed (rc 2) | `red-tests.txt` |
| Green at the texts commit | `03e46996` | 13 passed | |
| The self-review's stronger tests over `03e46996`'s texts | | 3 failed, 10 passed (rc 2) | `fold-red-tests.txt` |
| The codex fold's class test over `6ca512e1`'s texts | | 2 failed, 13 passed (rc 2) | `codex-fold-red-tests.txt` |
| Green at the code head | `7a7faffd` | 15 passed (rc 0) | `green-tests-7a7faffd.txt` |

The two passes in the first red run are the guard on the older lines and the
existing Slack test.

The invocation, from a clean clone at each commit:

```
env -i PATH="$PATH" HOME="$HOME" TMPDIR="$TMPDIR" make test PYTEST_DIRS="tests/test_texting_rules.py slack-full/tests/test_slack_prompt_contract.py" PYTEST_ARGS="-q -rA"
```

The mutation probes (`mutation-probes-7a7faffd.txt`) change one text at a
time. All four are caught:

- an indented paragraph inside a rule
- a fifth bullet among the rules
- the intro sentence
- one character of one row

The Slack fragment parses and renders as a Go template, with the define block
intact: `slack-fragment-go-template-render-7a7faffd.txt`, from the program in
`slack-fragment-go-template-render.go.txt`.

## Suites

From a clean clone at `7a7faffd`, under `env -i` (no lane variables):

- `make test`: 2705 passed, 126 skipped, 2 warnings (a fork DeprecationWarning in test_slack_company_outbound.py, outside this delta), 10689 subtests passed, rc 0 (`suite-make-test-7a7faffd.txt`)
- `make test PYTEST_DIRS=whatsapp/tests`: 57 passed, rc 0 (`suite-make-test-whatsapp-7a7faffd.txt`)

## Review

### Self-review

A fresh reviewer on the lane's model read the whole diff at `03e46996`. It
found 0 BLOCKER, 0 MAJOR, 5 MINOR and 5 NIT.

- **MINOR 1: FIXED in `6ca512e1`.** The Slack text said a company-room
  reminder gives `channel_id` and `origin_ts`, but a mention-only room's
  minimal reminder carries no `channel_id` (`company_mention_only.go`). The
  text now says "by its channel id and its timestamp".
- **MINOR 2: FIXED in part in `6ca512e1`.** The guard now reads "On a channel
  where you can react". On Google Chat and Slack it names the channel; on
  WhatsApp it plainly does not apply. Rule 4 stays unguarded, which is
  INTENDED: it says what goes on a request, so it holds on every channel.
- **MINOR 3: FIXED in `6ca512e1`.** "Never send a message only to
  acknowledge, agree, or repeat." is kept in Google Chat. Without it, nothing
  else forbade a message that only repeats.
- **MINOR 4: FIXED in `6ca512e1`.** Slack says that on `ambient` and
  `thread_ambient` turns, the rules under When to respond still decide
  whether to answer at all.
- **MINOR 5: FIXED in `6ca512e1`.** The rules test read a bullet only up to
  its first blank line. It now compares the whole list, and the mutation
  probes show the change is caught.
- **NIT 6: FIXED in `6ca512e1`.** The intro sentence is checked in each text.
  A bullet slipped in among the rules is caught.
- **NIT 7: FIXED in `6ca512e1`.** A blank line ends the rules before "Reply
  once per message".
- **NIT 8: INTENDED.** WhatsApp says twice, six lines apart, that the adapter
  may place a reaction. Items 2 and 9 each require it.
- **NIT 9: INTENDED.** The text promises no more than that a repeat reaction
  on Google Chat "does no harm", which is what the bead asks. A failed
  `gc gchat react` is now said in words (`7a7faffd`). Whether Google
  refuses a same-user repeat is for spec 1a to check before its
  acknowledgement goes on.
- **NIT 10: INTENDED.** The WhatsApp changelog line sits under the top
  section, "Unreleased (gp-g02t, PR 76 round 2)", as item 12 asks.

### Codex round 1

STANDARD, at `6ca512e1`. The answer is in `codex-round1-6ca512e1.md`; home
paths are shortened there. It returned BLOCK with one MAJOR.

**The finding:** `gc slack react` always reacts with the shared switchboard
token (`slack-full/adapter/main.go` handleReact). In a company DM, which only
the owner agent's token can see (`company_acks.go`: "The switchboard token
must never touch a DM"), Slack refuses the reaction. The command still prints
`"delivered": false` and exits 0 (`slack_chat_react.py`). A mayor following
"No text follows it" would leave the person with nothing.

**FIXED in `7a7faffd` at the class level.** The class is a reaction that
does not land, which is silent to the owner while the rules make it the
answer. The cases:

- **Slack:** every refusal comes back as `"delivered": false` with exit 0.
  This covers the company-DM token, `not_found` and transient errors.
- **Google Chat:** a refusal makes `gc gchat react` exit non-zero with the
  HTTP error (`gchat/commands/common.sh`).

Both texts now say that a reaction that was not placed is said in words. The
test is `test_a_reaction_that_is_not_placed_is_said_in_words` (Google Chat
and Slack).

**Not fixed here.** Codex's other remedy, resolving the company identity
before reacting, changes the adapter or the command. That is outside this
bead. It also needs a precondition that no city on this host meets: the one
city registered here has no `dm_bindings.json` or `agent_apps.json`, so it
runs no per-agent company DMs. The same limit means that
in a company room a reaction shows as the switchboard, not the agent. It
goes to the mayor as a follow-up.

Codex also noted "Tests: not run (sandbox)" and no evidence at the head. Both
were true at `6ca512e1`: the evidence is committed after the code.

The bead allows one codex round. The mayor's gate and a read follow the
READY, and the class test above is the one for that round to check.
