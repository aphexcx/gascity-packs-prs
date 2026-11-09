package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func appendCoverageFixture(t *testing.T, j *inboundJournal, at time.Time, rows ...inboundJournalRecord) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(j.dir, at.Format("20060102")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, r := range rows {
		r.TS = at
		if err := json.NewEncoder(f).Encode(r); err != nil {
			t.Fatal(err)
		}
	}
}

func coverageJournalSnapshot(t *testing.T, j *inboundJournal) []byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(j.dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var result []byte
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, filepath.Base(path)...)
		result = append(result, b...)
	}
	return result
}

func pruneCoverageFixture(t *testing.T, j *inboundJournal, now time.Time) {
	t.Helper()
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.pruneLocked(now, filepath.Join(j.dir, now.Format("20060102")+".jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestIngestionCoveragePruneProtectedSegmentDoesNotGrow(t *testing.T) {
	for _, pairs := range []int{2, 300} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("pairs_%d/restart_%v", pairs, restart), func(t *testing.T) {
				at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
				now := at.Add(48 * time.Hour)
				j := &inboundJournal{dir: t.TempDir(), retentionDays: 1}
				r := ingestionTestRecord("200.1", at, "protected coverage")
				r.Status, r.Phase = "ingested", "ingestion"
				r.Windows = nil
				var coverage []ingestionAlertCoverage
				for i := 0; i < pairs; i++ {
					ts := fmt.Sprintf("200.%06d", i)
					r.Windows = append(r.Windows, injectionWindow{MessageTS: ts, ThreadTS: ts})
					coverage = append(coverage, ingestionAlertCoverage{Receipt: ingestionKey(r, ""), MessageTS: ts, ThreadTS: ts, Session: "s1", BatchID: "original"})
				}
				protected := ingestionTestRecord("300.1", at, "unsettled protects the old day")
				appendCoverageFixture(t, j, at, r, protected, inboundJournalRecord{Phase: "ingestion_coverage", AlertCoverage: coverage})
				late := r
				late.TargetSessions, late.Status, late.Phase = []string{"s2"}, "delivered_unseen", "ingestion_alert"
				late.Alerted, late.AlertVersion, late.DeliveredAt = true, ingestionAlertCoverageVersion, now.Add(-3*time.Hour)
				appendCoverageFixture(t, j, now, late)
				before := coverageJournalSnapshot(t, j)
				for i := 0; i < 25; i++ {
					pruneCoverageFixture(t, j, now)
					if after := coverageJournalSnapshot(t, j); !bytes.Equal(before, after) {
						t.Fatalf("protected prune %d grew or changed journal: before=%d after=%d (64 MiB guard must stay unreachable)", i, len(before), len(after))
					}
				}
				if restart {
					j = &inboundJournal{dir: j.dir, retentionDays: 1}
				}
				protected.Status, protected.Phase = "ingested", "ingestion"
				appendCoverageFixture(t, j, now, protected)
				pruneCoverageFixture(t, j, now)
				if _, err := os.Stat(filepath.Join(j.dir, at.Format("20060102")+".jsonl")); !os.IsNotExist(err) {
					t.Fatalf("settled protection did not release old segment: %v", err)
				}
				carried := 0
				for _, row := range ingestionJournalRows(t, j) {
					if row.Phase == "ingestion_coverage" {
						carried += len(row.AlertCoverage)
					}
				}
				if carried != pairs {
					t.Fatalf("shared pairs carried %d times, want exactly %d", carried, pairs)
				}
				if restart {
					j = &inboundJournal{dir: j.dir, retentionDays: 1}
				}
				before = coverageJournalSnapshot(t, j)
				for i := 0; i < 25; i++ {
					pruneCoverageFixture(t, j, now)
				}
				if !bytes.Equal(before, coverageJournalSnapshot(t, j)) {
					t.Fatal("post-deletion prune repeated the carry")
				}
				j.mu.Lock()
				state, _, _, err := j.ingestionStateLocked(now)
				j.mu.Unlock()
				if err != nil || len(state[ingestionKey(r, "s2")].AlertCoverage) != pairs {
					t.Fatalf("surviving target lost attribution: %v", err)
				}
			})
		}
	}
}

