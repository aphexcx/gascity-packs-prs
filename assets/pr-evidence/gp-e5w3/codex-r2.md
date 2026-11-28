# gp-e5w3 Codex round 2, at a5258518

This is the lane's second Codex round, on the city's gate script through the hub: class STANDARD, the whole change against main 926f1791, and REVIEW.md from main. Its answer is codex-r2.txt (model gpt-6.2-astra, a pristine worktree at the head, no tests run in its sandbox).

Verdict: BLOCK, CRITICAL 3 / MAJOR 3.

No code changed after this round. The lane stopped there and asked the mayor (BLOCKED gp-e5w3). The findings meet at two questions the ticket does not settle:
- **Which time decides that an act came after the bound?** The ticket's outcome says the recorded time. That is what RetireSpaceHistory uses for every single held act at main. gchat/README.md ("Catch-up and restart recovery") says Chat's time for the act. That is what catch-up's queue uses. Each choice has drawn a CRITICAL:
  - recorded time alone: codex-r1.md finding 1;
  - the saved act time: findings 1 and 2 below.
- **Should a split turn stay one delivery?** Splitting it gives its kept acts deliveries of their own (findings 4 and 5).

| # | Codex grade | Finding | Status |
| --- | --- | --- | --- |
| 1 | CRITICAL, NEW | In a state file written by main, the owner's recorded time reads as the later of the turn's Created and Chat's time (`OriginalCreated`). With Chat's clock ahead, that releases an owner recorded before the bound. A refresh can then save that reading as `member_created`. | Open. This came in round 2, from review round 1's MINOR. Dropping the ActAt fallback reads such an owner as at main, retired with the turn. That is the lane's proposed next change in either direction. |
| 2 | CRITICAL, NEW | A creation with no resource time has an ActAt from its event time. Catch-up's queue then skips it by its true creation time, but leaves its committed member row in place, and the split posts it. The same goes for `history_time_missing`. | Open. The queue's decision is not saved on a member of an undelivered turn (RetireQueuedHistory skips committed rows). The split cannot see it. |
| 3 | CRITICAL, PRESENT AT BASE | A turn whose first recording is at or after the bound is not looked into. A member that Chat dates before the bound is posted with it. | Open. It is the same at main, and the same for a single held act at main. It is the recorded-or-act-time question above. |
| 4 | MAJOR, NEW | A member split off starts with no attempts. When its first fragment arrives after the split, it gets a new four-POST budget. | Open. It goes away if the turn stays one delivery. Otherwise every kept member would carry the turn's attempts and next attempt. |
| 5 | MAJOR, NEW | The kept owner's new Created changes the waiting mail's key and its acknowledgement, so a second waiting mail can go out. | Open. This is codex-r1.md finding 7 again. It also goes away if the turn stays one delivery, with its Created as its life. Then the stale-POST check needs something else to compare. |
| 6 | MAJOR, PRESENT AT BASE | A mention inside the first read's join reachback is retired, and its committed ID then stops the first read from delivering it. | Open. It is the same at main, and outside this change: it is about the bound itself. |
