package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestIngestionRefusedThenAmbiguousSubset(t *testing.T) {
	for _, crash := range []bool{false, true} {
		for _, proveB := range []bool{false, true} {
			t.Run(fmt.Sprintf("crash_%v/prove_B_%v", crash, proveB), func(t *testing.T) {
				at := time.Now().UTC()
				now := at
				j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
				r := ingestionTestRecord("200.1", at, "subset attempt")
				r.CanonicalSessionID = "s1"
				r.Windows = []injectionWindow{{MessageTS: "100.1", ThreadTS: "100.0"}, {MessageTS: "200.1", ThreadTS: "200.0"}}
				if err := j.append(r); err != nil {
					t.Fatal(err)
				}
				now = at.Add(ingestionWindow + time.Second)
				calls := 0
				accepted, named := map[string]bool{}, map[string]int{}
				o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
					fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
					notify: func(_ context.Context, rows []inboundJournalRecord) error {
						found := false
						for _, row := range ingestionJournalRows(t, j) {
							if row.Phase == "ingestion_batch_attempt" && row.AlertBatch.ID == rows[0].AlertBatchID {
								found = true
								if ingestionCoverageID(row.AlertBatch.Coverage) != ingestionCoverageID(ingestionCoverageFor(rows)) {
									t.Fatal("durable attempt does not name exactly the notified pairs")
								}
							}
						}
						if !found {
							t.Fatal("notify has no durable exact attempt")
						}
						calls++
						if calls == 1 {
							return ingestionNotifyRefusal{errors.New("refused")}
						}
						id := rows[0].AlertBatchID
						if !accepted[id] {
							accepted[id] = true
							for _, row := range rows {
								for _, w := range ingestionAlertWindows(row) {
									named[w.MessageTS]++
								}
							}
						}
						if calls == 2 {
							if crash {
								panic("accepted before completion")
							}
							return context.DeadlineExceeded
						}
						return nil
					}}
				if err := o.step(context.Background()); err == nil {
					t.Fatal("expected refusal")
				}
				now = now.Add(5 * time.Second)
				recordDeliveredPublish(config{inboundJournal: j}, r.Channel, "100.0", fmt.Sprintf("%d.000000", now.Unix()), "s1")
				func() {
					defer func() {
						if p := recover(); p != nil && (!crash || p != "accepted before completion") {
							panic(p)
						}
					}()
					if err := o.step(context.Background()); !crash && err == nil {
						t.Fatal("expected ambiguous subset")
					}
				}()
				if calls != 2 {
					t.Fatalf("retry did not run: %d", calls)
				}
				if proveB {
					recordDeliveredPublish(config{inboundJournal: j}, r.Channel, "200.0", fmt.Sprintf("%d.000001", now.Unix()), "s1")
				}
				j = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
				o.cfg.inboundJournal = j
				if err := o.step(context.Background()); err != nil {
					t.Fatal(err)
				}
				late := r
				late.TargetSessions, late.CanonicalSessionID = []string{"s2"}, "s2"
				if err := j.append(late); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 3; i++ {
					if err := o.step(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if named["100.1"] != 1 || named["200.1"] != 1 || len(named) != 2 {
					t.Fatalf("unsent A hidden or accepted B repeated: %v", named)
				}
			})
		}
	}
}

func TestIngestionRefusalStateBoundedAndBacksOff(t *testing.T) {
	at := time.Date(2026, 11, 9, 23, 58, 0, 0, time.UTC)
	now := at
	j := &inboundJournal{dir: t.TempDir(), retentionDays: 1, now: func() time.Time { return now }}
	r := ingestionTestRecord("100.1", at, "refusal outage")
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	now = at.Add(ingestionWindow + time.Second)
	var attempts []time.Time
	recovered, accepted := false, 0
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(context.Context, []inboundJournalRecord) error {
			attempts = append(attempts, now)
			if recovered {
				accepted++
				return nil
			}
			return ingestionNotifyRefusal{errors.New("connection refused")}
		}}
	firstSize, maxGrowth := 0, 0
	for i := 0; i < 1000; i++ {
		_ = o.step(context.Background())
		size := len(coverageJournalSnapshot(t, j))
		if i == 0 {
			firstSize = size
		}
		if size-firstSize > maxGrowth {
			maxGrowth = size - firstSize
		}
		if i == 500 {
			j = &inboundJournal{dir: j.dir, retentionDays: 1, now: func() time.Time { return now }}
			o.cfg.inboundJournal = j
		}
		pruneCoverageFixture(t, j, now)
		now = now.Add(5 * time.Second)
	}
	states := 0
	for _, row := range ingestionJournalRows(t, j) {
		if row.Phase == "ingestion_batch_attempt" || row.Phase == "ingestion_batch_refused" {
			states++
		}
	}
	if states != 1 || maxGrowth != 0 || firstSize >= 128<<10 {
		t.Errorf("unbounded refusal state: records=%d growth=%d initial=%d", states, maxGrowth, firstSize)
	}
	if len(attempts) < 7 || len(attempts) > 25 {
		t.Fatalf("backoff attempt count=%d, want 7..25 over 1000 passes", len(attempts))
	}
	t.Logf("1000 passes: %d attempts, %d live state row, %d journal bytes, %d byte growth after first refusal", len(attempts), states, firstSize, maxGrowth)
	for i := 1; i < len(attempts); i++ {
		want := 5 * time.Second << min(i-1, 6)
		if want > 5*time.Minute {
			want = 5 * time.Minute
		}
		if got := attempts[i].Sub(attempts[i-1]); got != want {
			t.Errorf("attempt %d delay=%v want=%v", i, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(j.dir, at.Format("20060102")+".jsonl")); err != nil {
		t.Fatalf("unsettled source lost: %v", err)
	}
	recovered = true
	next := attempts[len(attempts)-1].Add(5 * time.Minute)
	now = next.Add(-time.Second)
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accepted != 0 {
		t.Fatal("recovery bypassed persisted backoff")
	}
	now = next
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 {
		t.Fatalf("first eligible recovery accepted %d mails", accepted)
	}
}

func TestIngestionMailProcessBoundary(t *testing.T) {
	for _, mode := range []string{"pipe_success", "pipe_refusal", "pipe_success_short_deadline", "pipe_refusal_short_deadline", "cancel_after_start"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			script := "#!/bin/sh\n/bin/sleep 4 &\nexit 0\n"
			if mode == "pipe_refusal" || mode == "pipe_refusal_short_deadline" {
				script = "#!/bin/sh\n/bin/sleep 4 &\nprintf refused >&2\nexit 17\n"
			}
			if mode == "cancel_after_start" {
				script = "#!/bin/sh\nexec /bin/sleep 4\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "gc"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if mode == "pipe_success_short_deadline" || mode == "pipe_refusal_short_deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer stop()
			}
			if mode == "cancel_after_start" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer stop()
			}
			start := time.Now()
			err := (&ingestionObserver{}).mailUnseen(ctx, []inboundJournalRecord{ingestionTestRecord("100.1", start, "pipe boundary")})
			if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
				t.Errorf("mail waited for descendant: %v", elapsed)
			}
			if (mode == "pipe_success" || mode == "pipe_success_short_deadline") && err != nil {
				t.Errorf("successful exit became uncertain: %v", err)
			}
			if mode == "pipe_refusal" || mode == "pipe_refusal_short_deadline" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 17 || !ingestionNotifyDefinitelyRefused(err) {
					t.Errorf("refusal lost its exit: %v", err)
				}
			}
			if mode == "cancel_after_start" && (!errors.Is(err, context.DeadlineExceeded) || ingestionNotifyDefinitelyRefused(err)) {
				t.Errorf("post-start cancellation not ambiguous: %v", err)
			}
		})
	}
}

