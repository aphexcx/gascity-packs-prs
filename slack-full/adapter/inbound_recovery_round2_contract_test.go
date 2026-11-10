package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRound2StateAndHistoryCompatibility(t *testing.T) {
	// Existing fields retain their exact name, type and JSON key. New fields
	// must be optional; both the deployed decoder and current decoder read them.
	for _, pair := range [][2]reflect.Type{
		{reflect.TypeOf(round2BaseInboundRecoveryEntry{}), reflect.TypeOf(inboundRecoveryEntry{})},
		{reflect.TypeOf(round2BaseInboundHoldNotice{}), reflect.TypeOf(inboundHoldNotice{})},
		{reflect.TypeOf(round2BaseInboundDeadLetterRecord{}), reflect.TypeOf(inboundDeadLetterRecord{})},
	} {
		for i := 0; i < pair[0].NumField(); i++ {
			old := pair[0].Field(i)
			new, ok := pair[1].FieldByName(old.Name)
			if !ok || old.Type != new.Type || old.Tag != new.Tag {
				t.Fatalf("non-additive change: %s", old.Name)
			}
		}
		for i := 0; i < pair[1].NumField(); i++ {
			new := pair[1].Field(i)
			if _, ok := pair[0].FieldByName(new.Name); !ok && !strings.Contains(new.Tag.Get("json"), ",omitempty") {
				t.Fatalf("required new field: %s", new.Name)
			}
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	dead := filepath.Join(dir, "dead")
	now := time.Now()
	msg := testPending("C1", "100.1", "base payload").inbound
	base := round2BaseInboundRecoveryState{Entries: map[string]round2BaseInboundRecoveryEntry{"C1|100.1": {Inbound: msg, HeldAt: now, NextAttemptAt: now.Add(time.Minute), TargetSessions: []string{"s1"}, OriginalReceiptID: "r"}}, Notices: map[string]round2BaseInboundHoldNotice{"C1": {TS: "n", Since: now}}}
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := newInboundRecovery(path, dead)
	if err != nil {
		t.Fatal(err)
	}
	e := r.state.Entries["C1|100.1"]
	if e.Inbound.Text != msg.Text || e.OriginalReceiptID != "r" || e.ReplayBlockedReason != "" || e.BusyReaction || !e.LastVouchAt.IsZero() {
		t.Fatalf("base defaults changed: %+v", e)
	}
	e.BusyReaction = true
	e.ReplayBlockedReason = "manual"
	e.AcknowledgedAt = now
	e.LastVouchAt = now
	e.LastAttemptAt = now
	e.PendingSince = now
	e.NextPollAt = now
	e.RetryStep = 3
	e.AliasOriginalSessionID = "old"
	r.put("C1|100.1", e)
	n := r.state.Notices["C1"]
	n.NextAttemptAt = now
	n.Failures = 2
	n.LastError = "error"
	n.LoggedErrors = []string{"error"}
	n.Ended = true
	n.PreviousTS = []string{"old-notice"}
	r.saveNotice("C1", n)
	decoded, err := round2BaseReadRecoveryState(path)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Entries["C1|100.1"].Inbound.Text != msg.Text || decoded.Notices["C1"].TS != "n" {
		t.Fatal("rollback decoder lost existing fields")
	}
	baseRecord := round2BaseInboundDeadLetterRecord{Inbound: msg, Channel: "C1", DeadLetteredAt: now, OriginalReceiptID: "r", ReceiptStateKnown: true}
	oldData, _ := json.Marshal(baseRecord)
	var newRecord inboundDeadLetterRecord
	if err = json.Unmarshal(oldData, &newRecord); err != nil {
		t.Fatal(err)
	}
	if _, err = writeInboundDeadLetterRecords(dead, "C1", []inboundDeadLetterRecord{newRecord}); err != nil {
		t.Fatal(err)
	}
	fromOld, _ := newInboundRecovery("", dead)
	if err = fromOld.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	if fromOld.state.Entries["C1|100.1"].OriginalReceiptID != "r" {
		t.Fatal("base history lost provenance")
	}
	if err = r.writeDead(e, time.Time{}); err != nil {
		t.Fatal(err)
	}
	history, err := os.ReadFile(filepath.Join(dead, "C1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(history), []byte{'\n'}) {
		var old round2BaseInboundDeadLetterRecord
		if err = json.Unmarshal(line, &old); err != nil {
			t.Fatal(err)
		}
		if old.Inbound.ProviderMessageID != "100.1" {
			t.Fatal("rollback history payload changed")
		}
	}
}

func TestRound2HeldMentionRemovesAndRestoresBusyReaction(t *testing.T) {
	slackStub, reactions := newReactionRecordingSlackStub(t)
	withSlackAPIStub(t, slackStub)
	var ready atomic.Bool
	gcStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		status := "failed"
		if ready.Load() {
			status = "delivered"
		}
		fmt.Fprintf(w, `{"delivery":{"receipt_id":"r","status":%q,"members":[{"session_id":"s1","status":%q}]}}`, status, status)
	}))
	defer gcStub.Close()
	cfg := recoveryTestConfig(t, gcStub.URL)
	cfg.slackBotToken = "test"
	cfg.busyReaction = "hourglass"
	cfg.busyMarks = newBusyReactionRegistry()
	if err := installInboundRecovery(&cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cfg.inboundRecovery.now = func() time.Time { return now }
	env := botMentionEnvelope(t, "message", "EvHoldBusy", "C1", "100.1", "", "<@"+testBotUserID+"> work", true)
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
	first, second := reactions.await(t, 2*time.Second), reactions.await(t, 2*time.Second)
	if first.op != "add" || second.op != "remove" || second.name != "hourglass" {
		t.Fatalf("busy calls=%+v %+v", first, second)
	}
	if _, ok := cfg.busyMarks.pending("C1", "100.1"); ok {
		t.Fatal("held busy mark survived")
	}
	// No registry memory survives this reinstall: the durable fact drives re-add.
	if err := installInboundRecovery(&cfg); err != nil {
		t.Fatal(err)
	}
	cfg.inboundRecovery.now = func() time.Time { return now }
	now = now.Add(time.Minute)
	ready.Store(true)
	cfg.inboundRecovery.tick(false)
	if got := reactions.await(t, 2*time.Second); got.op != "add" || got.name != "hourglass" {
		t.Fatalf("recovery did not restore busy mark: %+v", got)
	}
}

// P8 specifically: an 18m20s send completes before the 30-minute retirement.
func TestRound2LatePendingReceiptSettlesBeforeBound(t *testing.T) {
	r, now := round2Recovery(t)
	start := *now
	round2Hold(t, r, "100.1")
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		if now.Sub(start) >= 1100*time.Second {
			return deliveryReceipt{present: true, id: "landed", status: "pending"}, nil
		}
		return recoveryReceipt("failed", "s1", false), nil
	}
	r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
		return receiptPollConcluded, recoveryReceipt("landed", "s1", true), nil
	}
	for sec := 1; sec <= 1800; sec++ {
		*now = start.Add(time.Duration(sec) * time.Second)
		r.tick(false)
	}
	e := r.state.Entries["C1|100.1"]
	if !e.DeadLetteredAt.IsZero() || e.DeliveredAt.IsZero() || e.DeliveredAt.After(start.Add(1110*time.Second)) {
		t.Fatalf("landed line retired: %+v", e)
	}
}

