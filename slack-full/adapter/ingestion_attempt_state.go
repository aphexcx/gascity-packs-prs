package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func (o *ingestionObserver) beginAlertAttempt(plan ingestionAlertBatch, rows []inboundJournalRecord) (ingestionAlertBatch, error) {
	state := ingestionAlertBatch{ID: plan.ID, Version: ingestionAlertStateVersion, Coverage: ingestionCoverageFor(rows)}
	possible := make(map[string]bool)
	for _, c := range plan.possible {
		possible[ingestionCoverageKey(c)] = true
	}
	for i, c := range plan.Coverage {
		if possible[ingestionCoverageKey(c)] {
			state.Prior = append(state.Prior, i)
		}
	}
	state.PriorAll = plan.Version < ingestionAlertCoverageVersion && plan.recovered
	if plan.attempt != nil {
		state.Refusals, state.RetryAt = plan.attempt.Refusals, plan.attempt.RetryAt
		if !plan.refused && ingestionCoverageID(plan.attempt.Coverage) == ingestionCoverageID(state.Coverage) {
			// This exact send is already fenced. A refusal still records Prior
			// from above, so it cannot invalidate the older ambiguous attempt.
			return state, nil
		}
	}
	return state, o.cfg.inboundJournal.writeAlertAttempt(plan, state, "ingestion_batch_attempt")
}

// Reserve the largest state this immutable plan can require, including all
// prior ambiguous pair references. Padding makes repeated refusal updates
// byte-neutral even when timestamps and the capped backoff counter change.
func ingestionAttemptSlotSize(plan ingestionAlertBatch) int {
	state := ingestionAlertBatch{ID: plan.ID, Version: ingestionAlertStateVersion,
		Coverage: plan.Coverage, PriorAll: true, Refusals: 7, RetryAt: 1<<63 - 1}
	for i := range plan.Coverage {
		state.Prior = append(state.Prior, i)
	}
	row := inboundJournalRecord{Phase: "ingestion_batch_attempt", AlertBatch: &state,
		TS: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)}
	b, _ := json.Marshal(row)
	return len(b) + 128
}

// Keep exactly one current attempt/refusal row per plan. Replace its slot in
// the owning segment atomically under the journal lock; do not append an
// outage history that retention cannot discard. A temporary file is never a
// replay input. Sync the replacement and its directory before calling gc.
func (j *inboundJournal) writeAlertAttempt(plan, state ingestionAlertBatch, phase string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now().UTC()
	if j.now != nil {
		now = j.now().UTC()
	}
	row := inboundJournalRecord{Phase: phase, TS: now, AlertBatch: &state}
	encoded, err := json.Marshal(row)
	if err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(j.dir, "*.jsonl"))
	if err != nil {
		return err
	}
	path := filepath.Join(j.dir, now.Format("20060102")+".jsonl")
	var content []byte
	start, end := -1, -1
	remaining := int64(64 << 20)
	foundPlan := false
	for _, candidate := range paths {
		f, err := os.Open(candidate)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(f, remaining+1))
		closeErr := f.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		remaining -= int64(len(data))
		if remaining < 0 {
			return fmt.Errorf("journal exceeds 64 MiB observation budget")
		}
		for offset := 0; offset < len(data); {
			next := bytes.IndexByte(data[offset:], '\n')
			if next < 0 {
				break
			} // Do not interpret an incomplete final record.
			next += offset + 1
			var saved inboundJournalRecord
			if json.Unmarshal(data[offset:next], &saved) == nil && saved.AlertBatch != nil && saved.AlertBatch.ID == plan.ID {
				switch saved.Phase {
				case "ingestion_batch":
					foundPlan, start, end = true, -1, -1
				case "ingestion_batch_done":
					foundPlan, start, end = false, -1, -1
				case "ingestion_batch_attempt", "ingestion_batch_refused":
					if foundPlan && saved.AlertBatch.Version >= ingestionAlertStateVersion {
						path, content, start, end = candidate, data, offset, next
					}
				}
			}
			offset = next
		}
	}
	if !foundPlan {
		return fmt.Errorf("ingestion attempt has no pending plan")
	}
	reservation := plan
	if plan.Version < ingestionAlertCoverageVersion {
		// Legacy plans stored receipt IDs only. Their first exact attempt
		// supplies the reservation; subsequent subsets reuse the same slot.
		reservation.Coverage = state.Coverage
	}
	slot := ingestionAttemptSlotSize(reservation)
	if start < 0 {
		path = filepath.Join(j.dir, now.Format("20060102")+".jsonl")
		content, err = os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if len(content) > 0 && content[len(content)-1] != '\n' {
			content = append(content, '\n')
		}
		start, end = len(content), len(content)
	} else {
		slot = max(slot, end-start)
	}
	if len(encoded)+1 > slot {
		return fmt.Errorf("ingestion attempt exceeds its reserved state slot")
	}
	var replacement []byte
	replacement = append(replacement, content[:start]...)
	replacement = append(replacement, encoded...)
	replacement = append(replacement, bytes.Repeat([]byte{' '}, slot-len(encoded)-1)...)
	replacement = append(replacement, '\n')
	replacement = append(replacement, content[end:]...)
	hook := func(stage string) error {
		if j.attemptWriteHook != nil {
			return j.attemptWriteHook(stage)
		}
		return nil
	}
	if err := hook("before_create"); err != nil {
		return err
	}
	f, err := os.CreateTemp(j.dir, ".ingestion-attempt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(replacement); err != nil {
		return err
	}
	if err := hook("after_write"); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := hook("after_sync"); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := hook("after_close"); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	if err := hook("after_rename"); err != nil {
		return err
	}
	dir, err := os.Open(j.dir)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return hook("after_dir_sync")
}