func TestIngestionAttemptReplacementCrashBoundaries(t *testing.T) {
	for _, transition := range []string{"first_attempt", "subset_attempt", "subset_refusal"} {
		for _, stage := range []string{"before_create", "after_write", "after_sync", "after_close", "after_rename", "after_dir_sync"} {
			t.Run(transition+"/"+stage, func(t *testing.T) {
				at := time.Now().UTC()
				now := at
				j := &inboundJournal{dir: t.TempDir(), maxBytes: 1, now: func() time.Time { return now }}
				r := ingestionTestRecord("200.1", at, "atomic attempts")
				r.CanonicalSessionID = "s1"
				r.Windows = []injectionWindow{{MessageTS: "100.1", ThreadTS: "100.0"}, {MessageTS: "200.1", ThreadTS: "200.0"}}
				if err := j.append(r); err != nil {
					t.Fatal(err)
				}
				now = at.Add(ingestionWindow + time.Second)
				injected := errors.New("state storage interrupted")
				hits, calls := 0, 0
				hook := func(s string) error {
					if s == stage {
						hits++
						return injected
					}
					return nil
				}
				o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
					fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
					notify: func(context.Context, []inboundJournalRecord) error {
						calls++
						return ingestionNotifyRefusal{errors.New("refused")}
					}}
				if transition != "first_attempt" {
					if err := o.step(context.Background()); err == nil {
						t.Fatal("expected setup refusal")
					}
					now = now.Add(5 * time.Second)
					recordDeliveredPublish(config{inboundJournal: j}, r.Channel, "100.0", fmt.Sprintf("%d.000000", now.Unix()), "s1")
				}
				before := calls
				if transition == "subset_refusal" {
					o.notify = func(context.Context, []inboundJournalRecord) error {
						calls++
						j.attemptWriteHook = hook
						return ingestionNotifyRefusal{errors.New("refused")}
					}
				} else {
					j.attemptWriteHook = hook
				}
				if err := o.step(context.Background()); !errors.Is(err, injected) {
					t.Fatalf("missing storage error: %v", err)
				}
				if hits != 1 {
					t.Fatalf("boundary not exercised: %d", hits)
				}
				if transition != "subset_refusal" && calls != before {
					t.Fatal("notify ran before durable state succeeded")
				}
				j = &inboundJournal{dir: j.dir, maxBytes: 1, now: func() time.Time { return now }}
				o.cfg.inboundJournal = j
				now = now.Add(10 * time.Second)
				named := map[string]int{}
				o.notify = func(_ context.Context, rows []inboundJournalRecord) error {
					for _, row := range rows {
						for _, w := range ingestionAlertWindows(row) {
							named[w.MessageTS]++
						}
					}
					return nil
				}
				if err := o.step(context.Background()); err != nil {
					t.Fatal(err)
				}
				late := r
				late.TargetSessions, late.CanonicalSessionID = []string{"s2"}, "s2"
				if err := j.append(late); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 3; i++ {
					if err := o.step(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if named["100.1"] != 1 || named["200.1"] != 1 {
					t.Fatalf("recovery hid or repeated a line: %v", named)
				}
				states := 0
				for _, row := range ingestionJournalRows(t, j) {
					if row.AlertBatch != nil && row.AlertBatch.ID == ingestionCoverageID(ingestionCoverageFor([]inboundJournalRecord{r})) && (row.Phase == "ingestion_batch_attempt" || row.Phase == "ingestion_batch_refused") {
						states++
					}
				}
				if states > 1 {
					t.Fatalf("crash left %d state records", states)
				}
			})
		}
	}
}

func TestIngestionVersionTwoPlanRemainsAmbiguous(t *testing.T) {
	at := time.Now().UTC()
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	r := ingestionTestRecord("100.1", at, "v2 acceptance unknown")
	r.CanonicalSessionID = "s1"
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	plan := ingestionAlertBatch{ID: "legacy-v2", Version: 2, Channel: r.Channel, Members: ingestionAlertMembers([]inboundJournalRecord{r}), Coverage: ingestionCoverageFor([]inboundJournalRecord{r})}
	if err := j.append(inboundJournalRecord{Phase: "ingestion_batch", AlertBatch: &plan}); err != nil {
		t.Fatal(err)
	}
	now = at.Add(ingestionWindow + time.Second)
	recordDeliveredPublish(config{inboundJournal: j}, r.Channel, r.ThreadTS, fmt.Sprintf("%d.000000", now.Unix()), "s1")
	j = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(context.Context, []inboundJournalRecord) error {
			t.Fatal("all-proven legacy plan sent a new mail")
			return nil
		}}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, row := range ingestionJournalRows(t, j) {
		if row.Phase == "ingestion_batch_done" {
			if row.AlertBatch.Sent || !row.AlertBatch.Unconfirmed || !row.AlertBatch.All {
				t.Fatalf("legacy uncertainty lost: %+v", row.AlertBatch)
			}
			return
		}
	}
	t.Fatal("legacy plan did not complete")
}

