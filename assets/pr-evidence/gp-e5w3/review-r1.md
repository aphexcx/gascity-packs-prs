# gp-e5w3 review, round 1, at 34b0a5a6

The reviewer was a fresh subagent on the builder's model (claude-opus-5-5). Its brief gave it the ticket's outcome and the code to read, with no conclusion of the builder's. It ran probes in a scratch folder outside the repository through Go overlays, and it did not write in the repository.

Verdict: BLOCK, CRITICAL 0 / MAJOR 2 / MINOR 1 / NOTE 4.

| # | Grade | Finding | Outcome |
| --- | --- | --- | --- |
| 1 | MAJOR | After the refusal, the refresh takes a deleted first act out of the turn. The turn keeps that act's time as its Created, and every act still in it is after the bound. Then the turn is not split. The retirement judges it by the old time, and the later acts are lost. | Fixed in d473c674. A turn is split when its Created is before the bound and any act of it is not. Row `first_act_deleted_after_the_refusal` (red at 34b0a5a6). Mutant m9 (the first round's rule) fails it. |
| 2 | MAJOR | An edit folded into its message's held creation. The creation is before the bound, and the edit is after it. The creation is retired, and the edit is never posted. This happens with the creation held alone and inside a refused turn, and it is the same at main. | Fixed in a6aaa797 and bf14cbaf, then withdrawn in 01cc478c after the Codex round (codex-r1.md findings 1, 3 and 8). The unfold could post an edit that Chat dates before the bound. It could post an edit twice after a replay, and it dropped the edit's label. Left for the mayor: the same as at main, and its own ticket. |
| 3 | MINOR | In a state file written by main, the owner reads as recorded at the turn's time. A bound inside such a turn retires the owner, which may have been recorded at the bound. | Changed in d473c674. The owner reads as recorded at the later of the turn's time and its own act's time (ActAt). Row `state_file_without_the_owners_time`, mutant m10. One case is left: the second review's note 3. |
| 4 | NOTE | The message of fac6df13 says five rows failed at main. Six did. | No history rewrite. The message of 7748b133 says six. |
| 5 | NOTE | No row covered the refresh's `copy.Created = member.OriginalCreated()`. | Row `edit_of_the_first_act_after_the_refusal` (7748b133). Mutant m8 fails it. |
| 6 | NOTE | The split also runs for a turn with a fragment in a terminal ("partial"). The members split off get a fresh life without the fragment mark. | First kept, then changed in 01cc478c after the Codex round graded it MAJOR (codex-r1.md finding 2). Such a turn is not split and is retired whole, as at main. Row `fragment_in_a_terminal`. |
| 7 | NOTE | The split changes the owner's Created, which is also the raise mail's life. If a waiting mail is in flight while the split runs, a second one can go out. | Kept, as a judgment for the mayor. The change of Created is what stops a pass that read the old turn from POSTing its text: saveAttempt checks it. The race needs a mail in flight at a first read. |
