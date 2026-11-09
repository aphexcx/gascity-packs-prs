package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ingestionReadAttempt(t *testing.T, j *inboundJournal, id string) ingestionAttemptState {
	t.Helper()
	b, err := os.ReadFile(j.alertAttemptPath(id))
	if err != nil {
		t.Fatal(err)
	}
	var state ingestionAttemptState
	if err := json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestIngestionAttemptLegacySixThousandReceipts(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	now := at
	j := &inboundJournal{dir: t.TempDir(), retentionDays: 1, now: func() time.Time { return now }}
	var rows []inboundJournalRecord
	for i := 0; i < 6000; i++ {
		rows = append(rows, ingestionTestRecord(fmt.Sprintf("1794198796.%06d", i), at, "legacy body"))
	}
	plan := ingestionAlertBatch{ID: ingestionAlertID(ingestionAlertMembers(rows)), Channel: "C1", Members: ingestionAlertMembers(rows), Created: at}
	if err := j.append(inboundJournalRecord{Phase: "ingestion_batch", AlertBatch: &plan}); err != nil {
		t.Fatal(err)
	}
	before := coverageJournalSnapshot(t, j)
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now }}
	if _, err := o.beginAlertAttempt(plan, rows); err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(coverageJournalSnapshot(t, j), []byte{'\n'}) {
		if len(line) >= 1<<20 {
			t.Errorf("attempt made a %d-byte journal line unreadable", len(line))
		}
	}
	if !bytes.Equal(before, coverageJournalSnapshot(t, j)) {
		t.Error("attempt changed the append-only journal")
	}
	state := ingestionReadAttempt(t, j, plan.ID)
	if len(state.Coverage) != 6000 || len(state.Prior) != 0 || ingestionCoverageID(state.Coverage) != ingestionCoverageID(ingestionCoverageFor(rows)) {
		t.Fatal("legacy attempt expanded or lost its exact pairs")
	}
	encoded, err := json.Marshal(state)
	if err != nil || len(encoded) > ingestionAttemptMaxBytes {
		t.Fatalf("unbounded legacy sidecar: bytes=%d err=%v", len(encoded), err)
	}
	if _, err := o.pending(); err != nil {
		t.Fatalf("observation blocked after legacy attempt: %v", err)
	}
	now = at.Add(48 * time.Hour)
	for _, channel := range []string{"C1", "C2"} {
		r := ingestionTestRecord("200.1", now.Add(-ingestionWindow-time.Second), "other channel")
		r.Channel = channel
		if err := j.append(r); err != nil {
			t.Fatalf("append/pruning blocked for %s: %v", channel, err)
		}
	}
	if pending, err := o.pending(); err != nil || len(pending) != 2 {
		t.Fatalf("channels did not remain observable: rows=%d err=%v", len(pending), err)
	}
	t.Logf("legacy plan: 6000 receipts, %d journal bytes; attempt changed no journal bytes", len(before))
}

func TestIngestionAttemptPlanWithoutFinalNewline(t *testing.T) {
	for _, nextDay := range []bool{false, true} {
		t.Run(fmt.Sprintf("next_day_%v", nextDay), func(t *testing.T) {
			at := time.Date(2026, 11, 9, 23, 56, 0, 0, time.UTC)
			now := at
			j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
			r := ingestionTestRecord("100.1", at, "unterminated plan")
			if err := j.append(r); err != nil {
				t.Fatal(err)
			}
			plan := ingestionAlertBatch{ID: ingestionCoverageID(ingestionCoverageFor([]inboundJournalRecord{r})), Version: ingestionAlertSidecarVersion,
				Channel: r.Channel, Members: ingestionAlertMembers([]inboundJournalRecord{r}), Coverage: ingestionCoverageFor([]inboundJournalRecord{r}), Created: at}
			if err := j.append(inboundJournalRecord{Phase: "ingestion_batch", AlertBatch: &plan}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(j.dir, at.Format("20060102")+".jsonl")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bytes.TrimSuffix(b, []byte{'\n'}), 0600); err != nil {
				t.Fatal(err)
			}
			now = at.Add(ingestionWindow + time.Second)
			if nextDay {
				now = at.Add(5 * time.Minute)
			}
			j = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
			calls := 0
			o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
				fetch:  func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
				notify: func(context.Context, []inboundJournalRecord) error { calls++; return nil }}
			if _, err := o.pending(); err != nil || len(o.preparedBatches) != 1 {
				t.Fatalf("replay lost valid final plan: %v", err)
			}
			if _, err := o.beginAlertAttempt(o.preparedBatches[0], []inboundJournalRecord{r}); err != nil {
				t.Fatalf("attempt writer disagrees with replay: %v", err)
			}
			for i := 0; i < 2; i++ {
				if err := o.step(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 1 {
				t.Fatalf("recovered plan mailed %d times", calls)
			}
		})
	}
}

func ingestionSidecarFixture(t *testing.T) (*inboundJournal, *ingestionObserver, ingestionAlertBatch, inboundJournalRecord) {
	t.Helper()
	now := time.Date(2026, 11, 9, 23, 59, 30, 0, time.UTC)
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	r := ingestionTestRecord("100.1", now.Add(-ingestionWindow-time.Second), "sidecar")
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil }}
	plan, _, err := o.prepareAlertBatch(r.Channel, []inboundJournalRecord{r})
	if err != nil {
		t.Fatal(err)
	}
	return j, o, plan, r
}

