# gp-e5w3 review, round 2, at a5a19dc8

The reviewer was a second fresh subagent on the builder's model (claude-opus-5-5), with the same brief as round 1. It reviewed the whole change first. Only after that did it read the round 1 report and judge each of that report's findings. It ran probes in a scratch folder outside the repository through Go overlays, and it did not write in the repository.

Verdict: BLOCK, CRITICAL 0 / MAJOR 1 / MINOR 2 / NOTE 5.

| # | Grade | Finding | Outcome |
| --- | --- | --- | --- |
| 1 | MAJOR (UNKNOWN on whether a city runs the gchat pack) | A merged turn that has already ended unresolved, for example after gc rejected it, is not split. The retirement judges it whole by its Created. It becomes committed/local, so the act recorded at the bound leaves `Unresolved` and a replay cannot reach it. A single act at the bound stays unresolved. Main does the same. | Not changed: left for the mayor. Such a turn is not one gc refused and the adapter retries. An operator owns it. Keeping the later act needs a notice of its own, which changes what the operator is told and mailed. That needs a ruling or its own ticket. |
| 2 | MINOR | A turn whose POST is in flight, or whose receipt is pending, when the bound arrives. It becomes a frame-only notice, and that includes the act after the bound. If gc then answers "failed", the answer is dropped, and that act is never posted. Main does the same. | Not changed: left for the mayor. The turn's delivery is unknown when the bound arrives. Posting its later act could post it twice. Row `post_may_have_landed` pins that such a turn is not split. |
| 3 | MINOR | The split gave a kept owner with a folded edit the kind `message.created`. A rolled-back adapter's superseded check would then withdraw it. | Fixed in bf14cbaf. The owner gets back hold's kind "coalesced" when an edit is folded into it. Row `kept_owner_keeps_its_fold`, mutant m14. |
| N1 | NOTE | `rewritable` ignores the fragment mark. | Round 1 note 6: kept as a judgment for the mayor. |
| N2 | NOTE | The split changes the owner's Created, which is the raise mail's life. | Round 1 note 7: kept as a judgment for the mayor. |
| N3 | NOTE | A state file written by main still loses an owner recorded at the bound that Chat dates before the turn's first act. The owner's event carries `received_at`, which gives a closer reading. | Not changed. `received_at` is the receive time of the last text a record carried. For a creation with a folded edit, that is the edit's time, which is later than the creation's recording, so it would read some creations as later than they were. The fallback keeps to two times that are at or before the recording. |
| N4 | NOTE | An edit folded into a creation, when a later edit with its own record has superseded it. It was unfolded and ended as an unresolved "withdrawn" notice. | Fixed in bf14cbaf. Only an edit that no later revision superseded leaves its creation. Row `folded_edit_superseded_by_a_later_edit`, mutant m12. |
| N5 | NOTE | No row tests that a pass which read the old turn before the split cannot POST its text. The reviewer's probe, a bound installed during the in-gate wait, shows that it holds: `body-002=0 body-003=1`. | Not pinned by a row. The property is saveAttempt's check of Created, which every split changes for a kept owner. |

The reviewer judged round 1's findings at a5a19dc8: 1, 2, 3 (for the case it showed), 4 and 5 resolved; 6 and 7 not resolved, kept by the builder.
