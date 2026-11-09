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
}

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
			// Do not fold new arrivals into an ambiguously accepted mail: gc
			// could deduplicate it and never deliver those new lines. Recover
			// the original batch first; newcomers wait for the next pass.
			return batch, selected, nil
		}
		if err := o.finishAlertBatch(batch, nil, false); err != nil {
			return ingestionAlertBatch{}, nil, err
		}
	}
	if len(unseen) == 0 {
		return ingestionAlertBatch{}, nil, nil
	}
	var chosen []inboundJournalRecord
	var batch ingestionAlertBatch
	for _, r := range ingestionAlertRows(unseen) {
		candidate := append(chosen, r)
		members := ingestionAlertMembers(candidate)
		plan := ingestionAlertBatch{ID: ingestionAlertID(members), Channel: channel, Members: members, Created: o.now()}
		plan.Version, plan.Coverage = ingestionAlertCoverageVersion, ingestionCoverageFor(candidate)
		plan.ID = ingestionCoverageID(plan.Coverage)
		encoded, err := json.Marshal(inboundJournalRecord{Phase: "ingestion_batch", TS: o.now(), AlertBatch: &plan})
		if err != nil {
			return ingestionAlertBatch{}, nil, err
		}
		_, body := ingestionAlertMessageWithBudget(candidate, 0)
		// Reserve space for the completion flag and timestamp formatting.
		if len(encoded)+256 > ingestionAlertBatchBudget || len(body) > ingestionAlertBatchBudget {
			if len(chosen) == 0 {
				return ingestionAlertBatch{}, nil, fmt.Errorf("single ingestion alert exceeds %d-byte batch budget", ingestionAlertBatchBudget)
			}
			break
		}
		chosen, batch = candidate, plan
	}
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
	if err := o.cfg.inboundJournal.append(inboundJournalRecord{Phase: "ingestion_batch", AlertBatch: &batch}); err != nil {
		return ingestionAlertBatch{}, nil, err
	}
	return batch, selected, nil
}

func (o *ingestionObserver) finishAlertBatch(batch ingestionAlertBatch, rows []inboundJournalRecord, sent bool) error {
	// One fsynced record establishes coverage for every mailed message
	// before the compatibility per-member rows are written individually.
	members := ingestionAlertMembers(rows)
	coverage := ingestionCoverageFor(rows)
	same := len(members) == len(batch.Members) && ingestionAlertID(members) == ingestionAlertID(batch.Members)
	if batch.Version >= ingestionAlertCoverageVersion {
		same = ingestionCoverageID(coverage) == ingestionCoverageID(batch.Coverage)
	}
	if same {
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
	return o.cfg.inboundJournal.append(inboundJournalRecord{Phase: "ingestion_batch_done", AlertBatch: &batch})
}
