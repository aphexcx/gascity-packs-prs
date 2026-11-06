# The load flake in restore.test.js and review.test.js (gp-wf4e, 11/4 22:5x-23:07Z)

The gp-9hf1 READY named it as a follow-up: a fixed clock pump followed by an
un-pumped wait flakes under a load spike. This round met it twice on citadel
(other city agents running; load averages 11 to 67) and converted the sites
it diagnosed. Nothing here is a wrong outcome: every failure is a wait that
timed out because nothing more was due on the test clock.

## 1. review.test.js "Minor 6" (a gap wait)

- Seen: the full suite at the round's first head, load 11.6: `✖ Minor 6 ... Error: timed out waiting for condition` (the un-named waitFor for A's restore_gap).
- Alone at the round head, load 14-20: 4 pass, 1 fail in 5 runs.
- Alone at ba915531 (git archive, the gate's head, unchanged tests), load 20-27: 7 pass, 1 fail in 8 runs. PRE-EXISTING.
- Instrumented at ba915531 (the failing run, the child's log and gc's transcript reads, clock relative to the restore):

```
DIAG restored at clock 1793804400000, clock now 1793804403000 (+3000 ms)
2026-11-04T22:57:49.143Z watermark read (schedule): 1 watermark(s), 0 gap(s) standing
2026-11-04T22:57:49.177Z watermark read (schedule): 1 watermark(s), 0 gap(s) standing
2026-11-04T22:57:49.278Z watermark read (schedule): 1 watermark(s), 0 gap(s) standing
[[0,"0001","asc",["wamid.CG0"]],[0,"0001","asc",["wamid.CG0"]],[0,"0001","asc",["wamid.CG1"]]]
```

  Every read ran before the restore landed (the last one still returned CG1),
  and the last of them ended while `pump(3000)` was moving the clock, so it
  stamped lastWatermarkReadAt late and the next scheduled read (every 2000 ms
  of the test clock) fell due after the pump's last step: no read after the
  restore, no gap, the wait timed out.

## 2. restore.test.js "(11a)" (a read-back wait)

- Seen: restore + review + gate concurrently at the round head, load 25-27: `✖ (11a) ... Error: timed out waiting for the ack of delivery 2`.
- Alone at the round head (a copy, instrumented), load 33-54: 14 of 14 pass. Concurrently with gate.test.js, load 58-67, the first run failed:

```
DIAG clock +6000 ms
2026-11-04T23:06:21.878Z registered with gc as provider=whatsapp account=dynasty callback=...
2026-11-04T23:06:22.158Z watermark read (schedule): 1 watermark(s), 0 gap(s) standing
2026-11-04T23:06:22.287Z watermark read (schedule): 1 watermark(s), 0 gap(s) standing
[[0,"POST /extmsg/adapters",null,null,null],[0,"POST /extmsg/inbound",null,null,"wamid.R0"],[0,"GET /extmsg/transcript","1","asc",null],[0,"GET /extmsg/transcript","1","asc",null],[0,"POST /extmsg/inbound",null,null,"wamid.R1"]]
```

  After R1's POST the child made NO request in 12 s of real time: no receipt
  read-back, no watermark read, nothing of the in-doubt path this round
  changed. The read-back is due at `clock() + 5000` taken when the child
  handles gc's pending answer (afterVerdict); the test proceeds as soon as
  the accepted line is on disk, and under that load the child set the due
  time after `pump(6000)` had moved the clock, so it fell due after the
  pump's last step.

## 3. restore.test.js "(11g)" (a stays check after a restart)

- Seen: restore + review + gate concurrently after the conversion, load 65-78, 1 of 4 runs: `✖ (11g) ... Error: no gap after the clear stopped holding` (1.6 s: at the first check).
- The check reads the journal FILE for restore_gap lines right after a SIGKILL and restart; the cleared gap's lines leave the file only at the restarted child's startup compaction (journal.js compact keeps the fold's standing gaps only), and at that load the child booted after the 13-step pump. The same at ba915531.
- The fix: wait for that compaction (no restore_gap line in the file) before the pump; the stays check after it is unchanged.

## The conversion (one commit, tests only)

Where the due time is stamped from the child's clock at an unsynchronised
moment (the scheduled watermark read, the pending read-back), the fixed pump
plus un-pumped wait becomes `r.pumpUntil(<the same condition>)`: the clock
keeps moving in the same steps until the outcome lands; every assertion after
it is unchanged. Sites: restore.test.js (11a), (11b), (11c) (the read-back),
(11d2), (11f), (11g), (11j) (the gap), review.test.js Minor 6 (the gap).
Waits keyed to a journaled time (posted.at + the grace) are not exposed and
are left as they are; the rest of the class in the other files stays the
gp-9hf1 follow-up.
