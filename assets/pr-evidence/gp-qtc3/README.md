# Reachable verification

The dispatch base was `7876f9768281dc85036160c9dddd71173401b5e3`. The accepted six-run timing series used `567f0c781599163726ff119dcba9d683bcddcb67`. The verified implementation head is `b44e04a9200662f0ea8bef1e948d5cd1ba52005f`. All hook, command and settings examples used temporary cities.

| Item | Result | Coverage |
| --- | --- | --- |
| 1. Addressed messages | NOT BUILT | The inbound event carries provider, conversation ID, actor, target session and target agent. None distinguishes a mention, direct message or thread. |
| 2. Replies during a wait | DONE | The R2 cases in `reply-grammar.test.cjs` check literal replies, temporary files, the reason given for each refused form and refusal counts in both modes. |
| 3. Unusable event rows | DONE | The R3 cases in `skipped-rows.test.cjs` cover routing, release of earlier waits, gap accounting and rotation across an already-consumed malformed line. |
| 4. Bounded hook writes | DONE | The R4 cases in `bounded-writes.test.cjs` cover duplicate processes, persistent errors, stale and held locks, 40,000 hook events and migration from version 1. |
| 5. Timing | DONE | The R5 cases in `cpu.test.cjs` and `bounded-writes.test.cjs` measure complete-process CPU and warm decisions at the collection limits. The existing 256 MiB wall-clock test remains opt-in. A separate CI job runs the default suite. |
| 6. Import alias | DONE | `alias.test.cjs` installs each alias, checks the complete conversation line and exercises its pass command. |
| 7. Uninstall | DONE | `settings-restore.test.cjs` covers three byte layouts, permissions, absent files and directories, and preserving later edits. |
| 8. Expiration | DONE | The R8 cases in `bounded-writes.test.cjs` exercise conversation expiry, persistent manual exemptions and the newest three settings backups. |
| 9. Status and doctor | DONE | The R3 and R4 tests assert skip counts, failing-open codes, recovery and the last successful hook time. `status.txt` includes the actual command output, including a corrupt-state read. |
| 10. Documentation | DONE | The README test checks the new reply, retention, diagnostic, restore and timing terms. |

Three existing assertions changed to match the contract. The exact-refusal test in `decision.test.cjs` checks the appended reason and preserves its original conversation-line assertion. The malformed-row test in `events.test.cjs` checks the skip count while retaining the unexplained-gap rejection. The README test in `pack.test.cjs` checks the new behavior is documented.

## Timing and archive replay

All six CPU checks passed. All six injected regressions exceeded the 100 ms budget. Loaded runs five and six exited 2 in `review 4: archive replay advances in bounded chunks without dropping rows` when its archive child returned `ETIMEDOUT`. The one-minute starting loads were 24.96 and 62.04. The repository Verify skill at the dispatch base records this case timing out under the two-second child deadline. It also timed out with the unchanged dispatch hook during the red-first suite, then passed in isolation at a starting load of 45.84. The archive child transport, two-second deadline and existing archive test are unchanged. See `load.txt` for all six rows.

## Self-review

The implementation immediately after self-review completed six CPU runs, all starting above load 20. Four complete suites passed. The two nonzero exits were the archive timeout and an existing outer-deadline test observing a child with a null exit status. Four isolated runs each of the outer-deadline case against the dispatch and review-fixed modules returned exit zero.

The read-only Codex review of `567f0c781599163726ff119dcba9d683bcddcb67` reported 1 P1, 11 P2 and 2 P3 findings. All were fixed in `f00ccd433641cb13ca9b345378d027f165e8fcdf`. The regression fixtures in `retention-review.test.cjs` exercise the following cases.

| Priority | Finding and verified fix |
| --- | --- |
| P1 | Sample the clock after the event batch is captured so a valid row arriving during acquisition remains measurable. |
| P2 | Count a complete blank line and consume the sequence gap it can explain. |
| P2 | Treat an unusable timestamp value as a damaged row without attempting object coercion. |
| P2 | Check the byte boundary and sequence before reusing a cursor in a replacement active log. |
| P2 | Use available routing information before a damaged outbound can release a wait. |
| P2 | Validate a quoted expansion against the logical word after folding line continuations. |
| P2 | Use the reply parser's quoting rules to recognize an alias before a refused suffix. |
| P2 | Permit a pass for an outstanding wait even after the known-conversation map evicts its entry. |
| P2 | Read the mode again after acquiring the state lock. |
| P2 | Preserve the previous successful hook timestamp when writing the log mirror fails. |
| P2 | Report a current backwards-clock failure even when its wall time precedes the last successful save. |
| P2 | Apply the history cap after replay completes deferred long calls. |
| P3 | Preserve the complete row-size budget when reusing a consumed archive prefix. |
| P3 | Recreate the original settings directory when restoring its file requires it. |

