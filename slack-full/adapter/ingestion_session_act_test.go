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

func TestIngestionCoalescedSessionActs(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
	}{
		{name: "neither"},
		{name: "last_only", second: "reaction"},
		{name: "first_only", first: "reaction"},
		{name: "both_reactions", first: "reaction", second: "reaction"},
		{name: "mixed", first: "reply", second: "reaction"},
		{name: "both_replies", first: "reply", second: "reply"},
	} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart_%t", tc.name, restart), func(t *testing.T) {
				at := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
				now := at
				j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
				r := ingestionTestRecord("200.1", at, "coalesced A and B")
				r.ThreadTS = "200.0"
				r.Windows = []injectionWindow{{MessageTS: "100.1", ThreadTS: "100.0"}, {MessageTS: "200.1", ThreadTS: "200.0"}}
				if err := j.append(r); err != nil {
					t.Fatal(err)
				}
				for i, kind := range []string{tc.first, tc.second} {
					if kind == "" {
						continue
					}
					now = at.Add(time.Duration(i+1) * time.Minute)
					w := r.Windows[i]
					if kind == "reaction" {
						j.recordSessionAct(ingestionSessionAct{Kind: kind, Source: "session_react", Channel: r.Channel, MessageTS: w.MessageTS, Emoji: "eyes"})
					} else {
						recordDeliveredPublish(config{inboundJournal: j}, r.Channel, w.ThreadTS, fmt.Sprintf("%d.000000", now.Unix()), "s1")
					}
				}
				if restart {
					j = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
				}
				now = at.Add(ingestionWindow + time.Second)
				alerts := 0
				wantSeen := tc.first != "" && tc.second != ""
				o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
					fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
					notify: func(_ context.Context, rows []inboundJournalRecord) error {
						alerts++
						_, body := ingestionAlertMessage(rows)
						for i, kind := range []string{tc.first, tc.second} {
							w := r.Windows[i]
							line := fmt.Sprintf("message_ts=%s thread_ts=%s receipt=%s", w.MessageTS, w.ThreadTS, r.ReceiptID)
							if strings.Contains(body, line) != (kind == "") {
								t.Errorf("mail member %s: proof=%q, body=%s", w.MessageTS, kind, body)
							}
						}
						return nil
					}}
				for i := 0; i < 2; i++ {
					if err := o.step(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if (alerts == 0) != wantSeen || alerts > 1 {
					t.Errorf("alerts=%d wantSeen=%t", alerts, wantSeen)
				}
				seen := false
				for _, row := range ingestionJournalRows(t, j) {
					if row.Status == "ingested" {
						seen = true
						if row.Evidence == nil || !row.Evidence.Timestamp.Equal(at.Add(2*time.Minute)) || row.ProofKind != tc.second {
							t.Errorf("missing completing act evidence: %+v", row)
						}
						if row.Evidence == nil || len(row.Evidence.Members) != 2 {
							t.Errorf("missing constituent proofs: %+v", row.Evidence)
						}
					}
				}
				if seen != wantSeen {
					t.Errorf("ingested=%t want=%t", seen, wantSeen)
				}
			})
		}
	}
}

func TestIngestionCoalescedFanoutRetryAndLateProof(t *testing.T) {
	at := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	r := ingestionTestRecord("200.1", at, "coalesced fanout")
	r.ThreadTS, r.TargetSessions = "200.0", []string{"s1", "s2"}
	r.Windows = []injectionWindow{{MessageTS: "100.1", ThreadTS: "100.0"}, {MessageTS: "200.1", ThreadTS: "200.0"}}
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	now = at.Add(time.Minute)
	// Each target has answered a different constituent; neither has seen both.
	for i, session := range r.TargetSessions {
		recordDeliveredPublish(config{inboundJournal: j}, r.Channel, r.Windows[i].ThreadTS, fmt.Sprintf("%d.000000", now.Unix()), session)
	}
	now = at.Add(ingestionWindow + time.Second)
	alerts := 0
	var batchID string
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(_ context.Context, rows []inboundJournalRecord) error {
			alerts++
			subject, body := ingestionAlertMessage(rows)
			if alerts == 1 {
				batchID = rows[0].AlertBatchID
				if len(rows) != 1 || !strings.Contains(subject, "2 lines") ||
					strings.Count(body, "message_ts=100.1 ") != 1 || strings.Count(body, "message_ts=200.1 ") != 1 {
					t.Fatalf("missing fanout union: %s", body)
				}
				return fmt.Errorf("mail unavailable")
			}
			if alerts != 2 || rows[0].AlertBatchID != batchID || !strings.Contains(subject, "1 lines") ||
				strings.Contains(body, "message_ts=100.1 ") || !strings.Contains(body, "message_ts=200.1 ") {
				t.Fatalf("retry changed identity or missing members: %s", body)
			}
			return nil
		}}
	if err := o.step(context.Background()); err == nil {
		t.Fatal("expected mail failure")
	}
	// A now has global reaction proof; only B for s1 still lacks proof.
	j.recordSessionAct(ingestionSessionAct{Kind: "reaction", Source: "session_react", Channel: r.Channel, MessageTS: "100.1", Emoji: "eyes"})
	j = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
	o.cfg.inboundJournal = j
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	j.recordSessionAct(ingestionSessionAct{Kind: "reaction", Source: "session_react", Channel: r.Channel, MessageTS: "200.1", Emoji: "eyes"})
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range ingestionJournalRows(t, j) {
		if row.Status == "ingested" {
			seen[row.TargetSessions[0]] = true
			if !row.Late || len(row.Windows) != 2 || row.Evidence == nil || len(row.Evidence.Members) != 2 {
				t.Fatalf("late proof lost durable membership: %+v", row)
			}
		}
	}
	if alerts != 2 || len(seen) != 2 {
		t.Fatalf("alerts=%d seen=%v", alerts, seen)
	}
}

