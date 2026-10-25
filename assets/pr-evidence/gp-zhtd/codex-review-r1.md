# codex-gate review: base 52cbd2f848d5c7b21046ac33179f73f101d4c079 (merge base of --base origin/jp/slack-inject-compact = 52cbd2f848d5c7b21046ac33179f73f101d4c079 and the head) head 80c7277937b55a11e93515e8cc555a1815679a44; 38 file(s) (13 under assets/pr-evidence/, outside the --max-files cap; 25 counted); class DEEP; model gpt-6-astra; 2026-10-25T10:04:19Z
# codex account: citadel (CODEX_HOME /Users/tailor512/.codex)
# reviewed in a pristine detached worktree at the head (HEAD of /Users/tailor512/city/.worktrees/gascity-packs/lane-implementation-worker-codex; 22 untracked entr(ies) there ignored)
# verdict rule: CLEAN with no CRITICAL or MAJOR finding (MINORs are reported, deferred by the lane, never blocking)
#  assets/pr-evidence/gp-zhtd/adapter-race.json                 |  18 +++
#  assets/pr-evidence/gp-zhtd/adapter-tests.json                |  17 ++
#  assets/pr-evidence/gp-zhtd/adapter-vet-final.json            |  11 ++
#  assets/pr-evidence/gp-zhtd/adapter-vet.json                  |  11 ++
#  assets/pr-evidence/gp-zhtd/baseline.json                     | 208 +++++++++++++++++++++++++
#  assets/pr-evidence/gp-zhtd/cli-vet.json                      |  11 ++
#  assets/pr-evidence/gp-zhtd/measurement-summary.json          |  11 ++
#  assets/pr-evidence/gp-zhtd/python-tests.json                 |  23 +++
#  assets/pr-evidence/gp-zhtd/replay-after.json                 |   5 +
#  assets/pr-evidence/gp-zhtd/replay-before.json                |   8 +
#  assets/pr-evidence/gp-zhtd/self-review-r1.md                 |  31 ++++
#  assets/pr-evidence/gp-zhtd/self-review-r2.md                 |   8 +
#  assets/pr-evidence/gp-zhtd/validation.md                     |  35 +++++
#  slack-full/CHANGELOG.md                                      |  10 ++
#  slack-full/README.md                                         |  12 ++
#  slack-full/adapter/SETUP.md                                  |  33 ++++
#  slack-full/adapter/company_mention_only.go                   |   4 +-
#  slack-full/adapter/inbound_coalescer.go                      |  49 +++++-
#  slack-full/adapter/inbound_journal.go                        | 166 ++++++++++++++++++++
#  slack-full/adapter/inbound_spool.go                          |   5 +
#  slack-full/adapter/injection_full_golden_test.go             |  73 +++++++++
#  slack-full/adapter/injection_mode.go                         | 127 +++++++++++++++
#  slack-full/adapter/injection_mode_test.go                    | 429 +++++++++++++++++++++++++++++++++++++++++++++++++++
#  slack-full/adapter/injection_replay_test.go                  |  66 ++++++++
#  slack-full/adapter/main.go                                   | 113 +++++++++++---
#  slack-full/adapter/mention_only.go                           |  35 +++--
#  slack-full/adapter/peer_bots.go                              |  13 ++
#  slack-full/adapter/receipt_follow_up.go                      |   6 +-
#  slack-full/adapter/testdata/injection-full/batch.golden      |  18 +++
#  slack-full/adapter/testdata/injection-full/human.golden      |  18 +++
#  slack-full/adapter/testdata/injection-full/peer.golden       |  19 +++
#  slack-full/adapter/testdata/injection-full/thread.golden     |  19 +++
#  slack-full/adapter/thread_context.go                         | 147 +++++++++++++++---
#  slack-full/docs/injection-progressive-disclosure-20261025.md | 178 +++++++++++++++++++++
#  slack-full/scripts/measure_injected_alerts.py                | 199 ++++++++++++++++++++++++
#  slack-full/scripts/replay_minimal_injections.py              | 106 +++++++++++++
#  slack-full/tests/test_measure_injected_alerts.py             |  84 ++++++++++
#  slack-full/tests/test_replay_minimal_injections.py           |  43 ++++++
#  38 files changed, 2302 insertions(+), 67 deletions(-)

Found three major regressions and one minor issue in `52cbd2f8…80c72779`.

1. **Major — Attachment descriptions can disappear in minimal mention-only deliveries.** [injection_mode.go:122](/var/folders/n0/yx26c20j4y5b0597zqpc5kzh0000gn/T/codex-gate-tree.5qinqf/tree/slack-full/adapter/injection_mode.go:122)
   `runMentionOnlyTargets` appends company-room file descriptions to `msg.Text`, without structured attachments. This new call treats that entire string as trimmable body. With a caption exceeding the default 3,500-byte budget, every attachment description can be removed. Preserve file descriptions in the renderer’s protected `files` argument and cover an oversized caption with attachments.

2. **Major — Reaction lookups now paginate entire threads.** [thread_context.go:560](/var/folders/n0/yx26c20j4y5b0597zqpc5kzh0000gn/T/codex-gate-tree.5qinqf/tree/slack-full/adapter/thread_context.go:560)
   `fetchReactionTargetContext` still calls this helper with `limit=3`, expecting a small lookup of the reacted-to message. The new loop instead traverses the entire thread in three-message pages, including in default full mode. This amplifies Slack requests; any later-page failure or the four-second deadline discards a target already obtained on page one. Give reaction lookup a separate fetch that stops once it finds the target.

3. **Major — Cached anchors continue quoting edited or deleted parents.** [main.go:4334](/var/folders/n0/yx26c20j4y5b0597zqpc5kzh0000gn/T/codex-gate-tree.5qinqf/tree/slack-full/adapter/main.go:4334)
   Minimal mode reuses the cached parent excerpt for up to seven days. However, `message_changed` is dropped and `message_deleted` only updates the coalescer; neither invalidates this cache. Subsequent replies therefore quote superseded or withdrawn text—even after a buffered root was replaced with a deletion notice. Invalidate cached parents on edits/deletions and fall back to the thread ID until refreshed.

4. **Minor — Reaction context pointers use a non-message timestamp.** [injection_mode.go:50](/var/folders/n0/yx26c20j4y5b0597zqpc5kzh0000gn/T/codex-gate-tree.5qinqf/tree/slack-full/adapter/injection_mode.go:50)
   Coalesced reactions also enter this renderer. For reactions on top-level messages, `ReplyToMessageID` is empty and `ProviderMessageID` is the reaction’s `event_ts`. The generated `--thread-ts` therefore identifies no Slack message. Carry the reacted-to message timestamp separately, or emit a channel-only pointer.

Tests/builds: **not run (sandbox)**. [Committed validation evidence](/var/folders/n0/yx26c20j4y5b0597zqpc5kzh0000gn/T/codex-gate-tree.5qinqf/tree/assets/pr-evidence/gp-zhtd/validation.md) reports 1,388 top-level adapter tests passing under race detection, adapter/CLI vet passing, and Python tests at 822 passed / 2 skipped. The identified scenarios lack regression coverage.

VERDICT: BLOCK
