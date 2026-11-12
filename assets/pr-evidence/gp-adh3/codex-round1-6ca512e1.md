# codex-gate review: base 28918336555abbeb4f52af3c05029e801c59cb6f (merge base of --base 28918336555abbeb4f52af3c05029e801c59cb6f = 28918336555abbeb4f52af3c05029e801c59cb6f and the head) head 6ca512e1c91144a60535066c1373a7befe9fb6db; 8 file(s); class STANDARD; model gpt-6-astra; 2026-11-12T03:50:46Z
# codex route: the hub
# reviewed in a pristine detached worktree at the head (--head 6ca512e1c91144a60535066c1373a7befe9fb6db; the working tree of the lane worktree not consulted)
# verdict rule: CLEAN with no CRITICAL or MAJOR finding (MINORs are reported, deferred by the lane, never blocking)
#  gchat/CHANGELOG.md                                 |   3 ++
#  gchat/skills/google-chat/SKILL.md                  |  34 +++++++++++---
#  slack-full/CHANGELOG.md                            |   3 ++
#  slack-full/template-fragments/slack-v0.template.md |  43 ++++++++++++++++++
#  slack-full/tests/test_slack_prompt_contract.py     |  31 +++++++++++++
#  tests/test_texting_rules.py                        | 147 +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
#  whatsapp/CHANGELOG.md                              |   3 ++
#  whatsapp/skills/whatsapp/SKILL.md                  |  31 +++++++++++++
#  8 files changed, 290 insertions(+), 5 deletions(-)

1. **MAJOR — Preserve per-agent identity for reaction-only replies** — [slack-full/template-fragments/slack-v0.template.md:67](<pristine-tree>/slack-full/template-fragments/slack-v0.template.md:67).

   **CLASS:** The new guidance requires reactions through a command that always uses the shared bot token (`slack-full/adapter/main.go:3243`). Both documented command forms have this limitation: company DMs and MPIMs require a participating agent’s token; company-room reactions appear as the switchboard instead of the responding agent.

   **SEQUENCE:** Configure the existing per-agent DM setup with distinct switchboard and agent tokens. Deliver “thanks” to the agent, then follow the explicit command with that DM’s channel, message timestamp, and `--emoji +1`. Slack receives the switchboard token and returns `channel_not_found`; the command prints `delivered:false` but exits successfully. Following “No text follows it” leaves the person without the prescribed acknowledgement. A regression test can reject the switchboard token and accept the owner token using the existing DM fixtures.

   **NEW:** The reaction-only requirement introduces this regression; the command’s token limitation already exists at the base. Resolve the company identity before reacting, or provide a supported reply fallback when the reaction cannot be delivered.

Tests: **not run (sandbox)**. No test evidence for this delta was found under `assets/pr-evidence/` at the head.

VERDICT: BLOCK