func TestIngestionAttemptSidecarCrashRecovery(t *testing.T) {
	for _, stage := range []string{"before_create", "after_mkdir", "after_parent_sync", "after_write", "after_sync", "after_close", "after_rename", "after_dir_sync"} {
		t.Run(stage, func(t *testing.T) {
			j, o, plan, r := ingestionSidecarFixture(t)
			before := coverageJournalSnapshot(t, j)
			interrupted := errors.New("crash before notify")
			j.attemptWriteHook = func(at string) error {
				if at == stage {
					return interrupted
				}
				return nil
			}
			if _, err := o.beginAlertAttempt(plan, []inboundJournalRecord{r}); !errors.Is(err, interrupted) {
				t.Fatalf("missing crash boundary: %v", err)
			}
			if !bytes.Equal(before, coverageJournalSnapshot(t, j)) {
				t.Fatal("sidecar crash altered journal bytes")
			}
			// Restart across midnight; the sidecar is not day/segment dependent.
			now := o.now().Add(time.Minute)
			j = &inboundJournal{dir: j.dir, now: func() time.Time { return now }, maxBytes: 1}
			o.cfg.inboundJournal, o.now = j, func() time.Time { return now }
			if _, err := o.pending(); err != nil || len(o.preparedBatches) != 1 {
				t.Fatalf("pending plan lost: %v", err)
			}
			replayed := o.preparedBatches[0]
			ambiguous := stage == "after_rename" || stage == "after_dir_sync"
			if replayed.attempted != ambiguous || (len(replayed.possible) > 0) != ambiguous {
				t.Fatalf("wrong crash classification: attempted=%v possible=%d", replayed.attempted, len(replayed.possible))
			}
			calls := 0
			o.notify = func(_ context.Context, rows []inboundJournalRecord) error {
				calls++
				if ingestionCoverageID(ingestionCoverageFor(rows)) != ingestionCoverageID(plan.Coverage) {
					t.Fatal("recovered attempt did not retry the full set")
				}
				return nil
			}
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := o.step(context.Background()); err != nil || calls != 1 {
				t.Fatalf("recovery mails=%d err=%v", calls, err)
			}
			if _, err := os.Stat(j.alertAttemptPath(plan.ID)); !os.IsNotExist(err) {
				t.Fatalf("completed sidecar retained: %v", err)
			}
		})
	}
}

