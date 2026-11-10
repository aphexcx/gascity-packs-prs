package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
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
	if e.Inbound.Text != msg.Text || e.OriginalReceiptID != "r" || e.ReplayBlockedReason != "" || e.BusyReaction || !e.LastVouchAt.IsZero() || e.ReactionReceiptGeneration != 0 || e.ReplayRequestGeneration != 0 {
		t.Fatalf("base defaults changed: %+v", e)
	}
	e.BusyReaction = true
	e.ReactionReceiptGeneration = 2
	e.ReplayIncludeOlder = true
	e.ReplayRequestGeneration = 3
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
	current, err := newInboundRecovery(path, dead)
	if err != nil || current.state.Entries["C1|100.1"].ReplayRequestGeneration != 3 {
		t.Fatalf("new decoder lost replay request generation: %v", err)
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
	if fromOld.state.Entries["C1|100.1"].OriginalReceiptID != "r" || fromOld.state.Entries["C1|100.1"].ReactionReceiptGeneration != 0 {
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

func TestRound2RecoveryWaitsForHeldHourglassRemoval(t *testing.T) {
	for _, route := range []string{"channel", "alias"} {
		t.Run(route, func(t *testing.T) {
			removing, release := make(chan struct{}, 1), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			adds := make(chan int32, 2)
			var added atomic.Int32
			var removed, outOfOrder atomic.Bool
			slackStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasSuffix(req.URL.Path, "/reactions.remove"):
					removing <- struct{}{}
					<-release
					removed.Store(true)
				case strings.HasSuffix(req.URL.Path, "/reactions.add"):
					n := added.Add(1)
					if n > 1 && !removed.Load() {
						outOfOrder.Store(true)
					}
					adds <- n
				}
				fmt.Fprint(w, `{"ok":true}`)
			}))
			t.Cleanup(slackStub.Close)
			withSlackAPIStub(t, slackStub)
			var ready atomic.Bool
			gcStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if strings.Contains(req.URL.Path, "/bindings") {
					fmt.Fprint(w, `{"items":[]}`)
					return
				}
				status := "failed"
				if ready.Load() || (route == "alias" && !strings.HasSuffix(req.URL.Path, "/messages")) {
					status = "delivered"
				}
				fmt.Fprintf(w, `{"delivery":{"receipt_id":"busy","status":%q,"members":[{"session_id":"s1","status":%q}]}}`, status, status)
			}))
			defer gcStub.Close()
			cfg := recoveryTestConfig(t, gcStub.URL)
			cfg.bindingCheck = newBindingCheckCache()
			cfg.slackBotToken, cfg.busyReaction = "test", "hourglass"
			cfg.busyMarks = newBusyReactionRegistry()
			reg := newTestHandleAliasRegistry(t)
			body := "<@" + testBotUserID + "> work"
			if route == "alias" {
				if err := reg.Set("mayor", "s1"); err != nil {
					t.Fatal(err)
				}
				body = "@mayor: work"
			}
			if err := installInboundRecovery(&cfg); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			cfg.inboundRecovery.now = func() time.Time { return now }
			env := botMentionEnvelope(t, "message", "EvHoldBusyFence", "C1", "100.1", "", body, true)
			processSlackEvent(cfg, reg, nil, nil, nil, nil, env, func() {})
			select {
			case <-removing:
			case <-time.After(3 * time.Second):
				t.Fatal("HOLD removal did not start")
			}
			if n := <-adds; n != 1 {
				t.Fatalf("initial adds=%d", n)
			}
			now = now.Add(time.Minute)
			ready.Store(true)
			cfg.inboundRecovery.tick(false)
			if _, ok := cfg.busyMarks.pending("C1", "100.1"); ok {
				t.Fatal("recovery restored the registry before HOLD removal finished")
			}
			close(release)
			select {
			case n := <-adds:
				if n != 2 || outOfOrder.Load() {
					t.Fatalf("busy restoration overtook removal: adds=%d outOfOrder=%v", n, outOfOrder.Load())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("recovery did not restore hourglass after removal")
			}
		})
	}
}

func TestRound2ConcurrentTwinsHoldWaitsForEveryBusyAdd(t *testing.T) {
	aliasEntered, releaseAlias := make(chan struct{}), make(chan struct{})
	firstAdd, twinAdd, releaseTwinAdd := make(chan struct{}), make(chan struct{}), make(chan struct{})
	removed := make(chan struct{}, 4)
	defer func() {
		for _, ch := range []chan struct{}{releaseAlias, releaseTwinAdd} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	var adds atomic.Int32
	slackStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/reactions.add"):
			if adds.Add(1) == 1 {
				close(firstAdd)
			} else {
				close(twinAdd)
				<-releaseTwinAdd
			}
		case strings.HasSuffix(req.URL.Path, "/reactions.remove"):
			removed <- struct{}{}
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(slackStub.Close)
	withSlackAPIStub(t, slackStub)
	gcStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/bindings") {
			fmt.Fprint(w, `{"items":[]}`)
			return
		}
		status := "delivered"
		if strings.HasSuffix(req.URL.Path, "/messages") {
			close(aliasEntered)
			<-releaseAlias
			status = "failed"
		}
		fmt.Fprintf(w, `{"delivery":{"receipt_id":"twins","status":%q,"members":[{"session_id":"s1","status":%q}]}}`, status, status)
	}))
	t.Cleanup(gcStub.Close)
	cfg := recoveryTestConfig(t, gcStub.URL)
	cfg.bindingCheck = newBindingCheckCache()
	cfg.slackBotToken, cfg.busyReaction = "test", "hourglass"
	cfg.busyMarks = newBusyReactionRegistry()
	reg := newTestHandleAliasRegistry(t)
	if err := reg.Set("mayor", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := installInboundRecovery(&cfg); err != nil {
		t.Fatal(err)
	}
	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal(what)
		}
	}
	ownerDone, twinDone := make(chan struct{}), make(chan struct{})
	owner := botMentionEnvelope(t, "message", "EvConcurrentHold", "C1", "100.1", "99.1", "@mayor: work", true)
	processSlackEvent(cfg, reg, nil, nil, nil, nil, owner, func() { close(ownerDone) })
	wait(aliasEntered, "alias did not start")
	wait(firstAdd, "owner add did not start")
	twin := botMentionEnvelope(t, "app_mention", "EvConcurrentHoldTwin", "C1", "100.1", "99.1", "@mayor: work", true)
	processSlackEvent(cfg, reg, nil, nil, nil, nil, twin, func() { close(twinDone) })
	wait(twinAdd, "twin add did not start")
	close(releaseAlias)
	wait(ownerDone, "owner did not enter HOLD")
	wait(twinDone, "twin did not conclude")
	select {
	case <-removed:
		t.Fatal("HOLD cleanup overtook the twin's in-flight add")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseTwinAdd)
	wait(removed, "HOLD did not remove the merged busy mark")
	if _, ok := cfg.busyMarks.pending("C1", "99.1"); ok {
		t.Fatal("held twin left a registry mark")
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

func TestRound2HoldPreservesUnansweredPredecessorBusy(t *testing.T) {
	for _, route := range []string{"channel", "alias", "suppressed-alias"} {
		t.Run(route, func(t *testing.T) {
			slackStub, reactions := newReactionRecordingSlackStub(t)
			withSlackAPIStub(t, slackStub)
			gcStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if strings.Contains(req.URL.Path, "/bindings") {
					if route == "suppressed-alias" {
						fmt.Fprint(w, `{"items":[{"status":"active","conversation":{"provider":"slack","conversation_id":"C1"}}]}`)
					} else {
						fmt.Fprint(w, `{"items":[]}`)
					}
					return
				}
				status := "failed"
				if route == "alias" && !strings.HasSuffix(req.URL.Path, "/messages") {
					status = "delivered"
				}
				fmt.Fprintf(w, `{"delivery":{"receipt_id":"r","status":%q,"members":[{"session_id":"s1","status":%q}]}}`, status, status)
			}))
			defer gcStub.Close()
			cfg := recoveryTestConfig(t, gcStub.URL)
			cfg.bindingCheck = newBindingCheckCache()
			cfg.slackBotToken, cfg.busyReaction = "test", "hourglass"
			cfg.busyMarks = newBusyReactionRegistry()
			reg := newTestHandleAliasRegistry(t)
			text := "<@" + testBotUserID + "> work"
			if route != "channel" {
				if err := reg.Set("mayor", "s1"); err != nil {
					t.Fatal(err)
				}
				text = "@mayor: work"
			}
			if err := installInboundRecovery(&cfg); err != nil {
				t.Fatal(err)
			}
			oldDone, _ := cfg.busyMarks.markBoth("C1", "99.1", "100.1")
			close(oldDone) // the previous unanswered message already wears its mark
			env := botMentionEnvelope(t, "message", "EvHoldPredecessor", "C1", "100.2", "99.1", text, true)
			done := make(chan struct{})
			processSlackEvent(cfg, reg, nil, nil, nil, nil, env, func() { close(done) })
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("dispatch did not conclude")
			}
			for _, op := range []string{"add", "remove"} {
				if got := reactions.await(t, 2*time.Second); got.op != op || got.timestamp != "100.2" {
					t.Fatalf("HOLD removed predecessor or left current mark: %+v", got)
				}
			}
			if ts, ok := cfg.busyMarks.pending("C1", "99.1"); !ok || ts != "100.1" {
				t.Fatalf("predecessor was not restored: %s %v", ts, ok)
			}
			intent := false
			for _, e := range cfg.inboundRecovery.state.Entries {
				intent = intent || (e.Inbound.ProviderMessageID == "100.2" && e.BusyReaction)
			}
			if !intent {
				t.Fatal("held route lost durable busy intent")
			}
			twin := botMentionEnvelope(t, "app_mention", "EvHoldPredecessorTwin", "C1", "100.2", "99.1", text, true)
			twinDone := make(chan struct{})
			processSlackEvent(cfg, reg, nil, nil, nil, nil, twin, func() { close(twinDone) })
			select {
			case <-twinDone:
			case <-time.After(3 * time.Second):
				t.Fatal("held twin did not conclude")
			}
			reactions.assertNoCall(t, 50*time.Millisecond)
		})
	}
}

