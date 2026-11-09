package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIngestionBatchRestartRetainsAcceptedMailIdentity(t *testing.T) {
	at := time.Now().UTC()
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	a, b := ingestionTestRecord("100.1", at, "a"), ingestionTestRecord("100.2", at, "b")
	for _, r := range []inboundJournalRecord{a, b} {
		if err := j.append(r); err != nil {
			t.Fatal(err)
		}
	}
	now = at.Add(ingestionWindow + time.Second)
	accepted := map[string]bool{}
	var firstID string
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(_ context.Context, rows []inboundJournalRecord) error {
			firstID = rows[0].AlertBatchID
			if firstID == "" || len(rows) != 2 {
				t.Fatal("original batch identity missing")
			}
			accepted[firstID] = true
			panic("crash after mail acceptance")
		}}
	func() {
		defer func() {
			if recover() != "crash after mail acceptance" {
				t.Fatal("expected crash")
			}
		}()
		_ = o.step(context.Background())
	}()
	// Reconstruct the partial-member shape reported by review, then allow a
	// new receipt to become eligible before recovery. Neither may change the
	// already-accepted mailbox dedup identity for B.
	a.Status, a.Phase, a.Alerted = "delivered_unseen", "ingestion_alert", true
	if err := j.append(a); err != nil {
		t.Fatal(err)
	}
	c := ingestionTestRecord("100.3", now, "c")
	if err := j.append(c); err != nil {
		t.Fatal(err)
	}
	now = now.Add(ingestionWindow + time.Second)
	o.cfg.inboundJournal = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
	calls := 0
	o.notify = func(_ context.Context, rows []inboundJournalRecord) error {
		calls++
		id := rows[0].AlertBatchID
		if calls == 1 && (len(rows) != 1 || rows[0].MessageTS != b.MessageTS || id != firstID) {
			t.Fatalf("recovery batch: %+v", rows)
		}
		if calls == 2 && (len(rows) != 1 || rows[0].MessageTS != c.MessageTS || id == firstID) {
			t.Fatalf("new batch: %+v", rows)
		}
		accepted[id] = true // model gc's durable mailbox dedup
		return nil
	}
	for i := 0; i < 3; i++ {
		if err := o.step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || len(accepted) != 2 {
		t.Fatalf("calls=%d accepted mails=%d", calls, len(accepted))
	}
}

func TestIngestionBatchBoundsLargeBacklogBeforePersisting(t *testing.T) {
	now := time.Now().UTC()
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now }}
	var rows []inboundJournalRecord
	for i := 0; i < 8500; i++ {
		r := ingestionTestRecord(fmt.Sprintf("1794198796.%06d", i), now.Add(-ingestionWindow), "body")
		rows = append(rows, r)
	}
	_, selected, err := o.prepareAlertBatch("C1", rows)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.pending(); err != nil {
		t.Fatalf("batch poisoned journal reader: %v", err)
	}
	if len(selected) == 0 || len(selected) >= len(rows) {
		t.Fatalf("unbounded backlog selection: %d", len(selected))
	}
}

func TestIngestionBatchRetryPayloadCannotGrowWithFanout(t *testing.T) {
	at := time.Now().UTC()
	now := at.Add(ingestionWindow + time.Second)
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	var records []inboundJournalRecord
	for i := 0; i < 300; i++ {
		r := ingestionTestRecord(fmt.Sprintf("1794198796.%06d", i), at, "body")
		r.Status, r.Phase = "delivered_unseen", "ingestion"
		records = append(records, r)
	}
	writeRows := func(rows []inboundJournalRecord) {
		t.Helper()
		var data []byte
		for _, r := range rows {
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			data = append(append(data, b...), '\n')
		}
		f, err := os.OpenFile(filepath.Join(j.dir, now.Format("20060102")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Write(data)
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("fixture write: %v %v", err, closeErr)
		}
	}
	writeRows(records)
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\000' \"$@\" >> \"$INGESTION_TEST_ARGS\"\nexit \"$INGESTION_TEST_EXIT\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("INGESTION_TEST_ARGS", argsPath)
	t.Setenv("INGESTION_TEST_EXIT", "17")
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil }}
	o.notify = o.mailUnseen
	if err := o.step(context.Background()); err == nil {
		t.Fatal("expected failed first mail")
	}
	var fanout []inboundJournalRecord
	for i := 0; i < 1000; i++ {
		r := records[0]
		r.TargetSessions = []string{fmt.Sprintf("late-%04d-%s", i, strings.Repeat("x", 54))}
		fanout = append(fanout, r)
	}
	writeRows(fanout)
	o.cfg.inboundJournal = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
	t.Setenv("INGESTION_TEST_EXIT", "0")
	now = now.Add(5 * time.Second) // Round-six ruling: first eligible refusal retry.
	if err := o.step(context.Background()); err != nil {
		t.Fatalf("fanout permanently blocked retry: %v", err)
	}
	b, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	if len(args) != 22 || args[6] != args[17] {
		t.Fatalf("retry changed dedup identity: %d argv entries", len(args))
	}
	if len(args[10]) > ingestionAlertBatchBudget || len(args[21]) > ingestionAlertBatchBudget {
		t.Fatal("mail exceeded budget")
	}
}

