# codex-gate review: base f507f7189184df2318011cfb4a6de8a82c5dc636 (merge base of --base f507f7189184df2318011cfb4a6de8a82c5dc636 = f507f7189184df2318011cfb4a6de8a82c5dc636 and the head) head 5d665d86b110c384ed1c0229232dd6da5e3ed1f0; 35 file(s) (8 under assets/pr-evidence/, outside the --max-files cap; 27 counted); class DEEP; model gpt-6-astra; 2026-10-25T21:09:50Z
# codex account: citadel (CODEX_HOME /Users/tailor512/.codex)
# reviewed in a pristine detached worktree at the head (HEAD of /Users/tailor512/city/.worktrees/gascity-packs/lane-implementation-worker-codex; 60 untracked entr(ies) there ignored)
# verdict rule: CLEAN with no CRITICAL or MAJOR finding (MINORs are reported, deferred by the lane, never blocking)
#  assets/pr-evidence/gp-j5p4/baseline.json                     | 208 +++++++++++++++++++++++
#  assets/pr-evidence/gp-j5p4/measure-first.md                  |  58 +++++++
#  assets/pr-evidence/gp-j5p4/measurement-summary.json          |  27 +++
#  assets/pr-evidence/gp-j5p4/minimal-before.json               |   5 +
#  assets/pr-evidence/gp-j5p4/peer-counts-before.json           |  54 ++++++
#  assets/pr-evidence/gp-j5p4/replay-after.json                 |  47 ++++++
#  assets/pr-evidence/gp-j5p4/replay-prepare-after.json         |  10 ++
#  assets/pr-evidence/gp-j5p4/self-review.md                    | 114 +++++++++++++
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
#  slack-full/adapter/peer_bots.go                              | 448 +++++++++++++++++++++++--------------------------
#  slack-full/adapter/peer_bots_test.go                         | 178 +++++++-------------
#  slack-full/adapter/peer_parity_test.go                       |  87 ++++++++++
#  slack-full/adapter/reminder_atomicity_test.go                |  17 +-
#  slack-full/adapter/reminder_budget.go                        | 173 ++++---------------
#  slack-full/adapter/reminder_budget_test.go                   | 132 +--------------
#  slack-full/adapter/testdata/injection-full/peer.golden       |   4 +-
#  slack-full/adapter/thread_context.go                         | 184 ++++-----------------
#  slack-full/adapter/thread_context_peer_bots_test.go          | 426 +----------------------------------------------
#  slack-full/adapter/thread_context_test.go                    |   8 +-
#  slack-full/docs/injection-progressive-disclosure-20261025.md | 126 +++++++++++---
#  slack-full/scripts/replay_minimal_injections.py              |  45 ++++-
#  slack-full/scripts/slack_chat_read.py                        |  21 ++-
#  slack-full/tests/test_replay_minimal_injections.py           |   8 +-
#  slack-full/tests/test_slack_chat_read.py                     |  36 ++++
#  35 files changed, 1625 insertions(+), 1667 deletions(-)

- **Major — Pending human messages are incorrectly treated as delivered.** At [peer_bots.go:601](/var/folders/n0/yx26c20j4y5b0597zqpc5kzh0000gn/T/codex-gate-tree.x2JXsa/tree/slack-full/adapter/peer_bots.go:601), `pendingContains` suppresses buffered messages from thread context. Unlike the human urgent path, peer delivery never calls `flushAheadOf`: it sends only the peer message. If a human question is buffered when a peer answers, the session receives the answer without the question, labeled “already delivered”; the question arrives later, potentially after the digest interval. Flush pending messages before acquiring the delivery hold, or collapse only messages actually delivered.

- **Minor — Small result limits multiply thread-history requests.** At [slack_chat_read.py:205](/var/folders/n0/yx26c20j4y5b0597zqpc5kzh0000gn/T/codex-gate-tree.x2JXsa/tree/slack-full/scripts/slack_chat_read.py:205), newest-tail scans use the requested result limit as the API page size. A 1,000-message thread with `--limit 1` therefore requires 1,000 requests when pages honor that limit, despite the supported 200-message page size. Fetch using the independent page ceiling and retain only the requested tail, preserving response-cut fallback.

Tests: **not run (sandbox)**. The committed [self-review:104](/var/folders/n0/yx26c20j4y5b0597zqpc5kzh0000gn/T/codex-gate-tree.x2JXsa/tree/assets/pr-evidence/gp-j5p4/self-review.md:104) reports 134 passing tests from worker validation; I did not independently verify that run.

VERDICT: BLOCK