func TestIngestionCoveragePruneCopiesOnlyExpiringSources(t *testing.T) {
	for _, mode := range []string{"duplicate_old_batches", "surviving_pair", "surviving_plan", "surviving_completion", "surviving_target_row", "partial_deletion_retry"} {
		t.Run(mode, func(t *testing.T) {
			at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
			now := at.Add(48 * time.Hour)
			j := &inboundJournal{dir: t.TempDir(), retentionDays: 1}
			r := ingestionTestRecord("200.1", at, "shared coverage")
			r.Status, r.Phase = "ingested", "ingestion"
			r.Windows = []injectionWindow{{MessageTS: "100.1", ThreadTS: "100.0"}, {MessageTS: "200.1", ThreadTS: "200.0"}}
			coverage := ingestionCoverageFor([]inboundJournalRecord{r})
			for i := range coverage {
				coverage[i].BatchID = "original"
			}
			appendCoverageFixture(t, j, at, r)
			batch := ingestionAlertBatch{ID: "first", Version: ingestionAlertCoverageVersion, Coverage: coverage}
			appendCoverageFixture(t, j, at, inboundJournalRecord{Phase: "ingestion_batch", AlertBatch: &batch})
			done := ingestionAlertBatch{ID: batch.ID, Version: ingestionAlertCoverageVersion, All: true, Sent: true}
			appendCoverageFixture(t, j, at, inboundJournalRecord{Phase: "ingestion_batch_done", AlertBatch: &done})
			batch.ID, done.ID = "second", "second"
			appendCoverageFixture(t, j, at.Add(24*time.Hour), inboundJournalRecord{Phase: "ingestion_batch", AlertBatch: &batch}, inboundJournalRecord{Phase: "ingestion_batch_done", AlertBatch: &done})
			late := r
			late.TargetSessions = []string{"s2"}
			appendCoverageFixture(t, j, now, late)
			wantCarry := 2
			if mode == "surviving_pair" {
				appendCoverageFixture(t, j, now, inboundJournalRecord{Phase: "ingestion_coverage", AlertCoverage: coverage[:1]})
				wantCarry = 1
			}
			if mode == "surviving_target_row" {
				own := r
				own.AlertCoverage = coverage[:1]
				appendCoverageFixture(t, j, now, own)
				wantCarry = 1
			}
			if mode == "surviving_plan" || mode == "surviving_completion" {
				batch.ID, batch.Coverage, done.ID = "third", coverage[:1], "third"
				planAt := now
				if mode == "surviving_completion" {
					planAt = at.Add(24 * time.Hour)
				} else {
					wantCarry = 1
				}
				appendCoverageFixture(t, j, planAt, inboundJournalRecord{Phase: "ingestion_batch", AlertBatch: &batch})
				appendCoverageFixture(t, j, now, inboundJournalRecord{Phase: "ingestion_batch_done", AlertBatch: &done})
			}
			oldPath := filepath.Join(j.dir, at.Format("20060102")+".jsonl")
			oldBytes, err := os.ReadFile(oldPath)
			if err != nil {
				t.Fatal(err)
			}
			before := len(ingestionJournalRows(t, j))
			pruneCoverageFixture(t, j, now)
			carried := 0
			for _, row := range ingestionJournalRows(t, j) {
				if row.Phase == "ingestion_coverage" {
					carried += len(row.AlertCoverage)
				}
			}
			if mode == "surviving_pair" {
				carried-- // This pair was already present, not written by prune.
			}
			if carried != wantCarry {
				t.Fatalf("copied surviving or duplicate coverage: got=%d want=%d initial rows=%d", carried, wantCarry, before)
			}
			j = &inboundJournal{dir: j.dir, retentionDays: 1}
			j.mu.Lock()
			state, _, _, err := j.ingestionStateLocked(now)
			j.mu.Unlock()
			if err != nil || len(state[ingestionKey(r, "s2")].AlertCoverage) != 2 {
				t.Fatalf("restart lost attribution: %v", err)
			}
			saved := coverageJournalSnapshot(t, j)
			if mode == "partial_deletion_retry" {
				// Model restart after the carry was fsynced but before all
				// source files were removed. The surviving carry is sufficient.
				if err := os.WriteFile(oldPath, oldBytes, 0600); err != nil {
					t.Fatal(err)
				}
				j = &inboundJournal{dir: j.dir, retentionDays: 1}
			}
			pruneCoverageFixture(t, j, now)
			if !bytes.Equal(saved, coverageJournalSnapshot(t, j)) {
				t.Fatal("second prune repeated coverage")
			}
		})
	}
}