func TestRound2ReplyDuringRecoveryDoesNotRestoreBusy(t *testing.T) {
	for _, phase := range []string{"initial-post", "pending", "pending-restart", "replacement-post", "settled"} {
		t.Run(phase, func(t *testing.T) {
			slackStub, reactions := newReactionRecordingSlackStub(t)
			withSlackAPIStub(t, slackStub)
			var cfg config
			gcStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if phase == "initial-post" {
					clearBusyReactionBefore(cfg, "C1", "99.1", "101.1")
				}
				fmt.Fprint(w, `{"delivery":{"receipt_id":"r","status":"pending","members":[{"session_id":"s1","status":"pending"}]}}`)
			}))
			defer gcStub.Close()
			cfg = recoveryTestConfig(t, gcStub.URL)
			cfg.slackBotToken, cfg.busyReaction = "test", "hourglass"
			cfg.busyMarks = newBusyReactionRegistry()
			if err := installInboundRecovery(&cfg); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			cfg.inboundRecovery.now = func() time.Time { return now }
			env := botMentionEnvelope(t, "message", "EvBusyReply", "C1", "100.1", "99.1", "<@"+testBotUserID+"> work", true)
			processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
			if got := reactions.await(t, 2*time.Second); got.op != "add" {
				t.Fatalf("initial mark=%+v", got)
			}
			removes := 1
			if phase == "initial-post" {
				removes++ // reply cleanup and HOLD cancellation both remove safely
			}
			for i := 0; i < removes; i++ {
				if got := reactions.await(t, 2*time.Second); got.op != "remove" {
					t.Fatalf("hold cleanup=%+v", got)
				}
			}
			if phase == "pending" || phase == "pending-restart" {
				clearBusyReactionBefore(cfg, "C1", "99.1", "101.1")
			}
			if phase == "pending-restart" {
				cfg.busyMarks = newBusyReactionRegistry()
				if err := installInboundRecovery(&cfg); err != nil {
					t.Fatal(err)
				}
				cfg.inboundRecovery.now = func() time.Time { return now }
			}
			r := cfg.inboundRecovery
			r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
				return receiptPollConcluded, recoveryReceipt("r", "s1", phase != "replacement-post"), nil
			}
			r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				clearBusyReactionBefore(cfg, "C1", "99.1", "101.1")
				return recoveryReceipt("replacement", "s1", true), nil
			}
			if phase == "settled" {
				finished := r.finished
				r.finished = func(e inboundRecoveryEntry) {
					clearBusyReactionBefore(cfg, "C1", "99.1", "101.1")
					finished(e)
				}
			}
			now = now.Add(time.Minute)
			r.tick(false)
			e := r.state.Entries["C1|100.1"]
			if e.DeliveredAt.IsZero() || e.BusyReaction {
				t.Fatalf("reply completion lost: delivered=%s busy=%v", e.DeliveredAt, e.BusyReaction)
			}
			if cfg.busyMarks.size() != 0 {
				t.Fatal("answered line re-registered as busy")
			}
			reactions.assertNoCall(t, 50*time.Millisecond)
		})
	}
}

func TestRound2BusyCompletionRespectsThreadAndCutoff(t *testing.T) {
	r, _ := round2Recovery(t)
	marks := newBusyReactionRegistry()
	for _, ts := range []string{"100.1", "100.2", "102.1"} {
		msg := testPending("C1", ts, "work").inbound
		msg.ReplyToMessageID = "99.1"
		if ts == "100.2" {
			msg.ReplyToMessageID = "98.1"
		}
		if err := r.hold(pendingChannelInbound{inbound: msg}, recoveryReceipt("r", "s1", false), false, true); err != nil {
			t.Fatal(err)
		}
	}
	r.takeBusyBefore(marks, "C2", "99.1", "101.1")
	if !r.state.Entries["C1|100.1"].BusyReaction {
		t.Fatal("other channel cancelled intent")
	}
	r.takeBusyBefore(marks, "C1", "99.1", "101.1")
	for key, e := range r.state.Entries {
		want := e.Inbound.ProviderMessageID != "100.1"
		if e.BusyReaction != want {
			t.Fatalf("thread/cutoff mismatch %s: busy=%v", key, e.BusyReaction)
		}
	}
	r.takeBusyBefore(marks, "C1", "", "103.1")
	for key, e := range r.state.Entries {
		if e.BusyReaction {
			t.Fatalf("top-level reply did not clear %s", key)
		}
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

func TestRound2RepeatedAliasRetargetRemovesDepartingRoutes(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				status, session := "failed", "b"
				if strings.Contains(req.URL.Path, "/session/c/messages") {
					status, session = "delivered", "c"
				}
				fmt.Fprintf(w, `{"delivery":{"receipt_id":%q,"status":%q,"members":[{"session_id":%q,"status":%q}]}}`, session, status, session, status)
			}))
			defer srv.Close()
			cfg := recoveryTestConfig(t, srv.URL)
			cfg.mentionOnly, _ = newMentionOnlyRegistry("")
			cfg.mentionOnlyDeliveries = newMentionOnlyDeliveryLog()
			cfg.handleAliases = newTestHandleAliasRegistry(t)
			for _, id := range []string{"a", "b", "c"} {
				if err := cfg.mentionOnly.Set("C1", mentionOnlyBinding{SessionID: id, SessionName: id, Handle: id}); err != nil {
					t.Fatal(err)
				}
			}
			if err := cfg.handleAliases.Set("mayor", "a"); err != nil {
				t.Fatal(err)
			}
			if err := installInboundRecovery(&cfg); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			r := cfg.inboundRecovery
			r.now = func() time.Time { return now }
			msg := testPending("C1", "100.1", "retargeted alias").inbound
			bindings := cfg.mentionOnly.ForChannel("C1")
			recordAliasDeliveryForMentionOnly(cfg, bindings, "a", msg, true)
			if err := r.holdAlias(msg, "a", "mayor", "body", recoveryReceipt("a", "a", false)); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"b", "c"} {
				if id == "c" && restart {
					if err := installInboundRecovery(&cfg); err != nil {
						t.Fatal(err)
					}
					r = cfg.inboundRecovery
					r.now = func() time.Time { return now }
				}
				if err := cfg.handleAliases.Set("mayor", id); err != nil {
					t.Fatal(err)
				}
				now = now.Add(time.Minute)
				r.observe(recoveryReceipt("vouch", id, true))
				r.tick(false)
				if id == "b" {
					rows := cfg.mentionOnlyDeliveries.forSession("b")
					if len(rows) != 1 || !rows[0].Provisional {
						t.Fatalf("fixture did not retain B provisional route: %+v", rows)
					}
				}
			}
			for _, id := range []string{"a", "b"} {
				if rows := cfg.mentionOnlyDeliveries.forSession(id); len(rows) != 0 {
					t.Fatalf("departed session %s retained routing: %+v", id, rows)
				}
			}
			rows := cfg.mentionOnlyDeliveries.forSession("c")
			if len(rows) != 1 || rows[0].Provisional || rows[0].TS != "100.1" {
				t.Fatalf("current delivered route missing: %+v", rows)
			}
		})
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