func TestIngestionSessionActDelayedConfirmation(t *testing.T) {
	for _, kind := range []string{"reaction", "reply"} {
		for _, offset := range []time.Duration{-31 * time.Second, -30 * time.Second, 30 * time.Second} {
			t.Run(fmt.Sprintf("%s/%s", kind, offset), func(t *testing.T) {
				at := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
				now := at.Add(offset)
				j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
				r := ingestionTestRecord("100.1", at.Add(2*time.Minute), "delayed confirmation")
				r.StartedAt = at
				if kind == "reaction" {
					j.recordSessionAct(ingestionSessionAct{Kind: kind, Source: "session_react", Channel: r.Channel, MessageTS: r.MessageTS, Emoji: "eyes"})
				} else {
					recordDeliveredPublish(config{inboundJournal: j}, r.Channel, r.ThreadTS, fmt.Sprintf("%d.000000", now.Unix()), "s1")
				}
				now = r.DeliveredAt
				if err := j.append(r); err != nil {
					t.Fatal(err)
				}
				j = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
				now = r.DeliveredAt.Add(ingestionWindow + time.Second)
				alerts := 0
				o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
					fetch:  func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
					notify: func(context.Context, []inboundJournalRecord) error { alerts++; return nil }}
				if err := o.step(context.Background()); err != nil {
					t.Fatal(err)
				}
				if wantSeen := offset >= -30*time.Second; (alerts == 0) != wantSeen {
					t.Fatalf("alerts=%d wantSeen=%t", alerts, wantSeen)
				}
			})
		}
	}
}

func TestIngestionSessionActSourcesAndRoot(t *testing.T) {
	at := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	r := ingestionTestRecord("100.1", at, "body")
	r.ThreadTS, r.CanonicalSessionID = "", "canonical"
	reply := ingestionSessionAct{Kind: "reply", Source: "session_publish", Channel: r.Channel,
		MessageTS: "200.1", ThreadTS: r.MessageTS, SessionID: "canonical", Timestamp: at.Add(time.Minute)}
	if sessionActEvidence(reply, r) == nil {
		t.Fatal("root reply from pinned canonical session ignored")
	}
	reply.Timestamp = at.Add(ingestionLateWindow + time.Second)
	if sessionActEvidence(reply, r) != nil {
		t.Fatal("reply outside late horizon accepted")
	}
	for _, source := range []string{"company_admission", "busy_reaction", ""} {
		act := ingestionSessionAct{Kind: "reaction", Source: source, Channel: r.Channel,
			MessageTS: r.MessageTS, Emoji: "eyes", Timestamp: at.Add(time.Minute)}
		if sessionActEvidence(act, r) != nil {
			t.Fatalf("automatic source %q became proof", source)
		}
	}
}

func TestIngestionReplyCannotCoverLaterInbound(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprintf("delayed_replay_%t", replay), func(t *testing.T) {
			at := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
			now := at
			j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
			r := ingestionTestRecord(fmt.Sprintf("%d.000000", at.Add(10*time.Second).Unix()), at.Add(20*time.Second), "new inbound")
			if replay {
				now = at.Add(time.Minute)
			}
			recordDeliveredPublish(config{inboundJournal: j}, r.Channel, r.ThreadTS, fmt.Sprintf("%d.000000", at.Unix()), "s1")
			now = r.DeliveredAt
			if err := j.append(r); err != nil {
				t.Fatal(err)
			}
			now = r.DeliveredAt.Add(ingestionWindow + time.Second)
			alerts := 0
			o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
				fetch:  func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
				notify: func(context.Context, []inboundJournalRecord) error { alerts++; return nil }}
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if alerts != 1 {
				t.Fatal("older reply suppressed newer unread input")
			}
		})
	}
}

