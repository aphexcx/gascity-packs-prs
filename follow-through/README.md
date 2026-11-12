# Follow-through

Keep a durable list of things an owner started and left, and reminders the
owner requested. This pack sends factual mail to the city's own agent. It
never writes to an owner. No listener, socket, API, service, model call or
package installation is involved: it runs on Node.js 20 or newer.

The mayor reads the follow-through skill before acting. That skill is shipped
separately (spec 4b); do not install this mechanism on an owner's box before
that skill is available. WhatsApp's window and update-template restrictions
remain the mayor's responsibility. This pack does not read the window-keeper.

## State and privacy

Everything durable is in `$GC_PACK_STATE_DIR/state.json`. With gc's canonical
layout this is `<city>/.gc/runtime/packs/follow-through/state.json`. Both pack
commands and exec orders receive the same `GC_PACK_STATE_DIR`; the pack
refuses to guess a path if it is absent. Verified at gc `a8217556`:
`cmd/gc/cmd_commands.go` uses `citylayout.PackRuntimeEnv`, and
`cmd/gc/order_store.go` uses `citylayout.PackStateDir`. Doctor uses that same
runtime environment. The import alias does not select a different state file.

The file contains conversation IDs, consent, hours, zones, items and their
summaries, reminders and their text, wake marks, mail bodies and delivery
attempts, and the last tick time. Closed items and cancelled reminders remain
in the file. There is no automatic deletion or pruning. State is private to
the box: nothing leaves the box through this pack. Only local gc mail receives
the due text. What the mayor subsequently sends is governed by its skills.

The directory is mode 0700 and the state file is mode 0600. Writes use a
private temporary file, fsync, rename, and directory fsync. A local-filesystem
bakery lock serializes ticks and mutations, including mail attempts. Transient
lock tickets contain a PID, process birth identity and sequence number. Dead
or recycled processes' tickets are reclaimed by their unique names. Linux
uses kernel start ticks and boot identity from `/proc`; macOS uses its shipped
`/bin/ps` start time. A matching live holder is never declared stale by age.
Do not share this directory over a network filesystem.
A crash may leave private temporary files, but the atomic state remains valid.

Restarting the box, city, mayor or tick process does not reset anything.
Status and doctor omit item summaries, reminder text and mail bodies. `list`
and `remind --list` intentionally show the owner's stored text.

## Commands

Use the pack's actual import alias in place of `follow-through` when different.
Conversation names are `provider/conversation-id`, for example `slack/C123`.

```sh
gc follow-through consent slack/C123 --on
gc follow-through consent slack/C123 --off
gc follow-through hours slack/C123 --hours "Mon-Fri 09:00-18:00" --tz America/Chicago
gc follow-through hours slack/C123 --hours "Mon-Fri 09:00-12:00, 13:00-18:00" --tz America/Chicago
gc follow-through hours slack/C123 --clear
gc follow-through hours slack/C123 --clear --tz Asia/Tokyo
gc follow-through add --conversation slack/C123 --summary "Choose a date for the invented workshop"
gc follow-through add --conversation slack/C123 --summary-file /path/to/summary.txt
gc follow-through list --conversation slack/C123 --all --json
gc follow-through done ITEM_ID
gc follow-through drop ITEM_ID
gc follow-through remind --conversation slack/C123 --in 2h --text "Review the invented agenda"
gc follow-through remind --conversation slack/C123 --at 2027-01-15T09:00:00-06:00 --text-file /path/to/reminder.txt
gc follow-through remind --list --json
gc follow-through remind --cancel REMINDER_ID
gc follow-through status --json
```

Consent starts unrecorded and counts as off. Hours support day ranges, split
spans, and spans crossing midnight, using the window-keeper's grammar. The
runtime validates IANA zone names. Clearing hours retains the zone unless
`--tz` sets another one; `--tz` alone changes only the zone. A missing zone
prevents follow-ups and is named by status and, when consent is on, doctor.

`add` prints an ID. Summaries have one line and at most 200 Unicode characters.
`list` shows open and raised items; `--all` adds done and dropped ones.
`list --wake WAKE_ID --json` returns exactly the items raised in that wake,
including items subsequently done or dropped. Conversation references are
limited to 512 UTF-8 bytes at every mutation verb.
`remind` prints the ID and due time in the recorded zone, or UTC without one.
Reminder text has at most 500 Unicode characters; file inputs accept a trailing
newline. `--in` accepts positive minutes, hours or days (`30m`, `2 hours`, `1d`).
`--at` requires RFC 3339 with an explicit offset. Past times are refused.

## When mail is raised

