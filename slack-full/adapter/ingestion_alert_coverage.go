package main

import (
	"encoding/json"
	"sort"
)

// A mail names a message once across fanout, but retains exactly which target
// lacked proof. Never infer coverage for the other messages in its receipt.
type ingestionAlertCoverage struct {
	Receipt   string `json:"receipt"`
	MessageTS string `json:"message_ts"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Session   string `json:"session"`
	BatchID   string `json:"batch_id,omitempty"`
}

const ingestionAlertCoverageVersion = 2

func ingestionCoverageKey(c ingestionAlertCoverage) string {
	b, _ := json.Marshal([]string{c.Receipt, c.MessageTS, c.ThreadTS, c.Session})
	return string(b)
}

func mergeIngestionCoverage(groups ...[]ingestionAlertCoverage) []ingestionAlertCoverage {
	unique := make(map[string]ingestionAlertCoverage)
	for _, group := range groups {
		for _, c := range group {
			key := ingestionCoverageKey(c)
			if _, exists := unique[key]; !exists {
				unique[key] = c
			}
		}
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var result []ingestionAlertCoverage
	for _, key := range keys {
		result = append(result, unique[key])
	}
	return result
}

func ingestionCoverageFor(rows []inboundJournalRecord) []ingestionAlertCoverage {
	var coverage []ingestionAlertCoverage
	for _, r := range rows {
		if r.AlertSelection != nil {
			coverage = append(coverage, r.AlertSelection...)
			continue
		}
		for _, w := range ingestionAlertWindows(r) {
			for _, session := range r.TargetSessions {
				coverage = append(coverage, ingestionAlertCoverage{Receipt: ingestionKey(r, ""),
					MessageTS: w.MessageTS, ThreadTS: w.ThreadTS, Session: session})
			}
		}
	}
	return mergeIngestionCoverage(coverage)
}

func ingestionCoverageID(coverage []ingestionAlertCoverage) string {
	keys := make([]string, 0, len(coverage))
	for _, c := range mergeIngestionCoverage(coverage) {
		keys = append(keys, ingestionCoverageKey(c))
	}
	return ingestionAlertID(keys)
}

// Reconstructed state shares channel-wide coverage for decisions. A durable
// target row carries only its own attribution; the batch records carry the set.
func ingestionTargetCoverage(r inboundJournalRecord) []ingestionAlertCoverage {
	targets := make(map[string]bool, len(r.TargetSessions))
	for _, session := range r.TargetSessions {
		targets[session] = true
	}
	var own []ingestionAlertCoverage
	for _, c := range r.AlertCoverage {
		if targets[c.Session] {
			own = append(own, c)
		}
	}
	return own
}

func ingestionMessageAlerted(r inboundJournalRecord, w injectionWindow) bool {
	for _, c := range r.AlertCoverage {
		if c.Receipt == ingestionKey(r, "") && c.MessageTS == w.MessageTS && c.ThreadTS == w.ThreadTS {
			return true
		}
	}
	return false
}

func ingestionAllMessagesAlerted(r inboundJournalRecord) bool {
	for _, w := range ingestionMessageWindows(r) {
		if !ingestionMessageAlerted(r, w) {
			return false
		}
	}
	return true
}

// Freeze retries to the original message/target set, not just its receipt IDs.
// New fanout members must not ride an already accepted mail's dedup identity.
func selectIngestionCoverage(rows []inboundJournalRecord, coverage []ingestionAlertCoverage) []inboundJournalRecord {
	allowed := make(map[string]bool, len(coverage))
	for _, c := range coverage {
		allowed[ingestionCoverageKey(c)] = true
	}
	var selected []inboundJournalRecord
	for _, r := range rows {
		var selection []ingestionAlertCoverage
		for _, c := range ingestionCoverageFor([]inboundJournalRecord{r}) {
			if allowed[ingestionCoverageKey(c)] {
				selection = append(selection, c)
			}
		}
		if len(selection) == 0 {
			continue
		}
		r.AlertSelection = selection
		r.UnseenWindows = nil
		sessions := make(map[string]bool)
		for _, c := range selection {
			r.UnseenWindows = append(r.UnseenWindows, injectionWindow{MessageTS: c.MessageTS, ThreadTS: c.ThreadTS})
			sessions[c.Session] = true
		}
		r.TargetSessions = nil
		for session := range sessions {
			r.TargetSessions = append(r.TargetSessions, session)
		}
		sort.Strings(r.TargetSessions)
		selected = append(selected, r)
	}
	return selected
}

func (o *ingestionObserver) recordAlertResult(r inboundJournalRecord, sent []ingestionAlertCoverage) error {
	for i := range sent {
		sent[i].BatchID = r.AlertBatchID
	}
	r.AlertVersion = ingestionAlertCoverageVersion
	r.AlertCoverage = mergeIngestionCoverage(r.AlertCoverage, sent)
	proof, missing, err := o.cfg.inboundJournal.sessionActProof(r)
	if err != nil {
		return err
	}
	if proof != nil {
		return o.markIngested(r, proof)
	}
	r.Alerted, r.Phase = true, "ingestion_alert"
	for _, w := range missing {
		if !ingestionMessageAlerted(r, w) {
			r.Alerted = false
			break
		}
	}
	return o.cfg.inboundJournal.append(r)
}