func TestIngestionSessionActCacheOverflowKeepsDurableProof(t *testing.T) {
	at := time.Now().UTC()
	now := at.Add(ingestionWindow + time.Second)
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	r := ingestionTestRecord("100.1", at, "body")
	r.Windows = append([]injectionWindow{{MessageTS: "100.0", ThreadTS: "99.0"}}, r.Windows...)
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for i := 0; i <= ingestionActCacheLimit; i++ {
		message := fmt.Sprintf("different.%d", i)
		if i == 0 {
			message = "100.0" // first member is cached; the second needs disk fallback
		}
		if i == ingestionActCacheLimit {
			message = r.MessageTS
		}
		act := ingestionSessionAct{Kind: "reaction", Source: "session_react", Channel: r.Channel,
			MessageTS: message, Emoji: "eyes", Timestamp: at.Add(time.Minute)}
		row := inboundJournalRecord{Phase: "reaction", TS: act.Timestamp, SessionAct: &act}
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		data = append(append(data, b...), '\n')
	}
	if err := os.WriteFile(filepath.Join(j.dir, at.Format("20060102")+".acts.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch:  func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(context.Context, []inboundJournalRecord) error { t.Fatal("evicted proof caused alarm"); return nil }}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(j.sessionActs) != ingestionActCacheLimit || !j.sessionActsOverflow {
		t.Fatal("fixture did not exceed bounded cache")
	}
}

func TestIngestionSessionActClockToleranceSurvivesMidnight(t *testing.T) {
	at := time.Date(2026, 11, 9, 0, 0, 5, 0, time.UTC)
	now := at.Add(-10 * time.Second)
	j := &inboundJournal{dir: t.TempDir(), retentionDays: 1, now: func() time.Time { return now }}
	r := ingestionTestRecord("100.1", at, "body")
	j.recordSessionAct(ingestionSessionAct{Kind: "reaction", Source: "session_react", Channel: r.Channel, MessageTS: r.MessageTS, Emoji: "eyes"})
	now = at
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	j = &inboundJournal{dir: j.dir, retentionDays: 1, now: func() time.Time { return now }}
	now = at.Add(ingestionWindow + time.Second)
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(context.Context, []inboundJournalRecord) error {
			t.Fatal("midnight pruned qualifying proof")
			return nil
		}}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIngestionSessionActDuringPassSuppressesBatchMember(t *testing.T) {
	at := time.Now().UTC()
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	r := ingestionTestRecord("100.1", at, "first")
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	second := ingestionTestRecord("100.2", at, "second")
	second.TargetSessions = []string{"s2"}
	if err := j.append(second); err != nil {
		t.Fatal(err)
	}
	now = at.Add(ingestionWindow + time.Second)
	alerts := 0
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(_ context.Context, session string) ([]json.RawMessage, error) {
			if session == "s2" {
				j.recordSessionAct(ingestionSessionAct{Kind: "reaction", Source: "session_react", Channel: r.Channel, MessageTS: r.MessageTS, Emoji: "eyes"})
			}
			return nil, nil
		}, notify: func(_ context.Context, batch []inboundJournalRecord) error {
			alerts++
			if len(batch) != 1 || batch[0].MessageTS != second.MessageTS {
				t.Fatalf("stale batch: %+v", batch)
			}
			return nil
		}}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if alerts != 1 {
		t.Fatalf("alerts=%d", alerts)
	}
}

func TestIngestionChannelBatchFailureDoesNotHideOtherChannel(t *testing.T) {
	at := time.Now().UTC()
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	for i, channel := range []string{"C1", "C1", "C2"} {
		r := ingestionTestRecord(fmt.Sprintf("100.%d", i), at, "body")
		r.Channel = channel
		if err := j.append(r); err != nil {
			t.Fatal(err)
		}
	}
	now = at.Add(ingestionWindow + time.Second)
	calls := map[string]int{}
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(_ context.Context, batch []inboundJournalRecord) error {
			channel := batch[0].Channel
			calls[channel]++
			if channel == "C1" && calls[channel] == 1 {
				return fmt.Errorf("mail offline")
			}
			return nil
		}}
	if err := o.step(context.Background()); err == nil {
		t.Fatal("mail failure swallowed")
	}
	if calls["C1"] != 1 || calls["C2"] != 1 {
		t.Fatalf("calls=%v", calls)
	}
	o.cfg.inboundJournal = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls["C1"] != 2 || calls["C2"] != 1 {
		t.Fatalf("restart calls=%v", calls)
	}
}