Each item becomes due 12 hours after opening. At the first eligible wake at or
after that point, all due items for one conversation enter one follow-up mail.
An item is raised only once in its life. A wake with nothing due sends nothing.
The mail prints a durable wake ID and at most 25 due items in full, oldest
first. Overflow says `N more are due. See them all: gc follow-through list
--wake WAKE_ID`. Every due item, including overflow, stores the wake ID and
becomes raised. Mail bodies are capped at 64 KiB: fewer lines are shown if
needed, and a single oversized line is shortened without losing its ID.
Reminder bodies obey the same byte bound. The item cap (`MAX_WAKE_ITEMS`),
body cap (`MAX_MAIL_BODY_BYTES`) and conversation cap
(`MAX_CONVERSATION_BYTES`) are proposal constants from dispatch ruling 2.

With hours, wakes are the start of the day's first span and 60 minutes before
the end of its last span. Spans belong to the local day on which they open,
including a close after midnight. Without hours, wakes are 09:00 and 17:00
Monday to Friday. No zone is guessed. DST folds use the earlier occurrence;
nonexistent wall times move forward by the gap, as in the window-keeper.

The dispatch ruling for gp-91f7 defines **on time** as less than 120 seconds
after the scheduled instant. Such a wake skips the late-wake hours check.
From 120 seconds onward, a missed wake is eligible only within two hours of
its scheduled instant and inside the owner's hours; without recorded hours,
it must be before 17:00 local. Thus a default 17:00 wake fires on time or waits
until morning. **No follow-up wake is ever raised at or after 20:00 local**,
on time or late. Turning consent off prevents further follow-up sends,
including pending retries. Open items remain open while consent is off.

A reminder fires at the first tick at or after its chosen time, at any hour,
without requiring consent or a zone. One missed during an outage fires once
after recovery. Its fixed body includes how many minutes late it is when
raised, if more than five minutes late. Cancelling a set reminder prevents
its raise; an already-raised reminder cannot be recalled.

The cooldown order runs once a minute, while gc's orchestrator checks orders
about every 30 seconds. A reminder or wake is therefore normally raised
within about a minute and a half of its time (long-running ticks add delay).
If the mayor is busy, `--notify` queues delivery until its turn ends; it does
not interrupt the turn. A sleeping mayor gets a managed wake.

The 12 hours, 09:00 and 17:00, Monday to Friday, 60 minutes, two-hour grace,
120 seconds and 20:00 cap are proposal defaults awaiting the founder's ruling
at merge. `ON_TIME_MS` is a single constant; schedule tests can shorten it.

## Mail recovery and health

Every raise, item/reminder transition, and attempt timestamp is fsynced before
mail. The only delivery command is:

```text
${GC_BIN:-gc} mail send ${FOLLOW_THROUGH_AGENT:-mayor} --notify --dedup KEY -s SUBJECT -m BODY
```

`FOLLOW_THROUGH_AGENT` names the city's own agent. The pack does not expose an
owner-recipient argument. Subjects are `follow-through: follow-up due` and
`follow-through: reminder due`. Bodies contain facts and the instruction to
read the follow-through skill; they do not draft a message to the owner.

Failed or interrupted attempts retry on the next eligible tick, no more than
once a minute, with the same durable key. After 24 hours from the raise they
become failed. Ticks prioritize unattempted raises, then the least recently
attempted, so slow failures cannot starve later mail. The 90-second tick
budget (`TICK_BUDGET_MS`) includes lock waiting and reserves a full 15-second
send timeout plus one second to save and release before starting another send.
This leaves room below the order's two-minute deadline. Raises remain durable
for later ticks when the budget is spent. Follow-up retries still obey consent, the original wake's
grace, hours and 20:00 cutoff; reminders remain eligible at any hour. The file
keeps the pending record if a process dies before the send. If it dies after
delivery but before recording success, gc dedup suppresses the repeated key
while the earlier mail is unarchived. This is gc's bounded dedup guarantee,
not a guarantee against a second mail after the earlier mail is archived.
The pack guarantees one logical raise per item/reminder. The mayor's skill
governs whether an owner receives one follow-up or none.

`status` reports owner and item counts, missing zones, set reminders, next
eligible scheduled wakes (whether or not an item is due), last tick, and
pending or failed mail attempts. Doctor is red if an active owner or set
reminder has no tick in five minutes, a consenting owner has no zone, or a
raise has exhausted its retries. Mail subprocesses have a 15-second timeout;
their output is never included in status or doctor.

## Tests

From `follow-through/`, run `npm test` (no install needed), or from
`follow-through/test/`, run `node --test`. From the repository root, `make test`
includes `follow-through/tests`, whose Python tests also run the Node suite
and the shipped command/order/doctor wrappers. Live-gc registration and
doctor coverage is in `tests/test_maintained_packs_live_gc.py`.

Tests inject clocks, run real overlapping/killed processes, and replace gc
with a local fake that records arguments and deduplicates keys. They never
mail a real city or sleep for a scheduled wake. `src/zoned.js` is copied from
`whatsapp/adapter/src/zoned.js` at `28918336555abbeb4f52af3c05029e801c59cb6f`,
with hours grammar, DST-gap and DST-fold tests carried into this pack. There
is no cross-pack import.
