# Self-review r2 (the keeper)

The window-keeper (keeper.js, zoned.js, keeper.test.js) was out of
self-review r1's scope. Round 2 reviewed it at commit `185a6098`, with its
seams: the join bind in inbound.js, the internal listener's `/consent` and
`/hours` (index.js), the keeper doctor row and the `consent`, `hours` and
`status` verbs. The reviewer reproduced the findings in scratch scripts
outside the repo (`exp.mjs` with the scenarios race, consent-format,
quiet-midnight, save-fail-consent, raise-save-fail and window-404, plus
`exp2.mjs` to `exp4.mjs`).

**Counts: BLOCKER 0, MAJOR 2, MINOR 7, NIT 6.** Both MAJORs, six MINORs and
four NITs are fixed in the head. One MINOR and two NITs are routed as design
questions, unchanged in the code. Each fix that changes behaviour has a
regression test. Against `185a6098`'s source those tests are RED:
`node-keeper-r2-RED.txt`, built by `make_red_keeper_r2.py`, 12 of 13 RED.
The one pass is (10d), whose rewrite is NIT 12: the old assertion could not
fail, and no behaviour changed. As a control, the whole of `keeper.test.js`
and `pipeline.test.js` was also run against `185a6098`'s source: 50 tests,
exactly those 12 fail, the other 38 pass. `pytest-keeper-r2-RED.txt` shows
5 of 6 RED; its pass is the yellow keeper row, a control.

Test names carry the item number of the round's brief (M for MAJOR, m for
MINOR) or, for a NIT, the review's own number (n10, n13, n14).

## MAJOR

1. **A failing `GET /v1/window` silently stopped the ladder and the lapse
   count. While a read was owed it bypassed the throttle (one read per
   250 ms tick, 240 a minute), and the doctor stayed green.** FIXED:
   - A failed read, owed or not, is retried on the ladder (1, 5, 15, 60 s)
     in clock time, using `ladderDelay`.
   - keeper.json records `window_read_ok_at` (the last good read, ISO) and
     `window_read_error` / `window_read_error_at` (the last failure since;
     a good read clears them). A good read that changes nothing else is
     stamped at most once a minute (`WHATSAPP_KEEPER_WINDOW_STAMP_MS`,
     default 60000).
   - The doctor keeper row is red (exit 2) when any conversation has consent
     ON and the stamp is missing or older than 5 minutes. The message names
     the last error and carries the 7-day rows.
   - `gc whatsapp status` prints `Window read: last good <t>; failing since
     <t> (<n> attempt(s)): <error>`. The status JSON's `window_read` is now
     `{ok_at, error, error_at, failures, retry_at, owed}`; it was `{at,
     error}`.

   Tests: (10b), rewritten: 4 reads at +0.25, +1.25, +6.25 and +21.25 s
   where there were 240, with the stamp and the error checked on disk and
   the heal checked. pytest: `test_keeper_row_is_red_when_the_window_clock_is_stale_while_the_keeper_is_on`,
   plus the status line in `test_status_renders_...`. An end-to-end check
   wrote keeper.json with the real keeper (a good read 10 minutes ago, then
   503s) and ran `check-keeper.sh` on it: red, naming the 503, then green
   after a good read.

2. **`consent` and `hours` took any wa_id string and created a new row.
   `consent +5511999990001 --off` left the real `5511999990001` ON and
   answered OK.** FIXED:
   - `setConsent` and `setHours` reduce the input to its digits and match a
     row, or a conversation in the last good window read. An unknown phone
     throws status 404 (the message names its last four digits only) and no
     row is created. Input with no digits is a 400. Both return the matched
     row's id.
   - The listener maps a thrown 400/404 through. It kept the hours/zone →
     400 mapping and 413. `/consent` and `/hours` answer `wa_id_last4`.
   - `consent.sh` and `hours.sh` print the adapter's last four, never the
     input's. An answer without `wa_id_last4` confirms nothing (exit 1).

   Tests: (r2 M2), (keeper r2 M2, n10) through the real child, and pytest
   `test_invite_consent_hours`, `test_consent_and_hours_refuse_...`.
   Consequence: a verb names a phone the keeper has seen, so the keeper
   unit tests onboard through `h.start()` (one window read first). Every
   old expectation is unchanged; the control run above passes them against
   the old source.

## MINOR

3. **A delivery noted while a window read was in flight was lost**: the
   read's completion cleared `readOwed`, so the old close's 60 mark fired
   one second after the owner wrote. FIXED: a generation counter.
   `noteDelivery` raises `owedGen`; a good read satisfies only the
   generation it began under. The ladder also re-checks `readOwed()` per
   conversation, so a delivery noted mid-pass holds it too. Test: (r2 m3)
   (the fake gateway's hook writes and notes the delivery after the read's
   answer is computed).
4. **A failed state write in `raise()` lost the event, and status showed it
   fired.** FIXED: the row is snapshotted before the event's record is made
   and restored when `save()` throws (the file on disk was not replaced),
   so the next tick raises it. The log line is throttled to once a minute.
   Test: (r2 m4): the state dir is 0500 across the 60 mark, then the 60 is
   raised at the first tick that can write, with the true minutes left.
5. **`setConsent`, `setHours` and `bindInvite` mutated memory before
   `save()`**, so a failed write left memory and disk disagreeing (consent
   OFF in memory, ON after a restart). FIXED: `commit()` writes a copy of
   the state with the changed row and invite. Only after that write
   succeeds are they committed, rows in place (`Object.assign`) so a tick
   holding a row keeps the live one. `recordInvite` uses it too. Test:
   (r2 m5): four failing writes, then memory and a restart equal to the
   state before them, and a retry that succeeds.