## Hosted verification

The first Linux run exposed three rotation failures. A replacement file can reuse an inode and have the same trailing bytes even when its last consumed sequence differs. A deterministic in-place rewrite reproduced the missing rows locally. The final fix checks the sequence before reusing the cursor and retains the fast path for an unchanged empty file.

The Linux Reachable job passed on the fixed implementation. All six default-suite runs on that implementation exited zero at starting loads of 27.58–37.40. Each passed 187 tests and skipped the opt-in benchmark. The separate 256 MiB benchmark also passed. [Hosted Reachable job](https://github.com/aphexcx/gascity-packs-prs/actions/runs/44478391821/job/135404382880)

## Round 2

The starting head was `060bc2aa75add7e91cacf01cbd06dd4bb04aa39f`. The tests in `fa103838` reproduced 21 failures. The two manifest-rename controls passed at that head. Both recovery orders now pass. The implementation including review fixes is `97ad7056d9b41b1b02f0673c1a10f8ca25faff9e`. The individual failing and passing test names are in red.txt and green.txt.

| Item | Verified behavior |
| --- | --- |
| R2-1 | A stale installed copy leaves version-one state byte-identical. Status and exemption listing can be read. Mutations return `hook_copy_outdated_run_install`. |
| R2-2 | Ticket identity and the loaded generation are checked before publication. Recovery withdraws the old staged source or deletion destination, including when the old writer paused inside rename. |
| R2-3 | Cursor and Stop lookbacks include the preceding delimiter of a maximum-size row. Stop also handles an incomplete following row. |
| R2-4 | Inferred gaps release older waits and report the last readable sequence. The archive-tail test failed at the starting head. An in-memory marker distinguishes inferred sequences. Version-two state may now carry `last_readable_seq`. |
| R2-5 | During install, `pending.previous` retains prior owned matchers and commands by event. `pending.first_install` preserves the initial observe decision through retries. The manifest clears pending after settings readback and mode publication. |
| R2-6 | An operator can remove a listed exemption after age or count eviction. |
| R2-7 | Replayed identities keep their newest event time with ties broken in sequence order. |
| M9 and M10 | Missing or changed snapshots use value-preserving uninstall. An unreadable error record is counted alongside other records. |

A skipped outbound with an unreadable conversation continues to release every older wait. This behavior is intended. Addressed-only waits remain deferred because the event row lacks addressing data.

### Native review

The read-only gpt-6.2-astra review of `48bd4a6f62b8fc15e88a6a4e86ecb814915d7ca3` returned one P1, six P2s and one P3. Each has a red assertion at that revision and a green assertion at `97ad7056`.

| Finding | Outcome |
| --- | --- |
| 1 · P1 | Fixed. A displaced mirror writer returns the decision already committed in the receipt. |
| 2 · P2 | Fixed. Uninstall moves owned files into a ticket-specific directory that recovery can withdraw before a successor publishes. |
| 3 · P2 | Fixed. Promoting a zero ticket uses the same checked, revocable publication as other writes. |
| 4 · P2 | Fixed. Stop measures the lookback from the last complete newline when the tail is unfinished. |
| 5 · P2 | Fixed. Recovery replaces a stale ticket's directory entry without opening its inode for writing. |
| 6 · P2 | Fixed. A settings inspection failure preserves the result of comparing the installed hook with the source. |
| 7 · P2 | Fixed. A contender recognizes a revoked ticket from the bytes it reads, including an empty legacy marker. |
| 8 · P3 | Fixed. Inferred positions cannot move the recorded last readable sequence. Existing version-two state resumes from its cursor. |

### Verification of the published change

The hosted Reachable job passed on Node 20 at `089f23b2515319270afd5c71d5fc89e27696ebbd`. The default target also passed at that commit on Node v26.12.1 with 219 passes and one opt-in skip. The PR checks page tracks the published head as evidence is added. [Hosted Reachable run](https://github.com/aphexcx/gascity-packs-prs/actions/runs/44561246929/job/135690813532) · [Checks at the PR head](https://github.com/aphexcx/gascity-packs-prs/pull/118/checks)

`git diff --stat origin/main HEAD` covers 25 files. Seventeen are under reachable/, seven are evidence files, and one is the Reachable CI job added in round 1.