func TestRound2NoticeSettlementCopy(t *testing.T) {
	for _, kind := range []string{"terminal", "paste", "unknown", "gate-off"} {
		t.Run(kind, func(t *testing.T) {
			r, now := round2Recovery(t)
			round2Hold(t, r, "100.1")
			var texts []string
			r.notice = func(_, _, _, text string) (string, error) { texts = append(texts, text); return "n", nil }
			*now = now.Add(time.Minute)
			r.updateNotices()
			r.gated = kind != "gate-off"
			r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				switch kind {
				case "unknown":
					return deliveryReceipt{}, nil
				case "paste":
					return deliveryReceipt{followUpLanded: true}, nil
				default:
					return recoveryReceipt("ok", "s1", true), nil
				}
			}
			r.tick(false)
			want := map[string]string{"terminal": "handed to the session's terminal", "paste": "submission and reading unconfirmed", "unknown": "proof is unavailable", "gate-off": "delivery remains unverified"}[kind]
			if len(texts) != 2 || !strings.Contains(texts[1], want) || strings.Contains(texts[1], "delivered late") {
				t.Fatalf("copy=%v want=%s", texts, want)
			}
			if (kind == "unknown" || kind == "gate-off") && !r.state.Entries["C1|100.1"].DeliveredAt.IsZero() {
				t.Fatal("unknown delivery marked delivered")
			}
		})
	}
}

