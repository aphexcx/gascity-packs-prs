# codex-gate review: base f507f7189184df2318011cfb4a6de8a82c5dc636 (merge base of --base f507f7189184df2318011cfb4a6de8a82c5dc636 = f507f7189184df2318011cfb4a6de8a82c5dc636 and the head) head 43e982a75af86f24553c1510fa98817178f14c40; 39 file(s) (12 under assets/pr-evidence/, outside the --max-files cap; 27 counted); class DEEP; model gpt-6-astra; 2026-10-25T21:19:29Z
# codex account: citadel (CODEX_HOME /Users/tailor512/.codex)
# reviewed in a pristine detached worktree at the head (HEAD of /Users/tailor512/city/.worktrees/gascity-packs/lane-implementation-worker-codex; 69 untracked entr(ies) there ignored)
# verdict rule: CLEAN with no CRITICAL or MAJOR finding (MINORs are reported, deferred by the lane, never blocking)
#  assets/pr-evidence/gp-j5p4/baseline.json                     | 208 +++++++++++++++++++++++
#  assets/pr-evidence/gp-j5p4/codex-review-r1.md                |  48 ++++++
#  assets/pr-evidence/gp-j5p4/codex-review-r1.md.rc             |   1 +
#  assets/pr-evidence/gp-j5p4/codex-review-r1.md.reason         |   1 +
#  assets/pr-evidence/gp-j5p4/measure-first.md                  |  58 +++++++
#  assets/pr-evidence/gp-j5p4/measurement-summary.json          |  27 +++
#  assets/pr-evidence/gp-j5p4/minimal-before.json               |   5 +
#  assets/pr-evidence/gp-j5p4/peer-counts-before.json           |  54 ++++++
#  assets/pr-evidence/gp-j5p4/replay-after.json                 |  47 ++++++
#  assets/pr-evidence/gp-j5p4/replay-prepare-after.json         |  10 ++
#  assets/pr-evidence/gp-j5p4/review-response.md                |  18 ++
#  assets/pr-evidence/gp-j5p4/self-review.md                    | 159 ++++++++++++++++++
#  slack-full/CHANGELOG.md                                      |  21 ++-
#  slack-full/README.md                                         |   9 +-
#  slack-full/adapter/SETUP.md                                  |  80 +++++----
#  slack-full/adapter/inbound_coalescer.go                      |  45 ++---
#  slack-full/adapter/inbound_coalescer_test.go                 |   2 +-
#  slack-full/adapter/inbound_rejection_ladder_test.go          |   4 +-
#  slack-full/adapter/injection_full_golden_test.go             |   7 +-
#  slack-full/adapter/injection_mode.go                         |  64 ++-----
#  slack-full/adapter/injection_mode_test.go                    |  21 +--
#  slack-full/adapter/injection_recovery_test.go                | 459 +++++++++++++++++++++++++++++++++------------------
#  slack-full/adapter/injection_replay_test.go                  |  30 +++-
#  slack-full/adapter/main.go                                   | 134 ++++-----------
#  slack-full/adapter/peer_bots.go                              | 454 +++++++++++++++++++++++---------------------------
#  slack-full/adapter/peer_bots_test.go                         | 178 +++++++-------------
#  slack-full/adapter/peer_parity_test.go                       | 154 +++++++++++++++++
#  slack-full/adapter/reminder_atomicity_test.go                |  17 +-
#  slack-full/adapter/reminder_budget.go                        | 173 ++++---------------
#  slack-full/adapter/reminder_budget_test.go                   | 132 +--------------
#  slack-full/adapter/testdata/injection-full/peer.golden       |   4 +-
#  slack-full/adapter/thread_context.go                         | 184 ++++-----------------
#  slack-full/adapter/thread_context_peer_bots_test.go          | 426 +----------------------------------------------
#  slack-full/adapter/thread_context_test.go                    |   8 +-
#  slack-full/docs/injection-progressive-disclosure-20261025.md | 126 +++++++++++---
#  slack-full/scripts/replay_minimal_injections.py              |  45 ++++-
#  slack-full/scripts/slack_chat_read.py                        |  23 ++-
#  slack-full/tests/test_replay_minimal_injections.py           |   8 +-
#  slack-full/tests/test_slack_chat_read.py                     |  42 +++++
#  39 files changed, 1819 insertions(+), 1667 deletions(-)

**Major — Peer arrivals can redirect automatic replies to the wrong conversation.** [peer_bots.go:565](/var/folders/n0/yx26c20j4y5b0597zqpc5kzh0000gn/T/codex-gate-tree.oNihTk/tree/slack-full/adapter/peer_bots.go:565) removes the `peer-bot ` actor prefix, but `slack_intake_common.py:577` still relies on that prefix to exclude peer events from latest-inbound selection.

If a human message arrives in C1, followed by a peer post in C2, `reply-current` now selects C2. Its separate transcript lookup still skips bot entries, so it can also inherit an unrelated older human thread in C2. This breaks the documented policy that automatic selection continues to prefer human turns.

Update event filtering alongside the actor-format change, preserving reliable bot provenance. Add a regression with a human inbound in one channel followed by a plain-name peer inbound in another; existing exclusion tests still use the retired prefix.

Tests: **not run (sandbox)**. The committed [worker review:104](/var/folders/n0/yx26c20j4y5b0597zqpc5kzh0000gn/T/codex-gate-tree.oNihTk/tree/assets/pr-evidence/gp-j5p4/self-review.md:104) reports 134 passing tests from earlier validation; subsequent follow-up reviews explicitly did not rerun tests.

VERDICT: BLOCK