func TestRound2RefusedReturnedReactionStaysTerminalAcrossRestart(t *testing.T) {
	for _, historyOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("history-only=%v", historyOnly), func(t *testing.T) {
			cfg := recoveryTestConfig(t, "http://unused")
			r := cfg.inboundRecovery
			p := testPending("C1", "100.1", "permanently refused reaction")
			p.reaction = true
			if err := r.hold(p, recoveryReceipt("failed", "s1", false), false); err != nil {
				t.Fatal(err)
			}
			cfg.coalescer.charge("C1", cfg.coalescer.reactions["C1"][0], &inboundPostError{Status: 422, StatusText: "422 invalid payload"})
			if r.state.Entries["C1|100.1"].ReplayBlockedReason == "" || r.queuedReactions["C1|100.1"] {
				t.Fatal("ordinary refusal did not terminate the durable mirror")
			}
			if historyOnly {
				if err := os.Remove(r.path); err != nil {
					t.Fatal(err)
				}
			}
			fresh := coalescingTestConfig("http://unused", time.Hour)
			fresh.cityPath, fresh.inboundDeadLetterDir = cfg.cityPath, cfg.inboundDeadLetterDir
			fresh.deliveryReceiptGate = true
			if err := installInboundRecovery(&fresh); err != nil {
				t.Fatal(err)
			}
			fresh.inboundRecovery.now = func() time.Time { return time.Now().Add(time.Hour) }
			fresh.inboundRecovery.tick(true)
			if len(fresh.coalescer.reactions["C1"]) != 0 || fresh.inboundRecovery.state.Entries["C1|100.1"].ReplayBlockedReason == "" {
				t.Fatal("restart requeued a permanently refused reaction")
			}
			delivered := 0
			fresh.coalescer.deliver = func(_ string, batch []pendingChannelInbound) error {
				for _, p := range batch {
					if p.reaction {
						t.Error("refused reaction piggybacked again")
					}
					delivered++
				}
				return nil
			}
			t.Cleanup(func() { fresh.coalescer.flushAll() })
			fresh.coalescer.enqueue("C1", testPending("C1", "100.2", "later human line"))
			fresh.coalescer.flushAheadOf("C1", "200.1")
			if delivered != 1 {
				t.Fatalf("later line did not flow alone: %d", delivered)
			}
		})
	}
}

func TestRound2LegacyRefusalFoldsIntoExistingMirror(t *testing.T) {
	for _, alias := range []bool{false, true} {
		for _, status := range []int{404, 408, 429, 422} {
			t.Run(fmt.Sprintf("alias=%v/status=%d", alias, status), func(t *testing.T) {
				r, now := round2Recovery(t)
				msg := testPending("C1", "100.1", "legacy payload").inbound
				key, session := "C1|100.1", ""
				if alias {
					session, key = "s1", key+"|alias:s1"
				}
				if err := r.holdEntry(inboundRecoveryEntry{Inbound: msg, Reaction: !alias, AliasSessionID: session}, recoveryReceipt("r", "s1", false)); err != nil {
					t.Fatal(err)
				}
				record := inboundDeadLetterRecord{Inbound: msg, Channel: "C1", AliasSessionID: session, DeadLetteredAt: *now, Reason: fmt.Sprintf("%d: legacy refusal", status)}
				if _, err := writeInboundDeadLetterRecords(r.deadDir, "C1", []inboundDeadLetterRecord{record}); err != nil {
					t.Fatal(err)
				}
				if err := r.importDeadLetters(); err != nil {
					t.Fatal(err)
				}
				wantBlocked := status == 422 || (alias && status == 404)
				if got := r.state.Entries[key].ReplayBlockedReason != ""; got != wantBlocked {
					t.Fatalf("legacy classifier blocked=%v want=%v", got, wantBlocked)
				}
			})
		}
	}
}

func TestRound2ImmediateReactionFailureSurvivesRestart(t *testing.T) {
	for _, spooled := range []bool{false, true} {
		t.Run(fmt.Sprintf("spooled=%v", spooled), func(t *testing.T) {
			var posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				posts.Add(1)
				fmt.Fprint(w, `{"delivery":{"receipt_id":"done","status":"delivered","members":[{"session_id":"s1","status":"delivered"}]}}`)
			}))
			defer srv.Close()
			cfg := recoveryTestConfig(t, srv.URL)
			r := cfg.inboundRecovery
			now := time.Now()
			r.now = func() time.Time { return now }
			count := 1
			if spooled {
				count = maxBufferedReactionsPerChannel
			}
			for i := 0; i < count; i++ {
				p := testPending("C1", fmt.Sprintf("100.%03d", i), "durable failed reaction")
				p.reaction = true
				if err := r.hold(p, recoveryReceipt("failed", "s1", false), false); err != nil {
					t.Fatal(err)
				}
			}
			if len(cfg.coalescer.reactions["C1"]) != count {
				t.Fatal("immediate failures did not return to the side lane")
			}
			spool := newInboundSpool(filepath.Join(t.TempDir(), "shutdown.jsonl"))
			if spooled && !spool.spillBatch("C1", cfg.coalescer.reactions["C1"]) {
				t.Fatal("could not spool shutdown copies")
			}
			// Discard the old process's volatile buffer without a flush.
			cfg.coalescer.reactions["C1"] = nil
			fresh := coalescingTestConfig(srv.URL, time.Hour)
			fresh.cityPath, fresh.inboundDeadLetterDir = cfg.cityPath, cfg.inboundDeadLetterDir
			fresh.deliveryReceiptGate = true
			if err := installInboundRecovery(&fresh); err != nil {
				t.Fatal(err)
			}
			fresh.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
				return deliverCoalescedBatch(fresh, channel, batch)
			}
			t.Cleanup(func() { fresh.coalescer.flushAll() })
			if len(fresh.inboundRecovery.state.Entries) != count {
				t.Fatal("crash lost immediately failed reaction ownership")
			}
			if spooled {
				if _, err := spool.replay(fresh.coalescer); err != nil {
					t.Fatal(err)
				}
				if len(fresh.coalescer.reactions["C1"]) != 0 {
					t.Fatal("spool bypassed durable reaction admission dedup")
				}
			}
			now = now.Add(time.Minute)
			fresh.inboundRecovery.now = func() time.Time { return now }
			fresh.inboundRecovery.tick(true)
			fresh.coalescer.admitReaction("C1", testPending("C1", "100.000", "duplicate event"), false)
			if len(fresh.coalescer.reactions["C1"]) != count || fresh.coalescer.timerWorthyLocked("C1") || posts.Load() != 0 {
				t.Fatal("restart lost no-wake ownership or duplicated the reaction")
			}
			fresh.coalescer.enqueue("C1", testPending("C1", "200.1", "fresh human line"))
			fresh.coalescer.flushAheadOf("C1", "300.1")
			if posts.Load() != 1 || len(fresh.coalescer.reactions["C1"]) != 0 {
				t.Fatal("restored reactions failed to piggyback exactly once")
			}
		})
	}
}

