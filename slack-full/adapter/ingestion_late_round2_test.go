package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestIngestionLongToolReturnSupersedesUnseenWithinBound(t *testing.T) {
	for _, test := range []struct {
		name         string
		toolDuration time.Duration
		creationTime bool
	}{
		{"90m_creation_timestamp", 90 * time.Minute, true},
		{"90m_return_timestamp", 90 * time.Minute, false},
		{"expired_creation_timestamp", 2*time.Hour + time.Second, true},
		{"expired_return_timestamp", 2*time.Hour + time.Second, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			at := time.Date(2026, 10, 30, 23, 59, 0, 0, time.UTC)
			now := at
			j := &inboundJournal{dir: t.TempDir(), retentionDays: 1, now: func() time.Time { return now }}
			r := ingestionTestRecord("100.1", at, "long tool body")
			if err := j.append(r); err != nil {
				t.Fatal(err)
			}
			var transcript []json.RawMessage
			alerts := 0
			o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
				fetch:  func(context.Context, string) ([]json.RawMessage, error) { return transcript, nil },
				notify: func(context.Context, inboundJournalRecord) error { alerts++; return nil }}
			now = at.Add(ingestionWindow + time.Second)
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if alerts != 1 {
				t.Fatalf("three-minute alert changed: alerts=%d", alerts)
			}
			pending, err := o.pending()
			if err != nil || len(pending) != 1 {
				t.Fatalf("alert prematurely ended late observation: pending=%v err=%v", pending, err)
			}
			// Simulate a restart and a queued attachment first rendered when
			// the mayor's 90-minute tool call returns, with that timestamp.
			o.cfg.inboundJournal = &inboundJournal{dir: j.dir, retentionDays: 1, now: func() time.Time { return now }}
			now = at.Add(test.toolDuration)
			reference := r
			if !test.creationTime {
				reference.DeliveredAt = now.Add(-time.Second)
			}
			transcript = []json.RawMessage{ingestionTestEntry(t, reference, "long tool body", "attachment")}
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if alerts != 1 {
				t.Fatalf("late polling sent a duplicate alert: %d", alerts)
			}
			var ingested bool
			for _, row := range ingestionJournalRows(t, j) {
				if row.Status != "ingested" {
					continue
				}
				ingested = true
				b, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err := json.Unmarshal(b, &fields); err != nil || fields["late"] != true {
					t.Fatalf("late ingestion lacks explicit marker: %s, %v", b, err)
				}
			}
			if ingested != (test.toolDuration < 2*time.Hour) {
				t.Fatalf("late ingestion=%v after tool call %s", ingested, test.toolDuration)
			}
			pending, err = o.pending()
			if err != nil || len(pending) != 0 {
				t.Fatalf("late observation did not settle or expire: pending=%v err=%v", pending, err)
			}
		})
	}
}
