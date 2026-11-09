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

func TestIngestionPartialAlertLateFanout(t *testing.T) {
	for _, mode := range []string{"live", "restart", "batch_completion_crash", "retention"} {
		t.Run(mode, func(t *testing.T) {
			at := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)
			now := at
			j := &inboundJournal{dir: t.TempDir(), retentionDays: 1, now: func() time.Time { return now }}
			r := ingestionTestRecord("200.1", at, "coalesced late fanout")
			r.ThreadTS = "200.0"
			r.TargetSessions, r.HandoffSessions = []string{"s1", "s2"}, []string{"s1"}
			r.Windows = []injectionWindow{{MessageTS: "100.1", ThreadTS: "100.0"}, {MessageTS: "200.1", ThreadTS: "200.0"}}
			if err := j.append(r); err != nil {
				t.Fatal(err)
			}
			now = at.Add(time.Minute)
			recordDeliveredPublish(config{inboundJournal: j}, r.Channel, "200.0", fmt.Sprintf("%d.000000", now.Unix()), "s1")
			now = at.Add(ingestionWindow + time.Second)
			var ids []string
			o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
				fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
				notify: func(_ context.Context, rows []inboundJournalRecord) error {
					ids = append(ids, rows[0].AlertBatchID)
					if len(ids) > 2 {
						t.Fatal("already named member alerted again")
					}
					want, absent, session := "100.1", "200.1", "s1"
					if len(ids) == 2 {
						want, absent, session = absent, want, "s2"
					}
					subject, body := ingestionAlertMessage(rows)
					if len(rows) != 1 || !strings.Contains(subject, "1 lines") ||
						!strings.Contains(body, "message_ts="+want+" ") || strings.Contains(body, "message_ts="+absent+" ") ||
						len(rows[0].TargetSessions) != 1 || rows[0].TargetSessions[0] != session {
						t.Fatalf("wrong partial mail for %s: %+v %s", session, rows, body)
					}
					return nil
				}}
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(ids) != 1 {
				t.Fatalf("first mail count=%d", len(ids))
			}
			if mode == "batch_completion_crash" {
				var data []byte
				for _, row := range ingestionJournalRows(t, j) {
					b, _ := json.Marshal(row)
					data = append(append(data, b...), '\n')
					if row.Phase == "ingestion_batch_done" {
						break
					}
				}
				if err := os.WriteFile(filepath.Join(j.dir, now.Format("20060102")+".jsonl"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "live" {
				j = &inboundJournal{dir: j.dir, retentionDays: 1, now: func() time.Time { return now }}
				o.cfg.inboundJournal = j
			}
			if mode == "retention" {
				now = at.Add(4*time.Hour + time.Minute)
			}
			r.HandoffSessions = []string{"s2"}
			r.DeliveredAt = now
			if err := j.append(r); err != nil {
				t.Fatal(err)
			}
			now = now.Add(ingestionWindow + time.Second)
			for i := 0; i < 3; i++ {
				if err := o.step(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if len(ids) != 2 || ids[0] == ids[1] {
				t.Fatalf("late unread B needs a distinct mail, got batch IDs %v", ids)
			}
			pending, err := o.pending()
			if err != nil {
				t.Fatal(err)
			}
			late := pending[ingestionKey(r, "s2")]
			if len(late.AlertCoverage) != 2 {
				t.Fatalf("missing durable message coverage: %+v", late.AlertCoverage)
			}
			for _, c := range late.AlertCoverage {
				if (c.MessageTS == "100.1" && (c.Session != "s1" || c.BatchID != ids[0])) ||
					(c.MessageTS == "200.1" && (c.Session != "s2" || c.BatchID != ids[1])) {
					t.Fatalf("mail attribution changed during recovery: %+v", c)
				}
			}
		})
	}
}

func TestIngestionPartialAlertRetryKeepsOriginalMembers(t *testing.T) {
	at := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	r := ingestionTestRecord("200.1", at, "coalesced retry")
	r.ThreadTS = "200.0"
	r.Windows = []injectionWindow{{MessageTS: "100.1", ThreadTS: "100.0"}, {MessageTS: "200.1", ThreadTS: "200.0"}}
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	now = at.Add(time.Minute)
	recordDeliveredPublish(config{inboundJournal: j}, r.Channel, "200.0", fmt.Sprintf("%d.000000", now.Unix()), "s1")
	now = at.Add(ingestionWindow + time.Second)
	var ids []string
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(_ context.Context, rows []inboundJournalRecord) error {
			ids = append(ids, rows[0].AlertBatchID)
			if len(ids) > 3 {
				t.Fatal("extra alert")
			}
			want, absent := "100.1", "200.1"
			if len(ids) == 3 {
				want, absent = absent, want
			}
			_, body := ingestionAlertMessage(rows)
			if !strings.Contains(body, "message_ts="+want+" ") || strings.Contains(body, "message_ts="+absent+" ") {
				t.Fatalf("retry membership grew or lost its original message: %s", body)
			}
			if len(ids) == 1 {
				return fmt.Errorf("mail may have been accepted; completion unavailable")
			}
			return nil
		}}
	if err := o.step(context.Background()); err == nil {
		t.Fatal("expected ambiguous mail failure")
	}
	j = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
	o.cfg.inboundJournal = j
	r.TargetSessions = []string{"s2"}
	r.DeliveredAt = now
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	now = now.Add(ingestionWindow + time.Second)
	for i := 0; i < 3; i++ {
		if err := o.step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(ids) != 3 || ids[0] != ids[1] || ids[1] == ids[2] {
		t.Fatalf("original retry and new member identities: %v", ids)
	}
}

func TestIngestionAdmissionEyesAreNotSessionProof(t *testing.T) {
	at := time.Now().UTC()
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	r := ingestionTestRecord("100.1", at, "automatic admission")
	r.Channel = testChannelID
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	h, _ := ackHarness(t)
	h.gw.cfg.inboundJournal = j
	spy := &ackSpy{}
	wireAckSpy(h, spy)
	ack := admitAckReceipt(t, h, r.MessageTS, r.ThreadTS, ingressStatusReceived, ackStateNone)
	now = at.Add(time.Minute)
	h.gw.applyAdmissionAck(ack)
	calls, _ := spy.snapshot()
	if len(calls) != 1 || calls[0].name != ackEmojiEyes || ack.AckState != ackStateEyes {
		t.Fatalf("fixture did not place admission eyes: %+v", calls)
	}
	now = at.Add(ingestionWindow + time.Second)
	alerts := 0
	o := &ingestionObserver{cfg: h.gw.cfg, now: func() time.Time { return now },
		fetch:  func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(context.Context, []inboundJournalRecord) error { alerts++; return nil }}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if alerts != 1 {
		t.Fatalf("adapter admission eyes suppressed the unseen alert: mails=%d", alerts)
	}
	for _, row := range ingestionJournalRows(t, j) {
		if row.Status == "ingested" {
			t.Fatal("adapter admission eyes became session proof")
		}
	}
}