func TestRound2LostReactionOverflowWaitsForFreshTraffic(t *testing.T) {
	for _, window := range []time.Duration{0, 8 * time.Second} {
		for _, pending := range []bool{false, true} {
			for _, trigger := range []string{"real", "fresh-overflow"} {
				t.Run(fmt.Sprintf("window=%s/pending=%v/%s", window, pending, trigger), func(t *testing.T) {
					var posts atomic.Int32
					var deliver atomic.Bool
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						posts.Add(1)
						status := "failed"
						if pending {
							status = "pending"
						}
						if deliver.Load() {
							status = "delivered"
						}
						fmt.Fprintf(w, `{"delivery":{"receipt_id":"overflow","status":%q,"members":[{"session_id":"s1","status":%q}]}}`, status, status)
					}))
					defer srv.Close()
					cfg := recoveryTestConfig(t, srv.URL)
					c, r := cfg.coalescer, cfg.inboundRecovery
					c.window = window
					// This matrix exercises scheduling, not snapshot durability
					// (covered by the restart regressions below).
					r.path = ""
					now := time.Now()
					r.now = func() time.Time { return now }
					for i := 0; i < maxBufferedReactionsPerChannel; i++ {
						c.admitReaction("C1", testPending("C1", fmt.Sprintf("100.%03d", i), "old reaction"), false)
					}
					if pending {
						r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
							return receiptPollConcluded, recoveryReceipt("overflow", "s1", false), nil
						}
						now = now.Add(time.Minute)
						r.tick(false)
					}
					c.reconcileTimers()
					c.mu.Lock()
					_, armed := c.timers["C1"]
					worthy, buffered := c.timerWorthyLocked("C1"), len(c.reactions["C1"])
					c.mu.Unlock()
					if armed || worthy || buffered != maxBufferedReactionsPerChannel || posts.Load() != 1 {
						t.Fatalf("lost overflow retried or disappeared: armed=%v worthy=%v buffered=%d posts=%d", armed, worthy, buffered, posts.Load())
					}
					// One fresh reaction cannot use the failed batch to earn a wake.
					c.admitReaction("C1", testPending("C1", "200.000", "fresh reaction"), false)
					if posts.Load() != 1 {
						t.Fatal("one fresh reaction retriggered the failed overflow")
					}
					if r.counts()["C1"] != (inboundRecoveryCounts{}) {
						t.Fatal("reactions entered human-line counts")
					}
					deliver.Store(true)
					if trigger == "real" {
						c.window = time.Hour
						c.enqueue("C1", testPending("C1", "300.1", "real message"))
						c.flushAheadOf("C1", "400.1")
					} else {
						for i := 1; i < maxBufferedReactionsPerChannel; i++ {
							c.admitReaction("C1", testPending("C1", fmt.Sprintf("200.%03d", i), "fresh reaction"), false)
						}
					}
					if posts.Load() != 2 || len(c.reactions["C1"]) != 0 {
						t.Fatalf("fresh traffic failed to carry retained reactions: posts=%d buffered=%d", posts.Load(), len(c.reactions["C1"]))
					}
				})
			}
		}
	}
}

func TestRound2AcknowledgedReactionIgnoresLatePendingReceipt(t *testing.T) {
	cfg := recoveryTestConfig(t, "http://unused")
	r := cfg.inboundRecovery
	now := time.Now()
	r.now = func() time.Time { return now }
	p := testPending("C1", "100.1", "reaction")
	p.reaction = true
	pending := deliveryReceipt{present: true, id: "old", status: "pending", targetSessionID: "s1"}
	if err := r.hold(p, pending, false); err != nil {
		t.Fatal(err)
	}
	r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
		return receiptPollConcluded, recoveryReceipt("old", "s1", false), nil
	}
	now = now.Add(time.Minute)
	r.tick(false)
	if !r.queuedReactions["C1|100.1"] {
		t.Fatal("fixture did not transfer reaction to side lane")
	}
	if n, err := r.acknowledge("C1", "100.1"); err != nil || n != 1 {
		t.Fatalf("acknowledge=%d: %v", n, err)
	}
	acknowledged := r.state.Entries["C1|100.1"].AcknowledgedAt
	// A piggyback POST began before acknowledgement and returns afterward.
	pending.id = "late-new-receipt"
	if err := r.hold(p, pending, false); err != nil {
		t.Fatal(err)
	}
	e := r.state.Entries["C1|100.1"]
	if !e.AcknowledgedAt.Equal(acknowledged) || e.PendingReceipt || e.ReactionReceiptGeneration != 0 || r.queuedReactions["C1|100.1"] {
		t.Fatalf("late receipt revived acknowledgement: %+v", e)
	}
	restarted, err := newInboundRecovery(r.path, r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.state.Entries["C1|100.1"].AcknowledgedAt.Equal(acknowledged) {
		t.Fatal("acknowledgement lost across restart")
	}
}

func TestRound2MixedFanoutLandedReactionNeverReturnsToBuffer(t *testing.T) {
	cfg := recoveryTestConfig(t, "http://unused.invalid")
	r := cfg.inboundRecovery
	now := time.Now()
	r.now = func() time.Time { return now }
	p := testPending("C1", "100.1", "already pasted reaction")
	p.reaction = true
	receipt := parseDeliveryReceipt([]byte(`{"delivery":{"receipt_id":"mixed","status":"partial","members":[{"session_id":"s1","status":"delivered"},{"session_id":"s2","status":"pending","delivered_bytes":10,"expected_bytes":10}]}}`))
	if receipt.verdict(true) != receiptUnconfirmed || !receipt.landedUnconfirmed() {
		t.Fatal("fixture lacks mixed-fanout paste evidence")
	}
	if err := r.hold(p, receipt, false); err != nil {
		t.Fatal(err)
	}
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		t.Fatal("resent a fully pasted reaction")
		return deliveryReceipt{}, nil
	}
	now = now.Add(time.Minute)
	r.tick(false)
	if r.state.Entries["C1|100.1"].DeliveredAt.IsZero() || len(cfg.coalescer.reactions["C1"]) != 0 {
		t.Fatal("mixed-fanout landing evidence was discarded")
	}
}

func TestRound2DeadReactionPiggybackCompletionSurvivesRestart(t *testing.T) {
	for _, outcome := range []string{"delivered", "mixed-paste", "pending"} {
		for _, historyOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/history-only=%v", outcome, historyOnly), func(t *testing.T) {
				var posts atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					body, _ := io.ReadAll(req.Body)
					if strings.Contains(string(body), "durable-reaction") {
						posts.Add(1)
						switch outcome {
						case "pending":
							fmt.Fprint(w, `{"delivery":{"receipt_id":"new-pending","status":"pending","members":[{"session_id":"s1","status":"pending"}]}}`)
							return
						case "mixed-paste":
							fmt.Fprint(w, `{"delivery":{"receipt_id":"mixed","status":"partial","members":[{"session_id":"s1","status":"delivered"},{"session_id":"s2","status":"pending","delivered_bytes":10,"expected_bytes":10}]}}`)
							return
						}
					}
					fmt.Fprint(w, `{"delivery":{"receipt_id":"done","status":"delivered","members":[{"session_id":"s1","status":"delivered"}]}}`)
				}))
				defer srv.Close()
				cfg := recoveryTestConfig(t, srv.URL)
				r := cfg.inboundRecovery
				now := time.Now()
				r.now = func() time.Time { return now }
				p := testPending("C1", "100.1", "durable-reaction")
				p.reaction = true
				pending := deliveryReceipt{present: true, id: "old-pending", status: "pending", targetSessionID: "s1"}
				if err := r.hold(p, pending, false); err != nil {
					t.Fatal(err)
				}
				r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
					return receiptPollPending, pending, nil
				}
				now = now.Add(r.bound)
				r.tick(false)
				if r.state.Entries["C1|100.1"].DeadLetteredAt.IsZero() {
					t.Fatal("fixture did not reach dead-letter history")
				}
				retired := r.state.Entries["C1|100.1"]
				r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
					return receiptPollConcluded, recoveryReceipt("old-pending", "s1", false), nil
				}
				now = now.Add(time.Minute)
				r.tick(false)
				r.tick(false)
				if !r.queuedReactions["C1|100.1"] || len(cfg.coalescer.reactions["C1"]) != 1 {
					t.Fatal("reaction handoff duplicated or discarded its durable mirror")
				}
				cfg.coalescer.enqueue("C1", p)
				if len(cfg.coalescer.reactions["C1"]) != 1 || !r.known("C1", "100.1") {
					t.Fatal("queued recovery mirror admitted a duplicate reaction")
				}
				cfg.coalescer.enqueue("C1", testPending("C1", "100.2", "fresh real line"))
				cfg.coalescer.flushAheadOf("C1", "200.1")
				if posts.Load() != 1 {
					t.Fatalf("piggyback sends=%d", posts.Load())
				}
				// A delayed writer can append the old receipt after the new
				// piggyback result. Neither history-only reconstruction nor
				// clearing its write intent may revive the obsolete receipt.
				if err := r.writeDead(retired, time.Time{}); err != nil {
					t.Fatal(err)
				}
				r.historyWritten("C1|100.1", retired)
				if outcome == "pending" && r.state.Entries["C1|100.1"].ReactionReceiptGeneration != 1 {
					t.Fatal("new pending receipt did not advance its generation")
				}
				if historyOnly {
					if err := os.Remove(r.path); err != nil {
						t.Fatal(err)
					}
				}
				fresh := coalescingTestConfig(srv.URL, time.Hour)
				fresh.cityPath, fresh.inboundDeadLetterDir = cfg.cityPath, cfg.inboundDeadLetterDir
				fresh.deliveryReceiptGate = true
				if err := installInboundRecovery(&fresh); err != nil {
					t.Fatal(err)
				}
				fresh.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
					return deliverCoalescedBatch(fresh, channel, batch)
				}
				t.Cleanup(func() { fresh.coalescer.flushAll() })
				reloaded := fresh.inboundRecovery
				now = now.Add(time.Minute)
				reloaded.now = func() time.Time { return now }
				reloaded.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
					if outcome != "pending" || id != "new-pending" {
						t.Fatalf("restart selected stale receipt %q for %s", id, outcome)
					}
					return receiptPollConcluded, recoveryReceipt(id, "s1", true), nil
				}
				reloaded.tick(true)
				if reloaded.state.Entries["C1|100.1"].DeliveredAt.IsZero() || len(fresh.coalescer.reactions["C1"]) != 0 {
					t.Fatal("restart resurrected completed reaction")
				}
				fresh.coalescer.enqueue("C1", testPending("C1", "100.3", "next real line"))
				fresh.coalescer.flushAheadOf("C1", "200.1")
				if posts.Load() != 1 {
					t.Fatalf("reaction replayed after restart: %d", posts.Load())
				}
				final, err := newInboundRecovery("", r.deadDir)
				if err != nil {
					t.Fatal(err)
				}
				if err := final.importDeadLetters(); err != nil || final.state.Entries["C1|100.1"].DeliveredAt.IsZero() {
					t.Fatalf("completion absent from history: %v", err)
				}
			})
		}
	}
}