func TestIngestionCoverageSourceIgnoresBatchAttribution(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	now := at.Add(48 * time.Hour)
	j := &inboundJournal{dir: t.TempDir(), retentionDays: 1}
	r := ingestionTestRecord("100.1", at, "pair source")
	r.Status, r.Phase = "ingested", "ingestion"
	coverage := ingestionCoverageFor([]inboundJournalRecord{r})
	coverage[0].BatchID = "old-batch"
	appendCoverageFixture(t, j, at, r, inboundJournalRecord{Phase: "ingestion_coverage", AlertCoverage: coverage})
	coverage[0].BatchID = "new-batch"
	late := r
	late.TargetSessions = []string{"s2"}
	appendCoverageFixture(t, j, now, late, inboundJournalRecord{Phase: "ingestion_coverage", AlertCoverage: coverage})
	pruneCoverageFixture(t, j, now)
	count := 0
	for _, row := range ingestionJournalRows(t, j) {
		if row.Phase == "ingestion_coverage" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("already surviving pair copied by batch attribution: %d", count)
	}
}

func TestIngestionLegacyAttemptStateCanRetry(t *testing.T) {
	for _, version := range []int{0, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			at := time.Now().UTC()
			now := at
			j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
			r := ingestionTestRecord("100.1", at, "legacy retry")
			if err := j.append(r); err != nil {
				t.Fatal(err)
			}
			plan := ingestionAlertBatch{ID: "legacy-retry", Channel: r.Channel, Version: version, Members: ingestionAlertMembers([]inboundJournalRecord{r})}
			if version == 2 {
				plan.Coverage = ingestionCoverageFor([]inboundJournalRecord{r})
			}
			if err := j.append(inboundJournalRecord{Phase: "ingestion_batch", AlertBatch: &plan}); err != nil {
				t.Fatal(err)
			}
			now = at.Add(ingestionWindow + time.Second)
			calls := 0
			o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
				fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
				notify: func(context.Context, []inboundJournalRecord) error {
					calls++
					if calls == 1 {
						return ingestionNotifyRefusal{errors.New("refused")}
					}
					return nil
				}}
			if err := o.step(context.Background()); err == nil || calls != 1 {
				t.Fatalf("legacy plan could not attempt: calls=%d err=%v", calls, err)
			}
			now = now.Add(5 * time.Second)
			o.cfg.inboundJournal = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("legacy retry calls=%d", calls)
			}
		})
	}
}

