# Fresh reviewer pass 1

Read-only reviewer subagent, same model, against origin/jp/slack-inject-compact
(52cbd2f8). No suites/builds run by the reviewer; no files modified.

1. P1: company-room mention-only deliveries lose identifying coordinates in
   minimal mode. mention_only.go assumed Text was prepared, but
   company_mention_only.go constructs raw text. Fix at a shared boundary with
   an explicit prepared marker and cover both callers.
2. P1: newest-window retention changes historical ownership routing. The same
   fetch helper served ownership scans, discarding early own-bot evidence in
   long pre-registry threads. Separate context-window retention from an
   all-page existence scan; test early and late ownership evidence.
3. P2: mention-only injections bypass the inbound journal. Journal the actual
   rendered bytes, target session and accurately distinguished confirmation
   status at postMentionOnlyReminder, including retry attempts.
4. P2: full batch windows can describe removed context or omit members.
   applyDeletion cleared text but retained window arrays. Legacy/reaction
   members disappeared when another member had metadata. Clear deleted
   windows and emit explicit unknown metadata for every other member.

Reviewer covered rendering, pagination callers, coalescing, spool persistence,
receipts, storage, replay parsing, documentation and evidence. Replay assumptions,
acceptance/delivery distinctions and crash gap were appropriately qualified.
Private inputs were not independently verified; city memory and whole mayor
transcripts were not read.

Implementation response: all four findings fixed, with focused passing tests
for raw/prepared session paths, mention-only journal target/bytes, early/late
ownership scanning, deleted/unknown batch windows, spool replay and held receipt
conclusions. Follow-up review requested after these fixes.
