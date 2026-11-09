package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestIngestionAcceptedRetryPreservesOriginalCoverage(t *testing.T) {
	for _, proven := range []string{"A", "B", "both"} {
		for _, mode := range []string{"live", "second_timeout", "restart", "restart_after_retry", "retention", "late_before_retry", "retention_new_target"} {
			t.Run(proven+"/"+mode, func(t *testing.T) {
				at := time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)
				now := at
				j := &inboundJournal{dir: t.TempDir(), retentionDays: 1, now: func() time.Time { return now }}
				r := ingestionTestRecord("200.1", at, "accepted retry")
				r.ThreadTS, r.CanonicalSessionID = "200.0", "s1"
				r.Windows = []injectionWindow{{MessageTS: "100.1", ThreadTS: "100.0"}, {MessageTS: "200.1", ThreadTS: "200.0"}}
				appendRow := func(row inboundJournalRecord) {
					t.Helper()
					if err := j.append(row); err != nil {
						t.Fatal(err)
					}
				}
				appendRow(r)
				now = at.Add(ingestionWindow + time.Second)
				accepted := make(map[string]bool)
				named := make(map[string]int)
				var calls int
				var originalID string
				o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
					fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
					notify: func(_ context.Context, rows []inboundJournalRecord) error {
						calls++
						id := rows[0].AlertBatchID
						if calls == 1 {
							originalID = id
						} else if calls == 2 && proven != "both" {
							want := "200.1"
							if proven == "B" {
								want = "100.1"
							}
							windows := ingestionAlertWindows(rows[0])
							if id != originalID || len(rows) != 1 || len(windows) != 1 || windows[0].MessageTS != want {
								t.Fatalf("retry changed identity or membership: id=%s windows=%+v", id, windows)
							}
						}
						// Model gc accepting mail before the adapter sees its result.
						if !accepted[id] {
							accepted[id] = true
							for _, row := range rows {
								for _, w := range ingestionAlertWindows(row) {
									named[w.MessageTS]++
								}
							}
						}
						if calls == 1 || (calls == 2 && mode == "second_timeout" && proven != "both") {
							return context.DeadlineExceeded
						}
						if calls == 2 && mode == "restart_after_retry" && proven != "both" {
							panic("restart before completion")
						}
						return nil
					}}
				restart := func() {
					j = &inboundJournal{dir: j.dir, retentionDays: 1, now: func() time.Time { return now }}
					o.cfg.inboundJournal = j
				}
				step := func() {
					t.Helper()
					if err := o.step(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if err := o.step(context.Background()); err == nil {
					t.Fatal("expected accepted-mail timeout")
				}
				now = now.Add(time.Second)
				for i, w := range r.Windows {
					if proven == "both" || (proven == "A" && i == 0) || (proven == "B" && i == 1) {
						recordDeliveredPublish(config{inboundJournal: j}, r.Channel, w.ThreadTS, fmt.Sprintf("%d.000000", now.Unix()), "s1")
					}
				}
				if mode == "restart" || mode == "restart_after_retry" {
					restart()
				}
				if mode == "late_before_retry" {
					late := r
					late.TargetSessions, late.CanonicalSessionID = []string{"s2"}, "s2"
					appendRow(late)
				}
				if mode == "second_timeout" && proven != "both" {
					if err := o.step(context.Background()); err == nil {
						t.Fatal("expected second timeout")
					}
				} else if mode == "restart_after_retry" && proven != "both" {
					func() {
						defer func() {
							if recover() != "restart before completion" {
								t.Fatal("expected crash after deduplicated retry")
							}
						}()
						step()
					}()
					restart()
				}
				step()
				restart()
				j.mu.Lock()
				pending, _, _, err := j.ingestionStateLocked(now)
				j.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				coverage := pending[ingestionKey(r, "s1")].AlertCoverage
				if len(coverage) != 2 {
					t.Fatalf("accepted A+B coverage lost: %+v", coverage)
				}
				for _, c := range coverage {
					if c.Session != "s1" || c.BatchID != originalID {
						t.Fatalf("original mail attribution lost: %+v", c)
					}
				}
				for _, row := range ingestionJournalRows(t, j) {
					if row.Phase == "ingestion_batch_done" && row.AlertBatch.ID == originalID && row.AlertBatch.Sent != (proven != "both") {
						t.Fatal("completion misreported whether this pass sent mail")
					}
				}
				if mode == "retention" || mode == "retention_new_target" {
					// A current transport row must retain yesterday's attribution
					// when the source day ages out.
					now = at.Add(24 * time.Hour)
					if mode == "retention" {
						appendRow(r)
					}
					restart()
				}
				late := r
				late.TargetSessions, late.CanonicalSessionID, late.DeliveredAt = []string{"s2"}, "s2", now
				appendRow(late)
				newcomer := ingestionTestRecord("300.1", now, "new unread message")
				newcomer.ThreadTS = "300.0"
				newcomer.Windows = []injectionWindow{{MessageTS: "300.1", ThreadTS: "300.0"}}
				appendRow(newcomer)
				now = now.Add(ingestionWindow + time.Second)
				step()
				step()
				if mode == "retention_new_target" {
					now = now.Add(3 * time.Hour)
					appendRow(inboundJournalRecord{Phase: "maintenance"})
					restart()
				}
				late.TargetSessions, late.CanonicalSessionID = []string{"s3"}, "s3"
				late.DeliveredAt = now.Add(-ingestionWindow - time.Second)
				appendRow(late)
				step()
				// The channel-wide once rule: late s2 does not repeat A or B,
				// while C must never disappear behind the original dedup key.
				if len(named) != 3 || named["100.1"] != 1 || named["200.1"] != 1 || named["300.1"] != 1 {
					t.Fatalf("hidden or repeated message: %v", named)
				}
			})
		}
	}
}