func TestIngestionPendingAttemptSurvivesProofAtRetentionBoundary(t *testing.T) {
	at := time.Date(2026, 11, 9, 23, 56, 0, 0, time.UTC)
	now := at
	j := &inboundJournal{dir: t.TempDir(), retentionDays: 1, now: func() time.Time { return now }}
	r := ingestionTestRecord("100.1", at, "pending attempt retention")
	r.CanonicalSessionID = "s1"
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	now = at.Add(ingestionWindow + time.Second)
	named := 0
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch:  func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(context.Context, []inboundJournalRecord) error { named++; return context.DeadlineExceeded }}
	if err := o.step(context.Background()); err == nil {
		t.Fatal("expected accepted but unconfirmed mail")
	}
	now = at.Add(5 * time.Minute)
	recordDeliveredPublish(config{inboundJournal: j}, r.Channel, r.ThreadTS, fmt.Sprintf("%d.000000", now.Unix()), "s1")
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	late := r
	late.TargetSessions, late.CanonicalSessionID = []string{"s2"}, "s2"
	if err := j.append(late); err != nil {
		t.Fatal(err)
	}
	o.cfg.inboundJournal = &inboundJournal{dir: j.dir, retentionDays: 1, now: func() time.Time { return now }}
	o.notify = func(context.Context, []inboundJournalRecord) error { named++; return nil }
	for i := 0; i < 3; i++ {
		if err := o.step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if named != 1 {
		t.Fatalf("proof pruned the pending plan before its All completion: named=%d", named)
	}
}