6. **The invite binding could be lost**: it bound only on the first sight
   of a joined delivery, so a kill between the received line and the bind,
   or a failed write, lost consent for good, and an empty code was silent.
   FIXED:
   - `bindInvite` is idempotent. A bound invite stays as `{..., bound_to,
     bound_at}` until the 30-day prune. A second sight of the same join is
     a no-op `true`, so a later `consent --off` stands. A code bound to one
     phone binds no other. `invites_pending` counts unbound invites.
   - inbound.js calls `bindJoined` at every sight of a joined delivery
     (first and re-read) and at startup for every joined journal entry
     (`e.received.envelope`). A failed bind is logged and retried at the
     next sight. An empty code is logged once per delivery, by ref and last
     four.

   Tests: (r2 m6), and through the real child (keeper r2 m6):
   `WHATSAPP_TEST_KILL_AT=after-received@<rid>`, the restart binds the
   invite (consent ON), and the empty-code line carries `m-…` and `…0002`
   and never the number.
7. **A keeper event answered `pending` was counted undelivered** (a plain
   `!== 'delivered'`). FIXED: `verdictOf` from inbound.js, the inbound
   path's reading of gc's receipt. delivered and pending are accepted;
   failed, partial, no_route (normalized like `key()`) and an unusable
   receipt are undelivered. The (10h) fake's `no_route` stays counted
   unchanged. Test: (r2 m7): pending accepted; partial, failed, no_route
   counted, with their reasons.
8. **The quiet rule compared calendar dates**, so under hours crossing
   midnight (18:00-02:00) the owner writing at 00:30 Wednesday (Tuesday's
   span) silenced Wednesday's 23:28 and 23:58 marks. FIXED: `businessDay()`
   is the date a span opens on, as `dueUpdate` defines it (the local date
   outside every span and without hours). The rule compares the business
   day of the update with the mark's. Test: (r2 m8): all four of
   Wednesday's marks fire, none silent. The (10c) quiet-rule test still
   holds.
9. **ROUTED (design question; the review's finding 8): a close in the first
   ~5 minutes of a span gets no nudge.** Under 09:00-18:00 a close at 09:02
   is case (1), and every mark falls before the open and drops; a close at
   08:57 is case (2) and gets four the evening before. This is the spec
   read literally, so it is left as built. A comment at the spot in
   `planLadder` names it as an open question for the founders.

## NIT

- **NIT 13**: `applyWindow` took a `last_inbound_at` that moved BACKWARD,
  emptied the fired set and re-raised a mark id. FIXED: a read with an older
  (or null) `last_inbound_at` than the recorded one is ignored, logged once.
  Test: (r2 n13).
- **NIT 14**: the ladder ran after the update's post without re-checking
  consent. FIXED: tickOnce re-checks consent on the live row after the
  update. Test: (r2 n14), where `consent --off` lands during the update's
  post.
- **NIT 10**: recorded hours could not be cleared. FIXED: `/hours` takes
  `hours: ""` (tz may then be omitted, keeping the recorded zone; `tz: ""`
  clears it), and `hours.sh` gains `--clear [--tz <zone>]`. Tests:
  (keeper r2 M2, n10), pytest `test_hours_clear`.
- **NIT 12**: (10d)'s `gw.sends === 0` could not fail (the keeper holds no
  send path to the fake). FIXED: the harness records every gateway client
  method the keeper calls, and (10d) asserts it is `window` alone. Every
  keeper event is the plain extmsg shape with no template field, and two
  `noteRefusedNudge` calls show as 2 in status and in keeper.json.
- **ROUTED (design question): the texts' local times are HH:MM only**, as
  the spec words them. A time on another day reads ambiguously, but the
  wording is the spec's to change.
- **ROUTED (design question): no single-process lock on the state dir.**
  Two adapters on one city would each rewrite keeper.json. This is a
  whole-adapter concern (the journal has it too), out of the keeper's scope.

## Notes for the reader

- **The stamp cadence knob.** Each stamp is an fsynced rewrite (about 13 ms
  on this Mac). The child-process suites jump the clock by hours, so a
  stamp at every jump made them about 25% slower; in one full run M2 and
  Minor 9 timed out. The test env therefore sets
  `WHATSAPP_KEEPER_WINDOW_STAMP_MS` to one day, and the keeper unit harness
  sets `windowStampMs` to one day. (10b) runs the production minute.
  Production keeps 60 s.
- **A listener mapping change.** A gateway HTTP 400/404 thrown from
  `POST /invite` (`gateway.createInvite`) now passes through as 400/404,
  where it was 500. The message names the gateway URL, so the CLI shows
  where it came from.
- **Left alone.** `noteRefusedNudge(waId)` still files by the publish
  target as given. The gateway refused that target `window_closed` rather
  than `not_bound`, so it is the bound id.
- **The reviewer's scenarios, rerun against the head.** Each keeper reads
  the window once before a verb, per the r2 rule:
  - race: no nudge for the moved close.
  - consent-format: the real row OFF, no event.
  - quiet-midnight: four marks, none silent.
  - save-fail-consent: memory and disk ON.
  - raise-save-fail: the 60 raised once the write succeeds, and on disk.
  - window-404: one read a minute with the error recorded, so the doctor
    row is red.