func TestRound2AgeLimitAppliesAfterPendingLoss(t *testing.T) {
	r, now := round2Recovery(t)
	round2Hold(t, r, "100.1")
	e := r.state.Entries["C1|100.1"]
	e.DeadLetteredAt = now.Add(-48 * time.Hour)
	e.PendingReceipt = true
	e.PendingReceiptID = "old"
	r.put("C1|100.1", e)
	r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
		return receiptPollConcluded, recoveryReceipt("old", "s1", false), nil
	}
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		t.Fatal("expired pending loss reposted")
		return deliveryReceipt{}, nil
	}
	r.tick(true)
	r.manual["C1"] = true
	r.tick(false)
	if r.state.Entries["C1|100.1"].PendingReceipt || r.counts()["C1"].Dead != 1 {
		t.Fatal("expired record not retained as dead letter")
	}
}

func TestRound2FailedNoticeEditEndsAtBound(t *testing.T) {
	r, now := round2Recovery(t)
	start := *now
	round2Hold(t, r, "100.1")
	edits := 0
	r.notice = func(_, _, ts, _ string) (string, error) {
		if ts != "" {
			edits++
			return "", fmt.Errorf("rate_limited")
		}
		return "n", nil
	}
	*now = now.Add(time.Minute)
	r.updateNotices()
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return recoveryReceipt("ok", "s1", true), nil }
	r.tick(false)
	for sec := 61; sec <= 3600; sec++ {
		*now = start.Add(time.Duration(sec) * time.Second)
		r.updateNotices()
	}
	if edits > 7 || !r.state.Notices["C1"].Resolved {
		t.Fatalf("unbounded edits=%d notice=%+v", edits, r.state.Notices["C1"])
	}
}

func TestRound2RetargetHistoryKeepsOriginalDedup(t *testing.T) {
	r, now := round2Recovery(t)
	msg := testPending("C1", "100.1", "alias").inbound
	if err := r.holdAlias(msg, "old", "mayor", "body", recoveryReceipt("r", "old", false)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(r.bound)
	r.tick(false)
	r.resolveAlias = func(string) (string, bool) { return "new", true }
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return recoveryReceipt("done", "new", true), nil }
	r.tick(true)
	rebuilt, _ := newInboundRecovery("", r.deadDir)
	if err := rebuilt.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	if len(rebuilt.state.Entries) != 1 || rebuilt.state.Entries["C1|100.1|alias:old"].DeliveredAt.IsZero() {
		t.Fatalf("retarget duplicated history: %+v", rebuilt.state.Entries)
	}
}

func TestRound2MissCountSurvivesSettlementBeforeProbe(t *testing.T) {
	r, now := round2Recovery(t)
	round2Hold(t, r, "100.1")
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return recoveryReceipt("ok", "s1", true), nil }
	*now = now.Add(time.Minute)
	r.tick(false)
	if r.newlyMissed() != 1 || r.newlyMissed() != 0 {
		t.Fatal("admission was not counted exactly once")
	}
}

func TestRound2RecoveryLifecycleLogs(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	r, now := round2Recovery(t)
	round2Hold(t, r, "100.1")
	*now = now.Add(r.bound)
	r.tick(false)
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return recoveryReceipt("ok", "s1", true), nil }
	r.manual["C1"] = true
	r.tick(false)
	for _, part := range []string{"inbound HOLD: LOSS chan=C1 ts=100.1 receipt=original dead-lettered after", "file=" + filepath.Join(r.deadDir, "C1.jsonl"), "inbound HOLD attempt:", "inbound HOLD settled:", "dead-lettered-at=", "trigger=verb armed=1"} {
		if !strings.Contains(logs.String(), part) {
			t.Fatalf("missing %q in %s", part, logs.String())
		}
	}
}

