package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func round2Recovery(t *testing.T) (*inboundRecovery, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 9, 20, 42, 22, 0, time.UTC)
	dir := t.TempDir()
	r, err := newInboundRecovery(filepath.Join(dir, "state.json"), filepath.Join(dir, "dead"))
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return now }
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		return recoveryReceipt("failed", "s1", false), nil
	}
	return r, &now
}

func round2Hold(t *testing.T, r *inboundRecovery, ts string) {
	t.Helper()
	if err := r.hold(testPending("C1", ts, "founder line"), recoveryReceipt("original", "s1", false), false); err != nil {
		t.Fatal(err)
	}
}

// P1: a vouch is a release signal, rate-limited to one induced send/minute.
func TestRound2VouchReleasesHeld(t *testing.T) {
	r, now := round2Recovery(t)
	start := *now
	round2Hold(t, r, "100.1")
	posts := 0
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		posts++
		return recoveryReceipt("new", "s1", now.Sub(start) >= 19*time.Minute), nil
	}
	for second := 1; second < 19*60; second++ {
		*now = start.Add(time.Duration(second) * time.Second)
		r.tick(false)
	}
	*now = start.Add(19 * time.Minute)
	before := posts
	r.observe(recoveryReceipt("fresh", "s1", true))
	r.tick(false)
	if posts != before+1 || r.state.Entries["C1|100.1"].DeliveredAt.IsZero() {
		t.Fatalf("vouch failed: posts=%d state=%+v", posts, r.state.Entries["C1|100.1"])
	}
	round2Hold(t, r, "100.2")
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		posts++
		return recoveryReceipt("still", "s1", false), nil
	}
	r.observe(recoveryReceipt("fresh", "s1", true))
	r.tick(false)
	before = posts
	for i := 0; i < 10; i++ {
		r.observe(recoveryReceipt("fresh", "s1", true))
		r.tick(false)
	}
	if posts != before {
		t.Fatal("vouch storm bypassed one-minute limit")
	}
}

// P12/P14: follower rungs wake the FIFO head, preserving the incident's order.
func TestRound2IncidentFollowerRungs(t *testing.T) {
	r, now := round2Recovery(t)
	start := *now
	round2Hold(t, r, "100.1")
	var attempts, delivered []string
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		attempts = append(attempts, fmt.Sprintf("%s@%s", e.Inbound.ProviderMessageID, now.Sub(start)))
		ok := now.Sub(start) >= 20*time.Minute+46*time.Second
		if ok {
			delivered = append(delivered, e.Inbound.ProviderMessageID)
			if strings.Contains(e.Inbound.Text, "delivered late") {
				t.Fatal("unnecessary dead letter")
			}
		}
		return recoveryReceipt("retry", "s1", ok), nil
	}
	for second := 1; second <= 21*60+20; second++ {
		*now = start.Add(time.Duration(second) * time.Second)
		if second == 20*60 {
			round2Hold(t, r, "100.2")
		}
		r.tick(false)
	}
	if !reflect.DeepEqual(delivered, []string{"100.1", "100.2"}) {
		t.Fatalf("delivered=%v attempts=%v", delivered, attempts)
	}
	if !strings.Contains(strings.Join(attempts, ","), "100.1@20m20s") || !strings.Contains(strings.Join(attempts, ","), "100.1@21m20s") {
		t.Fatalf("follower did not wake head: %v", attempts)
	}
	if r.counts()["C1"].Dead != 0 {
		t.Fatal("incident dead-lettered")
	}
}

func TestRound2BoundMakesLastAttempt(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			r, now := round2Recovery(t)
			start := *now
			round2Hold(t, r, "100.1")
			e := r.state.Entries["C1|100.1"]
			e.PendingReceipt = pending
			e.PendingReceiptID = "pending"
			e.NextPollAt = start.Add(time.Hour)
			r.put("C1|100.1", e)
			calls := 0
			r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				calls++
				return recoveryReceipt("last", "s1", true), nil
			}
			r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
				calls++
				return receiptPollConcluded, recoveryReceipt("last", "s1", true), nil
			}
			*now = start.Add(r.bound)
			r.tick(false)
			if calls != 1 || r.state.Entries["C1|100.1"].DeliveredAt.IsZero() || r.counts()["C1"].Dead != 0 {
				t.Fatalf("last attempt missing: calls=%d counts=%v", calls, r.counts())
			}
		})
	}
}

