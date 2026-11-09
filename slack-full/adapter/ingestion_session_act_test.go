package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestIngestionSessionActCacheOverflowKeepsDurableProof(t *testing.T) {
	at := time.Now().UTC()
	now := at.Add(ingestionWindow + time.Second)
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	r := ingestionTestRecord("100.1", at, "body")
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for i := 0; i <= ingestionActCacheLimit; i++ {
		message := fmt.Sprintf("different.%d", i)
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
