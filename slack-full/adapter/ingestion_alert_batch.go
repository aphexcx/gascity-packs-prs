package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Persist the original message/target set before mail. Its identity survives a
// crash, new arrivals, and individual members gaining proof or completion.
type ingestionAlertBatch struct {
	ID       string                   `json:"id"`
	Channel  string                   `json:"channel"`
	Members  []string                 `json:"members"`
	Created  time.Time                `json:"created"`
	Sent     bool                     `json:"sent,omitempty"`
	All      bool                     `json:"all,omitempty"` // completion references the prepared membership
	Version  int                      `json:"version,omitempty"`
	Coverage []ingestionAlertCoverage `json:"coverage,omitempty"`
	// A recovered plan may already have reached gc even when no retry is
	// needed. Keep that uncertainty separate from this pass's Sent result.
	Unconfirmed bool `json:"unconfirmed,omitempty"`
	// Read-only v4 migration fields. New attempt state lives in a sidecar.
	Prior     []int `json:"prior,omitempty"`
	PriorAll  bool  `json:"prior_all,omitempty"` // legacy plans without pair coverage
	Refusals  int   `json:"refusals,omitempty"`
	RetryAt   int64 `json:"retry_at,omitempty"` // Unix nanoseconds; survives restart
	recovered bool
	attempted bool // replayed attempt without a definite refusal
	possible  []ingestionAlertCoverage
	attempt   *ingestionAttemptState
	refused   bool
}

// Older saved plans have no attempt markers, so their acceptance stays unknown.
const ingestionAlertAttemptVersion = 3

const ingestionAlertStateVersion = 4

// Leave ample room below the journal scanner's 1 MiB record ceiling and
// platform argv limits. Excess receipts stay eligible for subsequent passes.
const ingestionAlertBatchBudget = 64 << 10