// P2/P3: unpollable and refused history cannot stop a later founder line.
func TestRound2UnsettleableHeadsDoNotBlock(t *testing.T) {
	for _, reason := range []string{errReceiptFollowUpStuck.Error(), errReceiptFollowUpLost.Error(), "422 Unprocessable Entity: bad"} {
		t.Run(reason, func(t *testing.T) {
			r, now := round2Recovery(t)
			_, err := writeInboundDeadLetterRecords(r.deadDir, "C1", []inboundDeadLetterRecord{{Inbound: testPending("C1", "99.1", "old").inbound, DeadLetteredAt: now.Add(-3 * time.Hour), Reason: reason}})
			if err != nil {
				t.Fatal(err)
			}
			if err = r.importDeadLetters(); err != nil {
				t.Fatal(err)
			}
			round2Hold(t, r, "100.1")
			*now = now.Add(20 * time.Second)
			var sent []string
			r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
				sent = append(sent, e.Inbound.ProviderMessageID)
				return recoveryReceipt("ok", "s1", true), nil
			}
			r.tick(true)
			for i := 0; i < 3; i++ {
				r.manual["C1"] = true
				r.observe(recoveryReceipt("vouch", "s1", true))
				r.tick(false)
			}
			if !reflect.DeepEqual(sent, []string{"100.1"}) || r.counts()["C1"].ByHand != 1 {
				t.Fatalf("sent=%v counts=%v", sent, r.counts())
			}
		})
	}
}

func TestRound2AliasRefusalAndTransientBackoff(t *testing.T) {
	for _, status := range []int{404, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			r, now := round2Recovery(t)
			for _, ts := range []string{"100.1", "100.2"} {
				round2Hold(t, r, ts)
				e := r.state.Entries["C1|"+ts]
				e.DeadLetteredAt = *now
				e.NextAttemptAt = time.Time{}
				if ts == "100.1" {
					e.AliasSessionID = "s1"
				}
				r.put("C1|"+ts, e)
			}
			var sent []string
			r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
				sent = append(sent, e.Inbound.ProviderMessageID)
				if e.AliasSessionID != "" {
					return deliveryReceipt{}, &inboundPostError{Status: status, StatusText: fmt.Sprint(status)}
				}
				return recoveryReceipt("ok", "s1", true), nil
			}
			r.tick(true)
			if status == 503 {
				if len(sent) != 1 {
					t.Fatal("transient head overtaken immediately")
				}
				*now = now.Add(time.Minute)
				r.tick(false)
			}
			if !reflect.DeepEqual(sent, []string{"100.1", "100.2"}) {
				t.Fatalf("blocked follower: %v", sent)
			}
			if status == 404 {
				r.manual["C1"] = true
				r.tick(false)
				if len(sent) != 2 {
					t.Fatal("reposted refused alias")
				}
			}
		})
	}
}

// P4/P8: fast receipt polling neither raises a false notice nor retires a landed line.
func TestRound2PendingClockAndNoFalseNotice(t *testing.T) {
	r, now := round2Recovery(t)
	start := *now
	receipt := deliveryReceipt{present: true, id: "p", status: "pending", targetSessionID: "s1"}
	if err := r.hold(testPending("C1", "100.1", "busy"), receipt, false); err != nil {
		t.Fatal(err)
	}
	posts, notices, polls := 0, 0, 0
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { posts++; return receipt, nil }
	r.notice = func(string, string, string, string) (string, error) { notices++; return "n", nil }
	r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
		polls++
		if now.Sub(start) >= 30*time.Second {
			return receiptPollConcluded, recoveryReceipt("p", "s1", true), nil
		}
		return receiptPollPending, receipt, nil
	}
	for sec := 1; sec <= 80; sec++ {
		*now = start.Add(time.Duration(sec) * time.Second)
		r.tick(false)
	}
	if posts != 0 || notices != 0 || polls != 6 || r.state.Entries["C1|100.1"].DeliveredAt.After(start.Add(35*time.Second)) {
		t.Fatalf("posts=%d notices=%d polls=%d", posts, notices, polls)
	}
	if r.state.Entries["C1|100.1"].Attempts != 0 {
		t.Fatal("polls charged as reposts")
	}
}