func TestIngestionBatchLegacyOversizeStillRecovers(t *testing.T) {
	now := time.Now().UTC()
	var rows []inboundJournalRecord
	for i := 0; i < 1500; i++ {
		r := ingestionTestRecord(fmt.Sprintf("1794198796.%06d", i), now.Add(-ingestionWindow), "body")
		r.Status, r.Phase = "delivered_unseen", "ingestion"
		rows = append(rows, r)
	}
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	members := ingestionAlertMembers(rows)
	legacy := ingestionAlertBatch{ID: ingestionAlertID(members), Channel: "C1", Members: members, Created: now}
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now }, preparedBatches: []ingestionAlertBatch{legacy}}
	plan, selected, err := o.prepareAlertBatch("C1", rows)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID != legacy.ID || len(selected) != len(rows) {
		t.Fatal("legacy identity/membership changed")
	}
	for i := range selected {
		selected[i].AlertBatchID = plan.ID
	}
	subject, body := ingestionAlertMessage(selected)
	if len(body) > ingestionAlertBatchBudget || !strings.Contains(subject, "1500 lines") || !strings.Contains(body, legacy.ID) {
		t.Fatalf("legacy retry must fit and identify full durable batch: subject=%s bytes=%d", subject, len(body))
	}
	if err := o.finishAlertBatch(plan, selected, true); err != nil {
		t.Fatal(err)
	}
	for _, row := range ingestionJournalRows(t, j) {
		b, _ := json.Marshal(row)
		if len(b) >= 1<<20 {
			t.Fatal("legacy completion exceeded reader ceiling")
		}
	}
}

func TestIngestionBatchCompletionRetainsSourceUntilMemberRows(t *testing.T) {
	at := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)
	now := at
	j := &inboundJournal{dir: t.TempDir(), retentionDays: 1, now: func() time.Time { return now }}
	for _, ts := range []string{"100.1", "100.2"} {
		if err := j.append(ingestionTestRecord(ts, at, "body")); err != nil {
			t.Fatal(err)
		}
	}
	now = at.Add(ingestionWindow + time.Second)
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch:  func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(context.Context, []inboundJournalRecord) error { return fmt.Errorf("mail unavailable") }}
	if err := o.step(context.Background()); err == nil {
		t.Fatal("expected failed batch")
	}
	now = at.Add(4*time.Hour + time.Second) // next day, beyond the late horizon
	o.notify = func(context.Context, []inboundJournalRecord) error { return nil }
	appends := 0
	j.now = func() time.Time {
		appends++
		if appends == 2 {
			panic("crash before first member row")
		}
		return now
	}
	func() {
		defer func() {
			if recover() != "crash before first member row" {
				t.Fatal("expected completion crash")
			}
		}()
		_ = o.step(context.Background())
	}()
	j = &inboundJournal{dir: j.dir, retentionDays: 1, now: func() time.Time { return now }}
	o.cfg.inboundJournal = j
	o.notify = func(context.Context, []inboundJournalRecord) error { t.Fatal("accepted mail repeated"); return nil }
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	members := map[string]bool{}
	for _, row := range ingestionJournalRows(t, j) {
		if row.Phase == "ingestion_alert" && row.Alerted {
			members[row.MessageTS] = true
		}
	}
	if len(members) != 2 {
		t.Fatalf("retention erased recoverable member rows: %v", members)
	}
}

func TestIngestionBatchAtomicCompletionRepairsIndividualRows(t *testing.T) {
	for _, keepFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_members", true: "after_first_member"}[keepFirst], func(t *testing.T) {
			at := time.Now().UTC()
			now := at
			j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
			for _, ts := range []string{"100.1", "100.2"} {
				if err := j.append(ingestionTestRecord(ts, at, "body")); err != nil {
					t.Fatal(err)
				}
			}
			now = at.Add(ingestionWindow + time.Second)
			o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
				fetch:  func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
				notify: func(context.Context, []inboundJournalRecord) error { return nil }}
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			var data []byte
			for _, row := range ingestionJournalRows(t, j) {
				b, _ := json.Marshal(row)
				data = append(append(data, b...), '\n')
				if (!keepFirst && row.Phase == "ingestion_batch_done") || (keepFirst && row.Phase == "ingestion_alert") {
					break
				}
			}
			if err := os.WriteFile(filepath.Join(j.dir, now.Format("20060102")+".jsonl"), data, 0600); err != nil {
				t.Fatal(err)
			}
			now = now.Add(ingestionLateWindow + time.Second)
			o.cfg.inboundJournal = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
			o.notify = func(context.Context, []inboundJournalRecord) error {
				t.Fatal("accepted batch mailed again")
				return nil
			}
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			members := map[string]bool{}
			for _, row := range ingestionJournalRows(t, j) {
				if row.Phase == "ingestion_alert" && row.Alerted {
					members[row.MessageTS] = true
				}
			}
			if len(members) != 2 {
				t.Fatalf("individual completion not recovered: %v", members)
			}
		})
	}
}
