# Self-review, round 1

A fresh reviewer on the lane's model (Claude Opus 5.5) read the whole diff of `951afdf341989622aa03a4549c27a7a16b050a2c` against main `926f17912914d9470b4ef477cc65e9fb70792b56`, with the contract, the ticket and its rulings, and the spec as detail. It changed no file; its probes and mutations ran on a copy of the module. Targeted tests (`go test -count=1 -run ChatOwner` on owners, inbound, server, ledger and cmd) passed, and passed under `-race`.

Counts: BLOCKER 0, MAJOR 0, MINOR 3, NIT 11. STOPS: none hit.

## The three answers

- Q1, an owner by anything but C5's two rules: no. `Book.Owner`, `Any` and `List` read one `view()`. Every place that asks goes through it: the message frame (and again after the knock), the reaction frame, the live and stored history frames, the join's "no owner is configured", the join's and the removal's check of a yes (`checkOwnerYes` -> `JoinOwner` -> `Speaker` -> `Book.Owner`), and the owners command. A deletion turn reuses its original's stored frame, as at base.
- Q2, the join's write or the door's remove without a good yes: no. Each runs only after `checkOwnerYes` and the save that marks the request used; the remove is sent once.
- Q3, a setting, variable or flag that moves the path or trusts an untrusted copy: no. `Dir` is a constant, `New` uses it with the uid 0 test, the package reads no environment variable, `ForTest` panics outside a test binary, `fakedoor` is imported by test files only, `claimBook` is replaced only by its test, `owner-claim-check` refuses every argument, and a build without file owners trusts nothing.

## Findings and outcomes

1. MINOR. A sender can write a line that reads like a fixed line (`provenance.Render` quotes only from-line lookalikes), in a direct chat or a room. It makes no owner and the from line stays right. **Outcome: fixed in the skill** (`f4e0368f`): a fixed line is the adapter's only as the last line of a turn whose message is the code alone, and who is an owner is the from line's word. Quoting such lines inside other messages would touch messages C4 leaves untouched: **left to the mayor** (named in the READY).
2. MINOR. Code-shaped strips spaces and hyphens only, so the code with a newline or a tab is untouched; the spec's ruling X3 says all white space. **Outcome: kept as built.** The contract rules over the spec (the ticket's comment of 10:47) and its C4 reads "with spaces and hyphens removed"; the Codex check of the spec graded X3 against C4 as its finding 1. Named in the READY.
3. MINOR. Nothing tested C12's one "gchat owners" line for each call of the owners command, or that the request's code stays out of the log. **Outcome: fixed** (`f4e0368f`): `TestChatOwnerOwnersLogLines`, which fails when the log call is taken out (A2 run 4).
4. NIT. The removal sent its remove without the join's second identity check. **Outcome: fixed** (`f4e0368f`).
5. NIT. The read again after `taken` was untested (a renamed copy is read again anyway). **Outcome: fixed** (`f4e0368f`): `TestChatOwnerTakenReadsTheCopyAgain` rewrites the copy in place with its size and time kept; it fails without the read (A2 run 4).
6. NIT. A catch-up knock is not bounded by the message's age; after a long stop each older code-shaped direct message can cost a live claim a try. **Outcome: named in the READY** (the same kind of cost ruling 17 accepts).
7. NIT. In a merged catch-up turn the fixed line ends its own message, not the whole turn. **Outcome: named in the READY.**
8. NIT. Older tests that give `principal.New` only `Owners`, or leave the server's `Owners` nil, read the fixed directory, absent on any test host. **Outcome: named in the READY**; no test of this change relies on it.
9. NIT. The `--add` answer said "owner page" in a city that is not a box. **Outcome: fixed** (`f4e0368f`): still `add_by_page`, with a message that says the settings name the owners there; the help and the skill say "On a box".
10. NIT. `owner-claim-check` printed a line on stderr on a failure; C3 says it writes nothing. **Outcome: fixed** (`f4e0368f`): nothing on any stream; the test asserts an empty stderr.
11. NIT. The owners package imports `testing` for the guard of `ForTest`. **Outcome: kept**; the guard is what makes the stand-ins test-only.
12. NIT. The removal reused the join's "cannot approve its own join". **Outcome: fixed** (`f4e0368f`): the removal says "removal"; the join's text is byte-identical.
13. NIT. `HistorySpeaker` builds its own book on the fixed directory and has no root-present test. **Outcome: named in the READY.**
14. NIT. The backup's own keys in the restore test are dropped by the decoder, so they could not fail the test. **Outcome: fixed** (`f4e0368f`): the test now reads the saved state and asserts none of those keys is there.

Verdict as given: "not ready yet; fix MINORs 1-3 in the same head (asking the mayor on 1 and 2), then go to the Codex round." Done at `f4e0368f`; points 1 and 2 are in the READY for the mayor.