func TestIngestionOversizeReceiptKeepsEveryMessage(t *testing.T) {
	at := time.Now().UTC()
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	r := ingestionTestRecord("1794200000.000001", at, "many messages")
	r.Windows = nil
	for i := 0; i < 300; i++ {
		ts := fmt.Sprintf("1794200000.%06d", i)
		r.Windows = append(r.Windows, injectionWindow{MessageTS: ts, ThreadTS: ts})
	}
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	now = at.Add(ingestionWindow + time.Second)
	named := make(map[string]int)
	var mails int
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(_ context.Context, rows []inboundJournalRecord) error {
			mails++
			for _, row := range rows {
				for _, w := range ingestionAlertWindows(row) {
					named[w.MessageTS]++
				}
			}
			return nil
		}}
	for i := 0; i < 4; i++ {
		if err := o.step(context.Background()); err != nil {
			t.Fatal(err)
		}
		o.cfg.inboundJournal = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
	}
	if mails < 2 || len(named) != 300 {
		t.Fatalf("unmailed members lost: mails=%d named=%d", mails, len(named))
	}
	for ts, count := range named {
		if count != 1 {
			t.Fatalf("message %s named %d times", ts, count)
		}
	}
}

func TestIngestionFreshBatchCompletionCoversOnlySentRows(t *testing.T) {
	now := time.Now().UTC()
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now }}
	a, b := ingestionTestRecord("100.1", now, "a"), ingestionTestRecord("200.1", now, "b")
	for _, row := range []inboundJournalRecord{a, b} {
		if err := j.append(row); err != nil {
			t.Fatal(err)
		}
	}
	batch, _, err := o.prepareAlertBatch("C1", []inboundJournalRecord{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.finishAlertBatch(batch, []inboundJournalRecord{b}, true); err != nil {
		t.Fatal(err)
	}
	rows, err := o.pending()
	if err != nil {
		t.Fatal(err)
	}
	if rows[ingestionKey(a, "s1")].Alerted || !rows[ingestionKey(b, "s1")].Alerted {
		t.Fatal("fresh completion covered an unsent row or lost the sent row")
	}
}

func TestIngestionLargeFanoutBatchAndTargetRows(t *testing.T) {
	for _, targets := range []int{3, 300} {
		t.Run(fmt.Sprint(targets), func(t *testing.T) {
			at := time.Now().UTC()
			now := at
			j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
			r := ingestionTestRecord("1794200000.000001", at, "fanout")
			r.ThreadTS = "1794199999.000001"
			r.Windows = []injectionWindow{{MessageTS: r.MessageTS, ThreadTS: r.ThreadTS}}
			r.TargetSessions = nil
			for i := 0; i < targets; i++ {
				r.TargetSessions = append(r.TargetSessions, fmt.Sprintf("ci-wisp-target-%03d", i))
			}
			if err := j.append(r); err != nil {
				t.Fatal(err)
			}
			now = at.Add(ingestionWindow + time.Second)
			mails := 0
			o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
				fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
				notify: func(_ context.Context, rows []inboundJournalRecord) error {
					mails++
					if len(rows) != 1 || len(ingestionAlertWindows(rows[0])) != 1 {
						t.Fatal("fanout changed the single message's mail")
					}
					return nil
				}}
			for i := 0; i < 3; i++ {
				if i == 1 {
					late := r
					late.TargetSessions = []string{"late-target"}
					if err := o.cfg.inboundJournal.append(late); err != nil {
						t.Fatal(err)
					}
				}
				if err := o.step(context.Background()); err != nil {
					t.Fatal(err)
				}
				o.cfg.inboundJournal = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
			}
			if mails != 1 {
				t.Fatalf("once rule failed: %d mails", mails)
			}
			for _, row := range ingestionJournalRows(t, j) {
				if row.AlertBatch != nil {
					data, _ := json.Marshal(row)
					if len(data) > ingestionAlertBatchBudget {
						t.Fatalf("oversized saved plan: %d", len(data))
					}
				}
				if len(row.TargetSessions) == 1 {
					for _, c := range row.AlertCoverage {
						if c.Session != row.TargetSessions[0] {
							t.Fatalf("target row repeats another target's coverage: target=%s coverage=%s", row.TargetSessions[0], c.Session)
						}
					}
				}
			}
		})
	}
}