func TestRound2AcknowledgementDoesNotClaimTerminalHandoff(t *testing.T) {
	r, now := round2Recovery(t)
	round2Hold(t, r, "100.1")
	var texts []string
	r.notice = func(_, _, _, text string) (string, error) {
		texts = append(texts, text)
		return "n", nil
	}
	*now = now.Add(time.Minute)
	r.updateNotices()
	if _, err := r.acknowledge("C1", "100.1"); err != nil {
		t.Fatal(err)
	}
	r.updateNotices()
	if len(texts) != 2 || !strings.Contains(texts[1], "acknowledged by hand") || strings.Contains(texts[1], "handed to") || strings.Contains(texts[1], "0001-") {
		t.Fatalf("acknowledgement claimed delivery: %v", texts)
	}
}

func TestRound2LiveAliasHoldIsProvisionalUntilRetirement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/messages") {
			fmt.Fprint(w, `{"delivery":{"receipt_id":"alias","status":"failed"}}`)
		} else if strings.Contains(req.URL.Path, "/bindings") {
			fmt.Fprint(w, `{"items":[]}`)
		} else {
			fmt.Fprint(w, `{"delivery":{"receipt_id":"channel","status":"delivered"}}`)
		}
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	cfg.bindingCheck = newBindingCheckCache()
	cfg.mentionOnly, _ = newMentionOnlyRegistry("")
	cfg.mentionOnlyDeliveries = newMentionOnlyDeliveryLog()
	reg := newTestHandleAliasRegistry(t)
	if err := reg.Set("mayor", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.mentionOnly.Set("C1", mentionOnlyBinding{SessionID: "s1", SessionName: "mayor", Handle: "mayor"}); err != nil {
		t.Fatal(err)
	}
	if err := installInboundRecovery(&cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := cfg.inboundRecovery
	r.now = func() time.Time { return now }
	env := botMentionEnvelope(t, "message", "alias-held", "C1", "100.1", "", "@mayor: please handle this", true)
	done := make(chan struct{})
	processSlackEvent(cfg, reg, nil, nil, nil, nil, env, func() { close(done) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("alias dispatch did not conclude")
	}
	list := cfg.mentionOnlyDeliveries.forSession("s1")
	if len(list) != 1 || !list[0].Provisional {
		t.Fatalf("held alias exposed as delivered: %+v", list)
	}
	now = now.Add(r.bound)
	r.tick(false)
	if list = cfg.mentionOnlyDeliveries.forSession("s1"); len(list) != 0 {
		t.Fatalf("dead-letter alias retained routing record: %+v", list)
	}
}

func TestRound2NoticeLogsEachDistinctErrorOnceAcrossRestart(t *testing.T) {
	r, _ := round2Recovery(t)
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	n := inboundHoldNotice{}
	for _, message := range []string{"rate_limited", "temporary_failure", "rate_limited"} {
		n = r.noticeFailure("C1", n, fmt.Errorf("%s", message))
	}
	r.saveNotice("C1", n)
	reloaded, err := newInboundRecovery(r.path, r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.noticeFailure("C1", reloaded.state.Notices["C1"], fmt.Errorf("temporary_failure"))
	if strings.Count(logs.String(), "inbound HOLD notice:") != 2 {
		t.Fatalf("repeated notice error logged: %s", logs.String())
	}
}

// P12's separate five-minute follower sequence must wake the older head.
func TestRound2FiveMinuteFollowerWakesHead(t *testing.T) {
	r, now := round2Recovery(t)
	start := *now
	round2Hold(t, r, "100.1")
	var attempts []time.Duration
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		if e.Inbound.ProviderMessageID != "100.1" {
			t.Fatal("follower overtook its unsettled head")
		}
		attempts = append(attempts, now.Sub(start))
		return recoveryReceipt("busy", "s1", false), nil
	}
	for second := 1; second <= 380; second++ {
		*now = start.Add(time.Duration(second) * time.Second)
		if second == 300 {
			round2Hold(t, r, "100.2")
		}
		r.tick(false)
	}
	want := []time.Duration{20 * time.Second, 80 * time.Second, 200 * time.Second, 320 * time.Second, 380 * time.Second}
	if !reflect.DeepEqual(attempts, want) {
		t.Fatalf("head attempts=%v want=%v", attempts, want)
	}
}