func TestIngestionAttemptSidecarCleanup(t *testing.T) {
	for _, mode := range []string{"completion", "crash_after_done", "prune_done", "unknown", "temporary", "temporary_prune"} {
		t.Run(mode, func(t *testing.T) {
			j, o, plan, r := ingestionSidecarFixture(t)
			if _, err := o.beginAlertAttempt(plan, []inboundJournalRecord{r}); err != nil {
				t.Fatal(err)
			}
			path := j.alertAttemptPath(plan.ID)
			switch mode {
			case "completion":
				if err := o.finishAlertBatch(plan, []inboundJournalRecord{r}, true); err != nil {
					t.Fatal(err)
				}
			case "crash_after_done", "prune_done":
				// Simulate death after the durable completion but before unlink.
				done := plan
				done.Sent, done.All, done.Members, done.Coverage = true, true, nil, nil
				if err := j.append(inboundJournalRecord{Phase: "ingestion_batch_done", AlertBatch: &done}); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("fixture did not leave the crash orphan: %v", err)
				}
				if mode == "prune_done" {
					r.Status, r.Phase, r.Alerted = "delivered_unseen", "ingestion_alert", true
					r.AlertCoverage, r.AlertVersion = plan.Coverage, ingestionAlertCoverageVersion
					if err := j.append(r); err != nil {
						t.Fatal(err)
					}
					old := filepath.Join(j.dir, o.now().Format("20060102")+".jsonl")
					now := o.now().AddDate(0, 0, 8)
					j.now = func() time.Time { return now }
					if err := j.append(inboundJournalRecord{Phase: "retention_tick"}); err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(old); !os.IsNotExist(err) {
						t.Fatalf("done row not pruned: %v", err)
					}
				}
			case "unknown", "temporary", "temporary_prune":
				path = j.alertAttemptPath("unknown-plan")
				if strings.HasPrefix(mode, "temporary") {
					path = filepath.Join(j.dir, "attempts", ".attempt-crash")
				}
				if err := os.WriteFile(path, []byte("not valid JSON; never replay this"), 0600); err != nil {
					t.Fatal(err)
				}
				if mode == "temporary_prune" {
					pruneCoverageFixture(t, j, o.now())
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("ordinary prune retained temporary file: %v", err)
					}
				}
			}
			if mode != "completion" && mode != "prune_done" {
				j = &inboundJournal{dir: j.dir}
				o.cfg.inboundJournal = j
				if _, err := o.pending(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("orphan sidecar not removed: %v", err)
			}
			if mode == "unknown" || strings.HasPrefix(mode, "temporary") {
				if !ingestionReadAttempt(t, j, plan.ID).AttemptedAt.Equal(o.now()) {
					t.Fatal("cleanup changed the live attempt")
				}
			}
		})
	}
}

func TestIngestionAttemptSidecarBounds(t *testing.T) {
	for _, mode := range []string{"oversize_write", "oversize_read", "aggregate_read", "invalid_json", "wrong_id", "wrong_version", "wrong_plan_version", "journal_over_budget"} {
		t.Run(mode, func(t *testing.T) {
			j, o, plan, r := ingestionSidecarFixture(t)
			state, err := o.beginAlertAttempt(plan, []inboundJournalRecord{r})
			if err != nil {
				t.Fatal(err)
			}
			path := j.alertAttemptPath(plan.ID)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "oversize_write":
				state.Coverage[0].Session = strings.Repeat("x", ingestionAttemptMaxBytes)
				if err := j.writeAlertAttempt(state); err == nil {
					t.Fatal("oversize state accepted")
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, original) {
					t.Fatal("failed oversize write replaced durable state")
				}
				return
			case "oversize_read":
				if err := os.Truncate(path, ingestionAttemptMaxBytes+1); err != nil {
					t.Fatal(err)
				}
			case "aggregate_read":
				j.preparedBatches = map[string]ingestionAlertBatch{plan.ID: plan}
				remaining := int64(len(original) - 1)
				if err := j.recoverAlertAttemptsLocked(&remaining); err == nil {
					t.Fatal("sidecar bypassed remaining observation budget")
				}
				return
			case "invalid_json":
				if err := os.WriteFile(path, []byte("{\"id\":"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong_id", "wrong_version", "wrong_plan_version":
				if mode == "wrong_id" {
					state.ID = "different-plan"
				} else if mode == "wrong_plan_version" {
					state.PlanVersion++
				} else {
					state.Version++
				}
				b, _ := json.Marshal(state)
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "journal_over_budget":
				segment := filepath.Join(j.dir, o.now().Format("20060102")+".jsonl")
				if err := os.Truncate(segment, 64<<20+1); err != nil {
					t.Fatal(err)
				}
				// The attempt writer must not scan any journal segment.
				if err := j.writeAlertAttempt(state); err != nil {
					t.Fatalf("sidecar writer read the journal: %v", err)
				}
			}
			if _, err := o.pending(); err == nil {
				t.Fatal("invalid/oversize persistent state silently accepted")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("failed replay deleted a pending sidecar")
			}
		})
	}
}

func TestIngestionAttemptSidecarFailureDoesNotBlockOtherChannels(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		for _, stage := range []string{"before_create", "after_write", "after_sync", "after_close", "after_rename", "after_dir_sync"} {
			t.Run(fmt.Sprintf("refusal_%v/%s", refusal, stage), func(t *testing.T) {
				j, o, _, r := ingestionSidecarFixture(t)
				r.Channel = "C2"
				if err := j.append(r); err != nil {
					t.Fatal(err)
				}
				injected := errors.New("first channel state storage failed")
				hits := 0
				hook := func(at string) error {
					if at == stage && hits == 0 {
						hits++
						return injected
					}
					return nil
				}
				if !refusal {
					j.attemptWriteHook = hook
				}
				mailed := map[string]int{}
				o.notify = func(_ context.Context, rows []inboundJournalRecord) error {
					if rows[0].Channel == "C1" {
						if !refusal {
							t.Fatal("notify ran after failed pre-notify fence")
						}
						j.attemptWriteHook = hook
						return ingestionNotifyRefusal{errors.New("refused")}
					}
					mailed[rows[0].Channel]++
					return nil
				}
				if err := o.step(context.Background()); !errors.Is(err, injected) {
					t.Fatalf("storage error not collected: %v", err)
				}
				if hits != 1 || mailed["C2"] != 1 {
					t.Fatalf("first channel blocked second: failures=%d mailed=%v", hits, mailed)
				}
			})
		}
	}
}

