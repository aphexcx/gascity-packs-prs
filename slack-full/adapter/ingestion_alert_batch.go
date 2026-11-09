package main

import (
	"encoding/json"
	"sort"
	"time"
)

// Persist the original receipt set before mail. Its identity must survive a
// crash, new arrivals, and individual members gaining proof or completion.
type ingestionAlertBatch struct {
	ID      string    `json:"id"`
	Channel string    `json:"channel"`
	Members []string  `json:"members"`
	Created time.Time `json:"created"`
	Sent    bool      `json:"sent,omitempty"`
}

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
	members := ingestionAlertMembers(unseen)
	batch := ingestionAlertBatch{ID: ingestionAlertID(members), Channel: channel, Members: members, Created: o.now()}
	if err := o.cfg.inboundJournal.append(inboundJournalRecord{Phase: "ingestion_batch", AlertBatch: &batch}); err != nil {
		return ingestionAlertBatch{}, nil, err
	}
	return batch, unseen, nil
}

func (o *ingestionObserver) finishAlertBatch(batch ingestionAlertBatch, rows []inboundJournalRecord, sent bool) error {
	// One fsynced record establishes completion for every mailed receipt
	// before the compatibility per-member rows are written individually.
	batch.Members = ingestionAlertMembers(rows)
	batch.Sent = sent
	return o.cfg.inboundJournal.append(inboundJournalRecord{Phase: "ingestion_batch_done", AlertBatch: &batch})
}