func ingestionAlertMembers(rows []inboundJournalRecord) []string {
	unique := make(map[string]bool)
	for _, r := range rows {
		unique[ingestionKey(r, "")] = true
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func ingestionAlertID(members []string) string {
	encoded, _ := json.Marshal(members)
	return ingestionDigest(string(encoded))
}

func (o *ingestionObserver) prepareAlertBatch(channel string, unseen []inboundJournalRecord) (ingestionAlertBatch, []inboundJournalRecord, error) {
	for _, batch := range o.preparedBatches {
		if batch.Channel != channel {
			continue
		}
		batch.recovered = batch.Version < ingestionAlertAttemptVersion || batch.attempted
		members := make(map[string]bool, len(batch.Members))
		for _, member := range batch.Members {
			members[member] = true
		}
		var selected []inboundJournalRecord
		for _, r := range unseen {
			if members[ingestionKey(r, "")] {
				selected = append(selected, r)
			}
		}
		if batch.Version >= ingestionAlertCoverageVersion {
			selected = selectIngestionCoverage(selected, batch.Coverage)
		}
		if len(selected) > 0 {
			if batch.attempt != nil && o.now().UnixNano() < batch.attempt.RetryAt {
				return batch, nil, nil
			}
			// Do not fold new arrivals into an ambiguously accepted mail: gc
			// could deduplicate it and never deliver those new lines. Recover
			// the original batch first; newcomers wait for the next pass.
			return batch, selected, nil
		}
		if err := o.finishAlertBatch(batch, nil, false); err != nil {
			return ingestionAlertBatch{}, nil, err
		}
		// Reconstruct the newly completed coverage before selecting another
		// batch. A late target in unseen may share these already named lines.
		return ingestionAlertBatch{}, nil, nil
	}
	if len(unseen) == 0 {
		return ingestionAlertBatch{}, nil, nil
	}
	var chosen []inboundJournalRecord
	var batch ingestionAlertBatch
	planFor := func(candidate []inboundJournalRecord) (ingestionAlertBatch, bool, error) {
		plan := ingestionAlertBatch{Channel: channel, Members: ingestionAlertMembers(candidate), Created: o.now(),
			Version: ingestionAlertSidecarVersion, Coverage: ingestionCoverageFor(candidate)}
		plan.ID = ingestionCoverageID(plan.Coverage)
		encoded, err := json.Marshal(inboundJournalRecord{Phase: "ingestion_batch", TS: o.now(), AlertBatch: &plan})
		_, body := ingestionAlertMessageWithBudget(candidate, 0)
		// Reserve space for the completion flag and timestamp formatting.
		return plan, len(encoded)+256 <= ingestionAlertBatchBudget && len(body) <= ingestionAlertBatchBudget, err
	}
	for _, r := range ingestionAlertRows(unseen) {
		candidate := append(chosen, r)
		plan, fits, err := planFor(candidate)
		if err != nil {
			return ingestionAlertBatch{}, nil, err
		}
		if !fits {
			if len(chosen) == 0 {
				// Even one receipt can exceed the budget through fanout. Save
				// a bounded prefix of its message/target pairs instead. The
				// channel-wide once rule covers named messages for later targets;
				// other messages remain eligible under a new identity next pass.
				coverage := ingestionCoverageFor([]inboundJournalRecord{r})
				low, high := 1, len(coverage)
				for low <= high {
					n := low + (high-low)/2
					part := selectIngestionCoverage([]inboundJournalRecord{r}, coverage[:n])
					partial, fits, err := planFor(part)
					if err != nil {
						return ingestionAlertBatch{}, nil, err
					}
					if fits {
						chosen, batch, low = part, partial, n+1
					} else {
						high = n - 1
					}
				}
				if len(chosen) == 0 {
					return ingestionAlertBatch{}, nil, fmt.Errorf("single ingestion alert member exceeds %d-byte batch budget", ingestionAlertBatchBudget)
				}
			}
			break
		}
		chosen, batch = candidate, plan
	}
	selected := selectIngestionCoverage(unseen, batch.Coverage)
	if err := o.cfg.inboundJournal.append(inboundJournalRecord{Phase: "ingestion_batch", AlertBatch: &batch}); err != nil {
		return ingestionAlertBatch{}, nil, err
	}
	return batch, selected, nil
}

func (o *ingestionObserver) finishAlertBatch(batch ingestionAlertBatch, rows []inboundJournalRecord, sent bool) error {
	// One fsynced record establishes coverage for every mailed message
	// before the compatibility per-member rows are written individually.
	members := ingestionAlertMembers(rows)
	confirmed := ingestionCoverageFor(rows)
	coverage := mergeIngestionCoverage(batch.possible, confirmed)
	full := len(members) == len(batch.Members) && ingestionAlertID(members) == ingestionAlertID(batch.Members)
	if batch.Version >= ingestionAlertCoverageVersion {
		full = ingestionCoverageID(coverage) == ingestionCoverageID(batch.Coverage)
	}
	if (batch.Version < ingestionAlertCoverageVersion && batch.recovered) || full {
		// A legacy plan can be close to the reader ceiling. Refer to its
		// durable membership rather than repeat it with extra completion fields.
		batch.All, batch.Members = true, nil
		batch.Coverage = nil
	} else {
		batch.Members = members
		if batch.Version >= ingestionAlertCoverageVersion {
			batch.Coverage = coverage
		}
	}
	batch.Sent = sent
	batch.Unconfirmed = len(batch.possible) > 0 && (!sent || ingestionCoverageID(coverage) != ingestionCoverageID(confirmed))
	if batch.Version < ingestionAlertCoverageVersion && batch.recovered {
		batch.Unconfirmed = !sent || !full
	}
	if err := o.cfg.inboundJournal.append(inboundJournalRecord{Phase: "ingestion_batch_done", AlertBatch: &batch}); err != nil {
		return err
	}
	return o.cfg.inboundJournal.removeAlertAttempt(batch.ID)
}