func TestRound2ReactionHandoffRechecksOwnershipAfterChannelWait(t *testing.T) {
	for _, outcome := range []string{"queued", "pending", "delivered"} {
		t.Run(outcome, func(t *testing.T) {
			cfg := recoveryTestConfig(t, "http://unused")
			c, r := cfg.coalescer, cfg.inboundRecovery
			c.deliver = func(string, []pendingChannelInbound) error { return nil }
			now := time.Now()
			r.now = func() time.Time { return now }
			p := testPending("C1", "100.1", "reaction handoff")
			p.reaction = true
			// Initial admission has persisted the mirror but still owns the
			// channel lock while it finishes transferring the side copy.
			release := c.holdDelivery("C1")
			held := true
			defer func() {
				if held {
					release()
				}
			}()
			if err := r.holdEntry(inboundRecoveryEntry{Inbound: p.inbound, Reaction: true}, recoveryReceipt("initial", "s1", false)); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			finished := make(chan struct{})
			go func() { r.tick(false); close(finished) }()
			deadline := time.Now().Add(2 * time.Second)
			for {
				c.mu.Lock()
				waiting := c.urgentWaiting["C1"] > 0
				c.mu.Unlock()
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("recovery did not wait on channel ownership")
				}
				time.Sleep(time.Millisecond)
			}
			r.mu.Lock()
			e := r.state.Entries["C1|100.1"]
			r.mu.Unlock()
			if err := r.queueReaction("C1|100.1", e); err != nil {
				t.Fatal(err)
			}
			if outcome != "queued" {
				c.mu.Lock()
				c.reactions["C1"] = nil // the next delivery consumed the copy
				c.mu.Unlock()
				if outcome == "pending" {
					if err := r.hold(p, deliveryReceipt{present: true, id: "new", status: "pending", targetSessionID: "s1"}, false); err != nil {
						t.Fatal(err)
					}
				} else {
					r.completeBufferedReactions([]pendingChannelInbound{p}, recoveryReceipt("done", "s1", true))
				}
			}
			release()
			held = false
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("recovery did not finish its delayed handoff")
			}
			stored := r.state.Entries["C1|100.1"]
			if outcome == "queued" {
				if len(c.reactions["C1"]) != 1 {
					t.Fatal("delayed tick duplicated the queued reaction")
				}
			} else if len(c.reactions["C1"]) != 0 || (outcome == "pending" && (!stored.PendingReceipt || stored.PendingReceiptID != "new" || stored.ReactionReceiptGeneration != 1)) || (outcome == "delivered" && stored.DeliveredAt.IsZero()) {
				t.Fatalf("delayed tick replaced newer %s state: %+v", outcome, stored)
			}
		})
	}
}

func TestRound2ReactionOwnershipTransferWaitsForChannelDelivery(t *testing.T) {
	var delivered atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
		}
		if strings.Contains(string(body), "lost-reaction") {
			delivered.Add(1)
		}
		fmt.Fprint(w, `{"delivery":{"receipt_id":"done","status":"delivered","members":[{"session_id":"s1","status":"delivered"}]}}`)
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	r := cfg.inboundRecovery
	now := time.Now()
	r.now = func() time.Time { return now }
	p := testPending("C1", "100.1", "lost-reaction")
	p.reaction = true
	if err := r.hold(p, deliveryReceipt{present: true, id: "pending", status: "pending"}, false); err != nil {
		t.Fatal(err)
	}
	polled := make(chan struct{})
	r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
		close(polled)
		return receiptPollConcluded, recoveryReceipt("pending", "s1", false), nil
	}
	release := cfg.coalescer.holdDelivery("C1")
	held := true
	defer func() {
		if held {
			release()
		}
	}()
	now = now.Add(time.Minute)
	finished := make(chan struct{})
	go func() { r.tick(false); close(finished) }()
	select {
	case <-polled:
	case <-time.After(2 * time.Second):
		t.Fatal("receipt was not polled")
	}
	select {
	case <-finished:
		t.Fatal("ownership transfer ran during another channel delivery")
	case <-time.After(50 * time.Millisecond):
	}
	cfg.coalescer.mu.Lock()
	buffered := len(cfg.coalescer.reactions["C1"])
	cfg.coalescer.mu.Unlock()
	if buffered != 0 || !r.known("C1", "100.1") {
		t.Fatal("reaction exposed before the old recovery owner was cleared")
	}
	release()
	held = false
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("ownership transfer did not resume")
	}
	if !r.known("C1", "100.1") || r.deliveryBlocked("C1", "100.1") {
		t.Fatal("recovery mirror lost admission dedup or blocked its side-buffer copy")
	}
	cfg.coalescer.enqueue("C1", testPending("C1", "100.2", "fresh real line"))
	cfg.coalescer.flushAheadOf("C1", "200.1")
	if delivered.Load() != 1 {
		t.Fatalf("reaction lost or duplicated during handoff: %d", delivered.Load())
	}
}

func TestRound2PendingAliasLossRefreshesDestinationAndFIFO(t *testing.T) {
	r, now := round2Recovery(t)
	start := *now
	for _, ts := range []string{"100.1", "100.3"} {
		if err := r.hold(testPending("C1", ts, "new-session line"), recoveryReceipt("r", "new", false), false); err != nil {
			t.Fatal(err)
		}
	}
	msg := testPending("C1", "100.2", "moving alias").inbound
	pending := deliveryReceipt{present: true, id: "old-pending", status: "pending", targetSessionID: "old"}
	if err := r.holdAlias(msg, "old", "mayor", "body", pending); err != nil {
		t.Fatal(err)
	}
	r.resolveAlias = func(string) (string, bool) { return "new", true }
	polls := 0
	r.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		polls++
		if id != "old-pending" {
			t.Fatalf("polled replacement receipt: %s", id)
		}
		return receiptPollConcluded, recoveryReceipt(id, "old", false), nil
	}
	var sent []string
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		if e.AliasSessionID == "old" {
			t.Error("replacement sent to stale alias session")
			return deliveryReceipt{}, &inboundPostError{Status: 404}
		}
		sent = append(sent, e.Inbound.ProviderMessageID)
		return recoveryReceipt("delivered", "new", true), nil
	}
	*now = start.Add(5 * time.Second)
	r.tick(false)
	e := r.state.Entries["C1|100.2|alias:old"]
	if len(sent) != 0 || e.PendingReceipt || e.AliasSessionID != "new" || e.AliasOriginalSessionID != "old" || e.Attempts != 0 {
		t.Fatalf("poll did not preserve routing/FIFO: sent=%v entry=%+v", sent, e)
	}
	*now = start.Add(20 * time.Second)
	r.tick(false)
	if !reflect.DeepEqual(sent, []string{"100.1", "100.2", "100.3"}) || polls != 1 || r.state.Entries["C1|100.2|alias:old"].DeliveredAt.IsZero() {
		t.Fatalf("retarget failed or overtook FIFO: sent=%v polls=%d", sent, polls)
	}
}

