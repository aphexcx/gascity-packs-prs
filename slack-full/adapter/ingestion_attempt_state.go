package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ingestionAlertSidecarVersion = 5

// Legacy receipt-only plans can name thousands of pairs. Keep their state
// outside the journal's 1 MiB line limit, with a separate hard read/write cap.
// Modern plans are limited to 64 KiB; this allows older plans to migrate too.
const ingestionAttemptMaxBytes = 8 << 20

type ingestionAttemptState struct {
	ID          string                   `json:"id"`
	Version     int                      `json:"version"`
	PlanVersion int                      `json:"plan_version"`
	Coverage    []ingestionAlertCoverage `json:"coverage"`            // last attempt, exactly
	Prior       []ingestionAlertCoverage `json:"prior,omitempty"`     // union, not history
	PriorAll    bool                     `json:"prior_all,omitempty"` // v0 uncertainty only
	AttemptedAt time.Time                `json:"attempted_at"`
	Refused     bool                     `json:"refused"`
	Refusals    int                      `json:"refusals"`
	RetryAt     int64                    `json:"retry_at"`
}

func (o *ingestionObserver) beginAlertAttempt(plan ingestionAlertBatch, rows []inboundJournalRecord) (ingestionAttemptState, error) {
	state := ingestionAttemptState{ID: plan.ID, Version: ingestionAlertSidecarVersion, PlanVersion: plan.Version,
		Coverage: ingestionCoverageFor(rows), Prior: mergeIngestionCoverage(plan.possible), AttemptedAt: o.now(),
		PriorAll: plan.Version < ingestionAlertCoverageVersion && plan.recovered}
	if plan.attempt != nil {
		state.Refusals, state.RetryAt = plan.attempt.Refusals, plan.attempt.RetryAt
	}
	// Each eligible invocation gets its own timestamp and exact pair list.
	// The previous unresolved attempt is already in Prior, so refusing this
	// invocation cannot erase an earlier ambiguous acceptance of the same set.
	return state, o.cfg.inboundJournal.writeAlertAttempt(state)
}

func (j *inboundJournal) alertAttemptPath(id string) string {
	// Hash even legacy IDs: journal content must never select a filesystem path.
	return filepath.Join(j.dir, "attempts", ingestionDigest(id)+".json")
}

func syncIngestionDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// The rename is the pre-notify fence. No journal segment is read or changed.
// A crash before it leaves the previous state; after it the last attempt is
// conservatively ambiguous until a refusal or completion is durable.
func (j *inboundJournal) writeAlertAttempt(state ingestionAttemptState) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, exists := j.preparedBatches[state.ID]; !exists {
		return fmt.Errorf("ingestion attempt has no replayed or newly appended pending plan")
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(encoded) > ingestionAttemptMaxBytes {
		return fmt.Errorf("ingestion attempt exceeds %d-byte sidecar budget", ingestionAttemptMaxBytes)
	}
	hook := func(stage string) error {
		if j.attemptWriteHook != nil {
			return j.attemptWriteHook(stage)
		}
		return nil
	}
	if err := hook("before_create"); err != nil {
		return err
	}
	path := j.alertAttemptPath(state.ID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := hook("after_mkdir"); err != nil {
		return err
	}
	// Persist the directory entry too, including on the first-ever attempt.
	if err := syncIngestionDirectory(j.dir); err != nil {
		return err
	}
	if err := hook("after_parent_sync"); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".attempt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(encoded); err != nil {
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
	if err := syncIngestionDirectory(dir); err != nil {
		return err
	}
	return hook("after_dir_sync")
}

// Caller holds the journal lock and has successfully replayed all segments.
// Never load unknown sidecars: a completion is authoritative even if a crash
// prevented unlinking its state. Temporary files are never recovery inputs.
func (j *inboundJournal) recoverAlertAttemptsLocked(remaining *int64) error {
	for id, plan := range j.preparedBatches {
		f, err := os.Open(j.alertAttemptPath(id))
		if os.IsNotExist(err) {
			continue // new plan = unattempted; legacy journal state stays intact
		}
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(f, min(int64(ingestionAttemptMaxBytes), *remaining)+1))
		closeErr := f.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		*remaining -= int64(len(data))
		if len(data) > ingestionAttemptMaxBytes || *remaining < 0 {
			return fmt.Errorf("ingestion attempt exceeds sidecar or 64 MiB observation budget")
		}
		var state ingestionAttemptState
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("invalid ingestion sidecar %s: %w", id, err)
		}
		if state.ID != id || state.PlanVersion != plan.Version || state.Version != ingestionAlertSidecarVersion || state.AttemptedAt.IsZero() || state.Refusals < 0 || state.Refusals > 7 {
			return fmt.Errorf("invalid ingestion sidecar state for %s", id)
		}
		plan.attempt, plan.refused = &state, state.Refused
		plan.possible = state.Prior
		if !state.Refused {
			plan.possible = mergeIngestionCoverage(plan.possible, state.Coverage)
		}
		plan.attempted = state.PriorAll || len(plan.possible) > 0
		j.preparedBatches[id] = plan
	}
	return j.pruneAlertAttemptFilesLocked(false)
}

// With tempsOnly, no journal reconstruction is needed: temporary files are
// never authoritative and no writer can be active while the caller holds mu.
func (j *inboundJournal) pruneAlertAttemptFilesLocked(tempsOnly bool) error {
	dir := filepath.Join(j.dir, "attempts")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	keep := make(map[string]bool, len(j.preparedBatches))
	for id := range j.preparedBatches {
		keep[filepath.Base(j.alertAttemptPath(id))] = true
	}
	removed := false
	for _, entry := range entries {
		name := entry.Name()
		temporary := strings.HasPrefix(name, ".attempt-")
		if entry.IsDir() || keep[name] || (tempsOnly && !temporary) || (!strings.HasSuffix(name, ".json") && !temporary) {
			continue
		}
		if !removed && !tempsOnly {
			// Replay may run inside completion's append, before its caller
			// syncs a newly rotated journal directory entry. Make the replayed
			// completion durable before deleting the orphan's retry evidence.
			if err := syncIngestionDirectory(j.dir); err != nil {
				return err
			}
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
		removed = true
	}
	if removed {
		return syncIngestionDirectory(dir)
	}
	return nil
}

func (j *inboundJournal) removeAlertAttempt(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	// append fsyncs the completion's file. Persist its directory entry too
	// before dropping retry evidence, including a new day or rotated segment.
	if j.attemptWriteHook != nil {
		if err := j.attemptWriteHook("before_completion_sync"); err != nil {
			return err
		}
	}
	if err := syncIngestionDirectory(j.dir); err != nil {
		return err
	}
	if j.attemptWriteHook != nil {
		if err := j.attemptWriteHook("before_remove"); err != nil {
			return err
		}
	}
	path := j.alertAttemptPath(id)
	if err := os.Remove(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if j.attemptWriteHook != nil {
		if err := j.attemptWriteHook("after_remove"); err != nil {
			return err
		}
	}
	if err := syncIngestionDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if j.attemptWriteHook != nil {
		return j.attemptWriteHook("after_remove_dir_sync")
	}
	return nil
}