func TestIngestionAttemptSidecarDeletionCrash(t *testing.T) {
	for _, stage := range []string{"before_completion_sync", "before_remove", "after_remove", "after_remove_dir_sync"} {
		t.Run(stage, func(t *testing.T) {
			j, o, plan, _ := ingestionSidecarFixture(t)
			injected := errors.New("completion cleanup interrupted")
			hits, calls := 0, 0
			j.attemptWriteHook = func(at string) error {
				if at == stage {
					hits++
					return injected
				}
				return nil
			}
			o.notify = func(context.Context, []inboundJournalRecord) error { calls++; return nil }
			if err := o.step(context.Background()); !errors.Is(err, injected) || hits != 1 || calls != 1 {
				t.Fatalf("completion boundary not exercised: err=%v hits=%d calls=%d", err, hits, calls)
			}
			j = &inboundJournal{dir: j.dir, now: o.now}
			o.cfg.inboundJournal = j
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("completion cleanup failure repeated accepted mail")
			}
			if _, err := os.Stat(j.alertAttemptPath(plan.ID)); !os.IsNotExist(err) {
				t.Fatalf("restart retained completed state: %v", err)
			}
		})
	}
}

func TestIngestionAttemptJournalMarkersMigrate(t *testing.T) {
	for _, version := range []int{3, 4} {
		for _, refused := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/refused_%v", version, refused), func(t *testing.T) {
				j, o, plan, r := ingestionSidecarFixture(t)
				plan.Version = version
				if err := j.append(inboundJournalRecord{Phase: "ingestion_batch", AlertBatch: &plan}); err != nil {
					t.Fatal(err)
				}
				marker := ingestionAlertBatch{ID: plan.ID, Version: version}
				if version == 4 {
					marker.Coverage = plan.Coverage
				}
				if err := j.append(inboundJournalRecord{Phase: "ingestion_batch_attempt", AlertBatch: &marker}); err != nil {
					t.Fatal(err)
				}
				if refused {
					marker.Refusals, marker.RetryAt = 1, o.now().Add(5*time.Second).UnixNano()
					if err := j.append(inboundJournalRecord{Phase: "ingestion_batch_refused", AlertBatch: &marker}); err != nil {
						t.Fatal(err)
					}
				}
				before := coverageJournalSnapshot(t, j)
				j = &inboundJournal{dir: j.dir, now: o.now}
				o.cfg.inboundJournal = j
				if _, err := o.pending(); err != nil || len(o.preparedBatches) != 1 {
					t.Fatalf("marker replay failed: %v", err)
				}
				replayed := o.preparedBatches[0]
				if replayed.attempted == refused || (len(replayed.possible) == 0) != refused {
					t.Fatalf("legacy marker meaning changed: attempted=%v possible=%d", replayed.attempted, len(replayed.possible))
				}
				if _, err := o.beginAlertAttempt(replayed, []inboundJournalRecord{r}); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, coverageJournalSnapshot(t, j)) {
					t.Fatal("migration wrote an in-journal attempt marker")
				}
				state := ingestionReadAttempt(t, j, plan.ID)
				if (len(state.Prior) == 0) != refused || len(state.Coverage) != 1 {
					t.Fatal("migration lost exact/ambiguous coverage")
				}
				if version == 4 && refused && (state.Refusals != marker.Refusals || state.RetryAt != marker.RetryAt) {
					t.Fatal("migration lost refusal backoff")
				}
			})
		}
	}
}