func TestRound2UnreadableHistoryQuarantinesSnapshot(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "directory", "invalid-history-root"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := round2Recovery(t)
			round2Hold(t, r, "100.1")
			r.retire("C1|100.1", r.state.Entries["C1|100.1"])
			path := filepath.Join(r.deadDir, "C1.jsonl")
			if err := os.Rename(path, filepath.Join(t.TempDir(), "saved.jsonl")); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(r.path, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "invalid-history-root":
				if err := os.Remove(r.deadDir); err != nil {
					t.Fatal(err)
				}
				err = os.WriteFile(r.deadDir, []byte("unreadable directory"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := r.importDeadLetters(); err != nil {
				t.Fatal(err)
			}
			r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				t.Fatal("replayed snapshot without complete history")
				return deliveryReceipt{}, nil
			}
			r.manual["C1"] = true
			r.tick(true)
			reloaded, err := newInboundRecovery(r.path, r.deadDir)
			if err != nil || !strings.Contains(reloaded.state.Entries["C1|100.1"].ReplayBlockedReason, "incomplete history") {
				t.Fatalf("quarantine not durable: %v", err)
			}
		})
	}
}

func TestRound2IncompleteHistoryNeverReplaysHiddenCompletion(t *testing.T) {
	for _, mode := range []string{"no-snapshot", "damaged-snapshot", "snapshot"} {
		for _, completion := range []string{"delivered", "acknowledged"} {
			t.Run(mode+"/"+completion, func(t *testing.T) {
				r, now := round2Recovery(t)
				round2Hold(t, r, "100.1")
				e := r.state.Entries["C1|100.1"]
				r.retire("C1|100.1", e)
				path := filepath.Join(r.deadDir, "C1.jsonl")
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.Write(append(bytes.Repeat([]byte("x"), 9<<20), '\n'))
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("fixture write: %v %v", err, closeErr)
				}
				rec := inboundDeadLetterRecord{Inbound: e.Inbound, Channel: "C1", DeadLetteredAt: *now}
				if completion == "delivered" {
					rec.ReplayedAt = *now
				} else {
					rec.AcknowledgedAt = *now
				}
				if _, err := writeInboundDeadLetterRecords(r.deadDir, "C1", []inboundDeadLetterRecord{rec}); err != nil {
					t.Fatal(err)
				}
				if mode == "no-snapshot" {
					if err := os.Remove(r.path); err != nil {
						t.Fatal(err)
					}
				} else if mode == "damaged-snapshot" {
					if err := os.WriteFile(r.path, []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				rebuilt, err := newInboundRecovery(r.path, r.deadDir)
				if err != nil {
					t.Fatal(err)
				}
				rebuilt.now = r.now
				if err := rebuilt.importDeadLetters(); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(rebuilt.state.Entries["C1|100.1"].ReplayBlockedReason, "incomplete history") {
					t.Fatal("partial prefix remained replayable")
				}
				round2Hold(t, rebuilt, "100.2")
				var sent []string
				rebuilt.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
					sent = append(sent, e.Inbound.ProviderMessageID)
					return recoveryReceipt("new", "s1", true), nil
				}
				*now = now.Add(time.Minute)
				rebuilt.manual["C1"] = true
				rebuilt.tick(true)
				if !reflect.DeepEqual(sent, []string{"100.2"}) {
					t.Fatalf("duplicate replay or blocked fresh line: %v", sent)
				}
			})
		}
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
func TestRound2FollowerCannotShortenRearmedHistoryCooldown(t *testing.T) {
	for _, trigger := range []string{"startup", "vouch", "verb"} {
		for _, followerChannel := range []string{"C1", "C2"} {
			t.Run(trigger+"/"+followerChannel, func(t *testing.T) {
				r, now := round2Recovery(t)
				start := *now
				round2Hold(t, r, "100.1")
				e := r.state.Entries["C1|100.1"]
				e.DeadLetteredAt = start
				e.NextAttemptAt = time.Time{}
				r.put("C1|100.1", e)
				var sent []string
				var attempts []time.Time
				r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
					sent = append(sent, e.Inbound.ProviderMessageID)
					attempts = append(attempts, *now)
					if len(sent) == 1 {
						return deliveryReceipt{}, &inboundPostError{Status: 503, StatusText: "unavailable"}
					}
					return recoveryReceipt("done", "s1", true), nil
				}
				r.tick(true)
				deadline := r.state.Entries["C1|100.1"].ReplayNotBefore
				if !deadline.After(start.Add(20 * time.Second)) {
					t.Fatal("fixture cooldown does not cover follower rung")
				}
				if err := r.hold(testPending(followerChannel, "100.2", "fresh follower"), recoveryReceipt("follower", "s1", false), false); err != nil {
					t.Fatal(err)
				}
				*now = start.Add(time.Second)
				switch trigger {
				case "startup":
					r.tick(true)
				case "vouch":
					r.observe(recoveryReceipt("vouch", "s1", true))
					r.tick(false)
				case "verb":
					response := httptest.NewRecorder()
					r.handleRedrive(response, httptest.NewRequest(http.MethodPost, "/internal/inbound/redrive", strings.NewReader(`{"conversation_id":"C1"}`)))
					if response.Code != http.StatusAccepted {
						t.Fatal(response.Body.String())
					}
					r.tick(false)
				}
				*now = start.Add(20 * time.Second)
				r.tick(false)
				if len(sent) != 1 {
					t.Fatalf("follower bypassed replay cooldown: %v at %v", sent, attempts)
				}
				*now = deadline
				r.tick(false)
				if len(sent) < 2 || sent[1] != "100.1" || !attempts[1].Equal(deadline) {
					t.Fatalf("replay missed its actual deadline: %v at %v", sent, attempts)
				}
				*now = deadline.Add(time.Minute)
				r.tick(false)
				if !reflect.DeepEqual(sent, []string{"100.1", "100.1", "100.2"}) {
					t.Fatalf("follower did not flow after replay: %v", sent)
				}
			})
		}
	}
}

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

func TestRound2ExplicitOlderReplayPreviewAndEligibility(t *testing.T) {
	r, now := round2Recovery(t)
	for i, age := range []time.Duration{time.Hour, 30 * 24 * time.Hour} {
		_, err := writeInboundDeadLetterRecords(r.deadDir, "C1", []inboundDeadLetterRecord{{
			Inbound:        testPending("C1", fmt.Sprintf("100.%d", i), "history").inbound,
			DeadLetteredAt: now.Add(-age),
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := r.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	var sent []string
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		sent = append(sent, e.Inbound.ProviderMessageID)
		return recoveryReceipt("ok", "s1", true), nil
	}
	r.tick(true)
	if !reflect.DeepEqual(sent, []string{"100.0"}) {
		t.Fatalf("automatic startup replayed old history: %v", sent)
	}
	request := func(preview, older bool) map[string]any {
		t.Helper()
		body := fmt.Sprintf(`{"conversation_id":"C1","preview":%t,"include_older":%t}`, preview, older)
		response := httptest.NewRecorder()
		r.handleRedrive(response, httptest.NewRequest(http.MethodPost, "/internal/inbound/redrive", strings.NewReader(body)))
		want := http.StatusAccepted
		if preview {
			want = http.StatusOK
		}
		if response.Code != want {
			t.Fatalf("redrive response=%d %s", response.Code, response.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	bare := request(false, false)
	r.tick(false)
	if bare["skipped_for_age"] != float64(1) || bare["oldest_skipped_at"] != now.Add(-30*24*time.Hour).UTC().Format(time.RFC3339) || len(sent) != 1 {
		t.Fatalf("bare verb did not honor horizon: %v sent=%v", bare, sent)
	}
	preview := request(true, true)
	r.tick(false)
	if preview["replay_count"] != float64(1) || len(sent) != 1 {
		t.Fatalf("preview sent work or miscounted: %v sent=%v", preview, sent)
	}
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	queued := request(false, true)
	r.tick(false)
	if queued["replay_count"] != float64(1) || !reflect.DeepEqual(sent, []string{"100.0", "100.1"}) {
		t.Fatalf("explicit override failed: %v sent=%v", queued, sent)
	}
	if !strings.Contains(logs.String(), "verb channel=C1 include_older=true replay_count=1 skipped_for_age=0") {
		t.Fatalf("missing operator audit: %s", logs.String())
	}
}

func TestRound2AcknowledgementFencesChannelWait(t *testing.T) {
	for _, route := range []string{"channel", "alias", "retargeted-alias"} {
		t.Run(route, func(t *testing.T) {
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				posts.Add(1)
				fmt.Fprint(w, `{"delivery":{"receipt_id":"unexpected","status":"delivered","members":[{"session_id":"s1","status":"delivered"}]}}`)
			}))
			defer server.Close()
			cfg := recoveryTestConfig(t, server.URL)
			r, c := cfg.inboundRecovery, cfg.coalescer
			now := time.Now()
			r.now = func() time.Time { return now }
			key := "C1|100.1"
			p := testPending("C1", "100.1", "acknowledge before channel unlock")
			if route == "channel" {
				if err := r.hold(p, recoveryReceipt("old", "s1", false), false); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := r.holdAlias(p.inbound, "s1", "mayor", "body", recoveryReceipt("old", "s1", false)); err != nil {
					t.Fatal(err)
				}
				key += "|alias:s1"
				if route == "retargeted-alias" {
					e := r.state.Entries[key]
					e.AliasSessionID, e.AliasOriginalSessionID = "s2", "s1"
					r.put(key, e)
				}
			}
			release := c.holdDelivery("C1")
			held := true
			defer func() {
				if held {
					release()
				}
			}()
			now = now.Add(time.Minute)
			tickDone := make(chan struct{})
			go func() { r.tick(false); close(tickDone) }()
			deadline := time.Now().Add(2 * time.Second)
			for {
				c.mu.Lock()
				waiting := c.urgentWaiting["C1"] > 0
				c.mu.Unlock()
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("recovery did not wait for channel ownership")
				}
				time.Sleep(time.Millisecond)
			}
			if n, err := r.acknowledge("C1", "100.1"); err != nil || n != 1 {
				t.Fatalf("acknowledgement=%d: %v", n, err)
			}
			release()
			held = false
			select {
			case <-tickDone:
			case <-time.After(2 * time.Second):
				t.Fatal("recovery did not finish after channel unlock")
			}
			if posts.Load() != 0 || r.state.Entries[key].AcknowledgedAt.IsZero() {
				t.Fatalf("acknowledged %s replayed after channel wait: posts=%d", route, posts.Load())
			}
		})
	}
}

func TestRound2AcknowledgementDoesNotInvertHistoryLock(t *testing.T) {
	r, now := round2Recovery(t)
	round2Hold(t, r, "100.1")
	selected := make(chan struct{})
	r.now = func() time.Time {
		close(selected)
		return *now
	}
	inboundDeadLetterWriteMu.Lock()
	writerHeld := true
	defer func() {
		if writerHeld {
			inboundDeadLetterWriteMu.Unlock()
		}
	}()
	ackDone := make(chan error, 1)
	go func() {
		_, err := r.acknowledge("C1", "100.1")
		ackDone <- err
	}()
	<-selected // acknowledgement selected its entry and now needs history I/O
	stateAvailable := make(chan struct{})
	go func() {
		r.mu.Lock()
		// An actual receipt outcome may arrive while acknowledgement waits
		// for the history writer. It must survive the acknowledgement merge.
		e := r.state.Entries["C1|100.1"]
		e.PendingReceipt, e.PendingReceiptID = true, "newer-receipt"
		r.state.Entries["C1|100.1"] = e
		r.mu.Unlock()
		close(stateAvailable)
	}()
	select {
	case <-stateAvailable:
	case <-time.After(2 * time.Second):
		inboundDeadLetterWriteMu.Unlock()
		writerHeld = false
		<-ackDone
		<-stateAvailable
		t.Fatal("acknowledgement held state while waiting for the history writer")
	}
	importDone := make(chan error, 1)
	go func() { importDone <- r.importDeadLetters() }()
	inboundDeadLetterWriteMu.Unlock()
	writerHeld = false
	for _, done := range []<-chan error{ackDone, importDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent acknowledgement/history import deadlocked")
		}
	}
	e := r.state.Entries["C1|100.1"]
	if e.AcknowledgedAt.IsZero() || !e.PendingReceipt || e.PendingReceiptID != "newer-receipt" {
		t.Fatalf("acknowledgement replaced the newer receipt outcome: %+v", e)
	}
}

func TestRound2AcknowledgementRemainsResponsiveDuringRecovery(t *testing.T) {
	for _, phase := range []string{"failed-post", "pending-post", "delivered-post", "failed-poll", "unrelated-post"} {
		t.Run(phase, func(t *testing.T) {
			r, now := round2Recovery(t)
			if phase == "failed-poll" {
				if err := r.hold(testPending("C1", "100.1", "pending"), deliveryReceipt{present: true, id: "inflight", status: "pending", targetSessionID: "s1"}, false); err != nil {
					t.Fatal(err)
				}
			} else {
				round2Hold(t, r, "100.1")
			}
			channel, ts := "C1", "100.1"
			if phase == "unrelated-post" {
				channel, ts = "C2", "100.2"
				if err := r.hold(testPending(channel, ts, "acknowledge queued line"), recoveryReceipt("queued", "s2", false), false); err != nil {
					t.Fatal(err)
				}
			}
			*now = now.Add(time.Minute)
			entered, release, tickDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			posts := 0
			r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
				posts++
				if phase == "failed-poll" || e.Inbound.ProviderMessageID != "100.1" {
					t.Error("acknowledged line was sent")
					return recoveryReceipt("unexpected", "s1", true), nil
				}
				close(entered)
				<-release
				if phase == "pending-post" {
					return deliveryReceipt{present: true, id: "new-pending", status: "pending", targetSessionID: "s1"}, nil
				}
				return recoveryReceipt("outcome", "s1", phase == "delivered-post"), nil
			}
			r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
				close(entered)
				<-release
				return receiptPollConcluded, recoveryReceipt("inflight", "s1", false), nil
			}
			go func() { r.tick(false); close(tickDone) }()
			<-entered
			response, requestDone := httptest.NewRecorder(), make(chan struct{})
			go func() {
				body := fmt.Sprintf(`{"conversation_id":%q,"acknowledge":%q}`, channel, ts)
				r.handleRedrive(response, httptest.NewRequest(http.MethodPost, "/internal/inbound/redrive", strings.NewReader(body)))
				close(requestDone)
			}()
			select {
			case <-requestDone:
			case <-time.After(2 * time.Second):
				close(release)
				<-tickDone
				<-requestDone
				t.Fatal("acknowledgement waited behind recovery I/O")
			}
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"acknowledged":1`) {
				t.Fatal(response.Body.String())
			}
			key := channel + "|" + ts
			persisted, err := newInboundRecovery(r.path, r.deadDir)
			if err != nil || persisted.state.Entries[key].AcknowledgedAt.IsZero() {
				t.Fatalf("response preceded durable acknowledgement: %v", err)
			}
			close(release)
			<-tickDone
			e := r.state.Entries[key]
			if e.AcknowledgedAt.IsZero() || (phase == "delivered-post" && e.DeliveredAt.IsZero()) || (phase == "pending-post" && e.PendingReceiptID != "new-pending") {
				t.Fatalf("in-flight outcome overwrote acknowledgement or receipt: %+v", e)
			}
			if phase == "failed-poll" && posts != 0 {
				t.Fatal("loss poll reposted an acknowledged line")
			}
			if err := os.Remove(r.path); err != nil {
				t.Fatal(err)
			}
			history, err := newInboundRecovery(r.path, r.deadDir)
			if err == nil {
				err = history.importDeadLetters()
			}
			if err != nil || history.state.Entries[key].AcknowledgedAt.IsZero() {
				t.Fatalf("history-only restart lost acknowledgement: %v", err)
			}
		})
	}
}

func TestRound2RedriveRemainsResponsiveDuringInflightRetry(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%v", pending), func(t *testing.T) {
			r, now := round2Recovery(t)
			round2Hold(t, r, "100.1")
			e := r.state.Entries["C1|100.1"]
			e.DeadLetteredAt = *now
			e.NextAttemptAt = time.Time{}
			r.put("C1|100.1", e)
			entered, release, tickDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				close(entered)
				<-release
				if pending {
					return deliveryReceipt{present: true, id: "inflight", status: "pending", targetSessionID: "s1"}, nil
				}
				return deliveryReceipt{}, &inboundPostError{Status: 503, StatusText: "unavailable"}
			}
			go func() { r.tick(true); close(tickDone) }()
			<-entered
			request := func(preview bool) *httptest.ResponseRecorder {
				t.Helper()
				response := httptest.NewRecorder()
				requestDone := make(chan struct{})
				go func() {
					body := fmt.Sprintf(`{"conversation_id":"C1","include_older":true,"preview":%v}`, preview)
					r.handleRedrive(response, httptest.NewRequest(http.MethodPost, "/internal/inbound/redrive", strings.NewReader(body)))
					close(requestDone)
				}()
				select {
				case <-requestDone:
				case <-time.After(2 * time.Second):
					close(release)
					<-tickDone
					<-requestDone
					t.Fatal("admin request waited behind network recovery")
				}
				return response
			}
			if preview := request(true); preview.Code != http.StatusOK {
				t.Fatal(preview.Body.String())
			}
			response := request(false)
			close(release)
			<-tickDone
			r.tick(false) // consume the manual trigger while the receipt is pending
			if response.Code != http.StatusAccepted {
				t.Fatal(response.Body.String())
			}
			var result map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			restarted, err := newInboundRecovery(r.path, r.deadDir)
			if err != nil {
				t.Fatal(err)
			}
			stored := restarted.state.Entries["C1|100.1"]
			if result["replay_count"] != float64(1) || !stored.ReplayIncludeOlder || stored.ReplayRequestGeneration != 1 || stored.NextAttemptAt.IsZero() {
				t.Fatalf("accepted override lost across restart: result=%v entry=%+v", result, stored)
			}
			if pending && (!stored.PendingReceipt || stored.PendingReceiptID != "inflight") {
				t.Fatal("new queue intent replaced its in-flight receipt")
			}
			*now = now.Add(48 * time.Hour)
			restarted.now = func() time.Time { return *now }
			restarted.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
				return receiptPollConcluded, recoveryReceipt("inflight", "s1", false), nil
			}
			sent := 0
			restarted.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				sent++
				return recoveryReceipt("done", "s1", true), nil
			}
			restarted.tick(true)
			if sent != 1 || restarted.state.Entries["C1|100.1"].DeliveredAt.IsZero() {
				t.Fatal("accepted override expired with the older in-flight attempt")
			}
		})
	}
}

func TestRound2OlderReplayQueueSurvivesRestartBehindPendingHead(t *testing.T) {
	for _, processQueue := range []bool{false, true} {
		t.Run(fmt.Sprintf("tick=%v", processQueue), func(t *testing.T) {
			r, now := round2Recovery(t)
			pending := deliveryReceipt{present: true, id: "head", status: "pending", targetSessionID: "s1"}
			if err := r.hold(testPending("C1", "100.0", "pending head"), pending, false); err != nil {
				t.Fatal(err)
			}
			round2Hold(t, r, "100.1")
			e := r.state.Entries["C1|100.1"]
			e.DeadLetteredAt = now.Add(-30 * 24 * time.Hour)
			e.NextAttemptAt = time.Time{}
			r.put("C1|100.1", e)
			r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
				return receiptPollPending, pending, nil
			}
			r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				t.Fatal("older replay overtook pending head")
				return deliveryReceipt{}, nil
			}
			response := httptest.NewRecorder()
			r.handleRedrive(response, httptest.NewRequest(http.MethodPost, "/internal/inbound/redrive", strings.NewReader(`{"conversation_id":"C1","include_older":true}`)))
			if response.Code != http.StatusAccepted {
				t.Fatal(response.Body.String())
			}
			if processQueue {
				r.tick(false)
			}
			restarted, err := newInboundRecovery(r.path, r.deadDir)
			if err != nil {
				t.Fatal(err)
			}
			queued := restarted.state.Entries["C1|100.1"]
			if !queued.ReplayIncludeOlder || queued.NextAttemptAt.IsZero() {
				t.Fatal("accepted older replay lost its queued permission")
			}
			*now = now.Add(time.Minute)
			restarted.now = func() time.Time { return *now }
			restarted.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
				return receiptPollConcluded, recoveryReceipt("head", "s1", true), nil
			}
			var sent []string
			restarted.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
				sent = append(sent, e.Inbound.ProviderMessageID)
				return recoveryReceipt("replayed", "s1", true), nil
			}
			restarted.tick(true)
			if !reflect.DeepEqual(sent, []string{"100.1"}) {
				t.Fatalf("accepted replay did not follow completed head: %v", sent)
			}
		})
	}
}

func TestRound2OlderReplayPermissionSurvivesOnlyItsPendingAttempt(t *testing.T) {
	r, now := round2Recovery(t)
	round2Hold(t, r, "100.1")
	e := r.state.Entries["C1|100.1"]
	e.DeadLetteredAt = now.Add(-30 * 24 * time.Hour)
	e.NextAttemptAt = time.Time{}
	r.put("C1|100.1", e)
	posts := 0
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		posts++
		return deliveryReceipt{present: true, id: "pending", status: "pending"}, nil
	}
	response := httptest.NewRecorder()
	r.handleRedrive(response, httptest.NewRequest(http.MethodPost, "/internal/inbound/redrive", strings.NewReader(`{"conversation_id":"C1","include_older":true}`)))
	if response.Code != http.StatusAccepted {
		t.Fatal(response.Body.String())
	}
	r.tick(false)
	if posts != 1 {
		t.Fatalf("initial replay posts=%d", posts)
	}
	restarted, err := newInboundRecovery(r.path, r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	restarted.now = func() time.Time { return *now }
	restarted.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
		return receiptPollConcluded, recoveryReceipt("pending", "s1", false), nil
	}
	restarted.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		posts++
		return deliveryReceipt{}, &inboundPostError{Status: 503, StatusText: "503 unavailable"}
	}
	restarted.tick(true)
	if posts != 2 || restarted.state.Entries["C1|100.1"].ReplayIncludeOlder {
		t.Fatalf("permission not consumed after concluded failure: posts=%d", posts)
	}
	*now = now.Add(time.Hour)
	restarted.tick(true)
	if posts != 2 {
		t.Fatal("later startup reused an expired manual override")
	}
}

func TestRound2FailedRetirementNoticeNeverClaimsDelivery(t *testing.T) {
	r, now := round2Recovery(t)
	if err := os.WriteFile(r.deadDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	round2Hold(t, r, "100.1")
	var texts []string
	r.notice = func(_, _, _, text string) (string, error) {
		texts = append(texts, text)
		return "n", nil
	}
	*now = now.Add(time.Minute)
	r.updateNotices()
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		return deliveryReceipt{}, &inboundPostError{Status: 422, StatusText: "422 refused"}
	}
	r.tick(false)
	e := r.state.Entries["C1|100.1"]
	if !e.Retiring || !e.DeliveredAt.IsZero() {
		t.Fatalf("retirement ownership lost: %+v", e)
	}
	if len(texts) != 2 || !strings.Contains(texts[1], "unconfirmed") || strings.Contains(texts[1], "handed to") || strings.Contains(texts[1], "0001-") {
		t.Fatalf("failed retirement claimed delivery: %v", texts)
	}
	// A delivered sibling does not prove that the whole incident recovered.
	sibling := e
	sibling.Inbound.ProviderMessageID = "100.2"
	sibling.DeliveredAt = *now
	sibling.Retiring = false
	sibling.Reason = "terminal handoff confirmed"
	r.put("C1|100.2", sibling)
	notice := r.state.Notices["C1"]
	notice.Resolved = false
	r.saveNotice("C1", notice)
	r.updateNotices()
	if len(texts) != 3 || !strings.Contains(texts[2], "unconfirmed") || strings.Contains(texts[2], "handed to") {
		t.Fatalf("delivered sibling hid the failed retirement: %v", texts)
	}
}