// P7: an old retry counter must not slow a newly accepted replay receipt.
func TestRound2PendingReplayDrainsWithinMinute(t *testing.T) {
	r, now := round2Recovery(t)
	start := *now
	for i := 1; i <= 3; i++ {
		ts := fmt.Sprintf("100.%d", i)
		round2Hold(t, r, ts)
		e := r.state.Entries["C1|"+ts]
		e.DeadLetteredAt = *now
		e.Attempts = 5
		e.RetryStep = 5
		e.NextAttemptAt = time.Time{}
		r.put("C1|"+ts, e)
	}
	var sent []string
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		sent = append(sent, e.Inbound.ProviderMessageID)
		return deliveryReceipt{present: true, id: e.Inbound.ProviderMessageID, status: "pending", targetSessionID: "s1"}, nil
	}
	r.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		return receiptPollConcluded, recoveryReceipt(id, "s1", true), nil
	}
	r.observe(recoveryReceipt("vouch", "s1", true))
	for sec := 0; sec < 60; sec++ {
		*now = start.Add(time.Duration(sec) * time.Second)
		r.tick(false)
	}
	if !reflect.DeepEqual(sent, []string{"100.1", "100.2", "100.3"}) || r.counts()["C1"].Dead != 0 {
		t.Fatalf("sent=%v counts=%v", sent, r.counts())
	}
}

// P5/P10: failed posts have persistent backoff, incidents end, later holds get a notice.
func TestRound2NoticeBackoffAndIncidentEnd(t *testing.T) {
	r, now := round2Recovery(t)
	start := *now
	round2Hold(t, r, "100.1")
	calls := 0
	r.notice = func(string, string, string, string) (string, error) { calls++; return "", errors.New("rate_limited") }
	for sec := 0; sec <= 1800; sec++ {
		*now = start.Add(time.Duration(sec) * time.Second)
		r.tick(false)
	}
	if calls > 7 {
		t.Fatalf("unbounded notice failures: %d", calls)
	}
	reloaded, err := newInboundRecovery(r.path, r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.state.Notices["C1"].Failures == 0 {
		t.Fatal("backoff not persisted")
	}
	*now = now.Add(6 * time.Hour)
	round2Hold(t, r, "100.2")
	var texts []string
	r.notice = func(_, _, ts, text string) (string, error) { texts = append(texts, ts+"|"+text); return "notice", nil }
	*now = now.Add(time.Minute)
	r.tick(false)
	*now = now.Add(r.bound)
	r.tick(false)
	if len(texts) != 2 || !strings.Contains(texts[1], "not confirmed within") || !r.state.Notices["C1"].Resolved {
		t.Fatalf("incident did not end: %v", texts)
	}
	for _, code := range []string{"thread_not_found", "channel_not_found", "not_in_channel", "is_archived", "message_not_found", "cant_update_message", "edit_window_closed"} {
		if !permanentNoticeError(errors.New(code)) {
			t.Fatal(code)
		}
	}
}

// P6: startup and the bare verb cannot replay an old record.
func TestRound2ReplayAgeAndNoImportedNotice(t *testing.T) {
	r, now := round2Recovery(t)
	for i, age := range []time.Duration{time.Hour, 30 * 24 * time.Hour} {
		_, err := writeInboundDeadLetterRecords(r.deadDir, "C1", []inboundDeadLetterRecord{{Inbound: testPending("C1", fmt.Sprintf("100.%d", i), "history").inbound, DeadLetteredAt: now.Add(-age)}})
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
		return recoveryReceipt("done", "s1", true), nil
	}
	r.notice = func(string, string, string, string) (string, error) {
		t.Error("imported history opened notice")
		return "", nil
	}
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	r.tick(true)
	r.manual["C1"] = true
	r.tick(false)
	if !reflect.DeepEqual(sent, []string{"100.0"}) || r.counts()["C1"].Dead != 1 || !strings.Contains(logs.String(), "left-unreplayed-for-age=1") {
		t.Fatalf("sent=%v counts=%v log=%s", sent, r.counts(), logs.String())
	}
}

// P9: one human line, two destinations, one cumulative miss; acknowledgement is durable.
func TestRound2CountsAndAcknowledgement(t *testing.T) {
	r, now := round2Recovery(t)
	round2Hold(t, r, "100.1")
	if err := r.holdAlias(testPending("C1", "100.1", "alias").inbound, "s2", "mayor", "body", recoveryReceipt("a", "s2", false)); err != nil {
		t.Fatal(err)
	}
	if r.counts()["C1"].Held != 1 || r.newlyMissed() != 1 || r.newlyMissed() != 0 {
		t.Fatal("standing copy counted twice")
	}
	*now = now.Add(r.bound)
	r.tick(false)
	if n, err := r.acknowledge("C1", "100.1"); err != nil || n != 2 {
		t.Fatalf("ack=%d %v", n, err)
	}
	if r.counts()["C1"] != (inboundRecoveryCounts{}) {
		t.Fatal("ack remains counted")
	}
	fromHistory, err := newInboundRecovery("", r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = fromHistory.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	if fromHistory.counts()["C1"] != (inboundRecoveryCounts{}) {
		t.Fatal("history lost ack")
	}
}

// P11: reactions only ride real deliveries and never anchor a notice.
func TestRound2ReactionsReturnToSideLane(t *testing.T) {
	cfg := recoveryTestConfig(t, "http://unused")
	r := cfg.inboundRecovery
	now := time.Now()
	r.now = func() time.Time { return now }
	p := testPending("C1", "100.1", "reaction")
	p.reaction = true
	cfg.coalescer.charge("C1", p, &unvouchedDeliveryError{receipt: recoveryReceipt("bad", "s1", false)})
	if r.known("C1", "100.1") {
		t.Fatal("unvouched reaction entered timed ladder")
	}
	if len(cfg.coalescer.reactions["C1"]) != 1 {
		t.Fatal("reaction did not return to no-wake side buffer")
	}
	receipt := deliveryReceipt{present: true, id: "pending", status: "pending"}
	if err := r.hold(p, receipt, false); err != nil {
		t.Fatal(err)
	}
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		t.Fatal("solo reaction POST")
		return deliveryReceipt{}, nil
	}
	r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
		return receiptPollConcluded, recoveryReceipt("pending", "s1", false), nil
	}
	now = now.Add(time.Minute)
	r.tick(false)
	if r.known("C1", "100.1") || r.counts()["C1"] != (inboundRecoveryCounts{}) {
		t.Fatal("lost reaction still timed or counted")
	}
	round2Hold(t, r, "100.2")
	now = now.Add(time.Minute)
	r.notice = func(_, thread, _, _ string) (string, error) {
		if thread != "100.2" {
			t.Fatalf("anchor=%s", thread)
		}
		return "n", nil
	}
	r.updateNotices()
}

func TestRound2NoticeConfig(t *testing.T) {
	for _, s := range []string{"", "on", "1", "true", "yes", " ON "} {
		if on, err := parseHoldNotice(s); err != nil || !on {
			t.Fatalf("%q: %t %v", s, on, err)
		}
	}
	for _, s := range []string{"off", "0", "false", "no", " Off "} {
		if on, err := parseHoldNotice(s); err != nil || on {
			t.Fatalf("%q: %t %v", s, on, err)
		}
	}
	if _, err := parseHoldNotice("perhaps"); err == nil {
		t.Fatal("invalid option accepted")
	}
}

func TestRound2DamagedStateAndHistoryDoNotStopStartup(t *testing.T) {
	dir := t.TempDir()
	dead := filepath.Join(dir, "dead")
	state := filepath.Join(dir, "state.json")
	if err := os.WriteFile(state, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := writeInboundDeadLetterRecords(dead, "good", []inboundDeadLetterRecord{{Inbound: testPending("good", "100.1", "good").inbound, DeadLetteredAt: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(state, filepath.Join(dead, "symlink.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Mkfifo(filepath.Join(dead, "fifo.jsonl"), 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	r, err := newInboundRecovery(state, dead)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	if len(r.state.Entries) != 1 || !strings.Contains(logs.String(), "symlink.jsonl") || !strings.Contains(logs.String(), "fifo.jsonl") || !strings.Contains(logs.String(), "LOSS unreadable state") {
		t.Fatalf("entries=%d log=%s", len(r.state.Entries), logs.String())
	}
}

func TestRound2FIFOIsPerSession(t *testing.T) {
	r, now := round2Recovery(t)
	round2Hold(t, r, "100.1")
	if err := r.holdAlias(testPending("C1", "100.1", "alias").inbound, "s2", "other", "body", recoveryReceipt("a", "s2", false)); err != nil {
		t.Fatal(err)
	}
	var sessions []string
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		sessions = append(sessions, e.TargetSessions...)
		return recoveryReceipt("r", e.TargetSessions[0], e.AliasSessionID != ""), nil
	}
	*now = now.Add(20 * time.Second)
	r.tick(false)
	if !reflect.DeepEqual(sessions, []string{"s1", "s2"}) {
		t.Fatalf("unrelated session blocked: %v", sessions)
	}
}

func TestRound2NoticeIsNotOwnThreadEvidence(t *testing.T) {
	r, _ := round2Recovery(t)
	r.state.Notices["C1"] = inboundHoldNotice{TS: "100.2", PreviousTS: []string{"100.1"}}
	ignore := func(ts string) bool { return r.isNotice("C1", ts) }
	posts := []slackThreadMessage{{TS: "100.1", User: "bot"}, {TS: "100.2", User: "bot"}}
	if threadHasOwnBotPost(posts, "bot", "100.3", ignore) {
		t.Fatal("notice made own thread")
	}
	posts = append(posts, slackThreadMessage{TS: "100.0", User: "bot"})
	if !threadHasOwnBotPost(posts, "bot", "100.3", ignore) {
		t.Fatal("real bot reply ignored")
	}
}

func TestRound2TornJournalKeepsGoodTargets(t *testing.T) {
	r, _ := round2Recovery(t)
	for _, ts := range []string{"100.1", "100.2"} {
		round2Hold(t, r, ts)
		e := r.state.Entries["C1|"+ts]
		e.TargetSessions = nil
		e.OriginalReceiptID = ""
		r.put("C1|"+ts, e)
	}
	dir := t.TempDir()
	var data []byte
	for _, ts := range []string{"100.1", "100.2"} {
		row, _ := json.Marshal(inboundJournalRecord{Channel: "C1", MessageTS: ts, ReceiptID: "r", TargetSessions: []string{"s1"}, Phase: "accepted"})
		data = append(data, row...)
		data = append(data, []byte("\n{torn\n")...)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.enrichFromJournal(dir); err != nil {
		t.Fatal(err)
	}
	for _, e := range r.state.Entries {
		if !reflect.DeepEqual(e.TargetSessions, []string{"s1"}) || e.OriginalReceiptID != "r" {
			t.Fatalf("good provenance lost: %+v", e)
		}
	}
}

func TestRound2AliasReplayRoutingAndRetarget(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				paths = append(paths, req.URL.Path)
				fmt.Fprint(w, `{"delivery":{"receipt_id":"a","status":"delivered"}}`)
			}))
			defer srv.Close()
			cfg := recoveryTestConfig(t, srv.URL)
			cfg.mentionOnly, _ = newMentionOnlyRegistry("")
			cfg.mentionOnlyDeliveries = newMentionOnlyDeliveryLog()
			cfg.handleAliases = newTestHandleAliasRegistry(t)
			binding := mentionOnlyBinding{SessionID: "s1", SessionName: "mayor", Handle: "mayor"}
			if err := cfg.mentionOnly.Set("C1", binding); err != nil {
				t.Fatal(err)
			}
			if err := cfg.handleAliases.Set("mayor", "s1"); err != nil {
				t.Fatal(err)
			}
			if err := installInboundRecovery(&cfg); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			r := cfg.inboundRecovery
			r.now = func() time.Time { return now }
			msg := testPending("C1", "100.1", "alias").inbound
			msg.ReplyToMessageID = "99.1"
			msg.ReceivedAt = now
			recordAliasDeliveryForMentionOnly(cfg, []mentionOnlyBinding{binding}, "s1", msg, true)
			if err := r.holdAlias(msg, "s1", "mayor", "body", recoveryReceipt("a", "s1", false)); err != nil {
				t.Fatal(err)
			}
			if !cfg.mentionOnlyDeliveries.forSession("s1")[0].Provisional {
				t.Fatal("hold exposed as delivered")
			}
			if restart {
				cfg.mentionOnlyDeliveries = newMentionOnlyDeliveryLog()
				if err := installInboundRecovery(&cfg); err != nil {
					t.Fatal(err)
				}
				r = cfg.inboundRecovery
				r.now = func() time.Time { return now }
			}
			newer := msg
			newer.ProviderMessageID = "101.1"
			newer.ReceivedAt = now.Add(time.Second)
			recordAliasDeliveryForMentionOnly(cfg, []mentionOnlyBinding{binding}, "s1", newer)
			now = now.Add(time.Minute)
			r.tick(false)
			list := cfg.mentionOnlyDeliveries.forSession("s1")
			if len(list) < 2 || list[0].TS != "100.1" || list[0].ThreadTS != "99.1" || list[0].Provisional || !list[0].ReceivedAt.After(list[1].ReceivedAt) {
				t.Fatalf("routing=%+v", list)
			}
			msg.ProviderMessageID = "102.1"
			if err := r.holdAlias(msg, "s1", "mayor", "body", recoveryReceipt("b", "s1", false)); err != nil {
				t.Fatal(err)
			}
			if err := cfg.handleAliases.Set("mayor", "s2"); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			r.observe(recoveryReceipt("vouch", "s2", true))
			r.tick(false)
			if !strings.Contains(paths[len(paths)-1], "/session/s2/messages") {
				t.Fatalf("retarget paths=%v", paths)
			}
		})
	}
}
