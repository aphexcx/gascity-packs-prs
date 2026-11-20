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
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func recoveryTestConfig(t *testing.T, url string) config {
	t.Helper()
	t.Setenv("SLACK_INBOUND_HOLD_NOTICE", "off")
	t.Setenv("SLACK_INBOUND_HOLD_NOTICE_ROOMS", "")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gc"), []byte("#!/bin/sh\n[ \"$3 $4 $5\" = 'mail send mayor' ]\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := coalescingTestConfig(url, time.Hour)
	cfg.cityPath = t.TempDir()
	cfg.inboundDeadLetterDir = filepath.Join(cfg.cityPath, "dead")
	cfg.deliveryReceiptGate = true
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	cfg.inboundJournal = &inboundJournal{dir: filepath.Join(cfg.cityPath, "journal")}
	if err := installInboundRecovery(&cfg); err != nil {
		t.Fatal(err)
	}
	copyCfg := cfg
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
		return deliverCoalescedBatch(copyCfg, channel, batch)
	}
	cfg.coalescer.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		_, err := writeInboundDeadLetter(cfg.inboundDeadLetterDir, channel, batch, cause)
		return err == nil
	}
	t.Cleanup(func() { cfg.coalescer.flushAll() })
	return cfg
}

func recoveryReceipt(id, session string, delivered bool) deliveryReceipt {
	status := "failed"
	if delivered {
		status = "delivered"
	}
	return deliveryReceipt{present: true, id: id, status: status, targetSessionID: session}
}

// Before gp-b06o, the existing receipt harness charged three flushes and
// dead-lettered this line. Drive the production wiring with a fake clock and a
// gc accepting every POST but withholding delivery evidence for nine minutes.
func TestInboundRecoveryBusyComposerRegression(t *testing.T) {
	now := time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)
	start := now
	var posts []string
	var originalHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Message externalInboundMessage `json:"message"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		posts = append(posts, body.Message.ProviderMessageID)
		originalHeaders = append(originalHeaders, req.Header.Get("X-GC-Original-Receipt-ID"))
		status := "failed"
		if now.Sub(start) >= 9*time.Minute || body.Message.ProviderMessageID == "101.000001" {
			status = "delivered"
		}
		session := "s1"
		if body.Message.ProviderMessageID == "101.000001" {
			session = "s2"
		}
		fmt.Fprintf(w, `{"TargetSessionID":%q,"delivery":{"receipt_id":"receipt-%d","status":%q,"members":[{"session_id":%q,"status":%q}]}}`, session, len(posts), status, session, status)
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	r := cfg.inboundRecovery
	r.now = func() time.Time { return now }
	cfg.coalescer.enqueue("C1", testPending("C1", "100.000001", "founder line"))
	for i := 0; i < 3; i++ {
		cfg.coalescer.flushAheadOf("C1", "")
	}
	if len(posts) != 1 || r.counts()["C1"].Held != 1 {
		t.Fatalf("three fast flushes: posts=%v counts=%v", posts, r.counts())
	}
	// New traffic still flows, while a duplicate of the held message cannot
	// bypass ownership. This fresh delivery targets a different session;
	// same-session vouch release is covered by TestRound2VouchReleasesHeld.
	cfg.coalescer.enqueue("C1", testPending("C1", "100.000001", "duplicate"))
	cfg.coalescer.enqueue("C1", testPending("C1", "101.000001", "new line"))
	cfg.coalescer.flushAheadOf("C1", "")
	if len(posts) != 2 || posts[1] != "101.000001" {
		t.Fatalf("fresh traffic blocked: %v", posts)
	}
	for i, at := range []time.Duration{20 * time.Second, 80 * time.Second, 200 * time.Second, 500 * time.Second, 1100 * time.Second} {
		now = start.Add(at - time.Nanosecond)
		r.tick(false)
		if len(posts) != i+2 {
			t.Fatalf("early retry at %s: %v", at, posts)
		}
		now = start.Add(at)
		r.tick(false)
		if len(posts) != i+3 {
			t.Fatalf("missing retry at %s: %v", at, posts)
		}
		if r.counts()["C1"].Dead != 0 {
			t.Fatal("dead-lettered before 30 minutes")
		}
	}
	if r.counts()["C1"].Held != 0 {
		t.Fatalf("vouched recovery still held: %v", r.counts())
	}
	if originalHeaders[len(originalHeaders)-1] != "receipt-1" {
		t.Fatalf("original receipt header = %v", originalHeaders)
	}
	files, err := filepath.Glob(filepath.Join(cfg.inboundJournal.dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"original_receipt_id":"receipt-1"`)) {
		t.Fatal("journal lost original receipt")
	}
}

func TestInboundRecoveryDeadlineReplayOrderDedupAndRestart(t *testing.T) {
	now := time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path, dead := filepath.Join(dir, "holds", "state.json"), filepath.Join(dir, "dead")
	r, err := newInboundRecovery(path, dead)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return now }
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return recoveryReceipt("fail", "s1", false), nil }
	for _, ts := range []string{"102.0", "100.0", "101.0"} {
		if err := r.hold(testPending("C1", ts, "line "+ts), recoveryReceipt("original-"+ts, "s1", false), false); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(30*time.Minute - time.Nanosecond)
	r.tick(false)
	if r.counts()["C1"].Dead != 0 {
		t.Fatal("early retirement")
	}
	now = now.Add(time.Nanosecond)
	r.tick(false)
	if r.counts()["C1"].Dead != 3 {
		t.Fatalf("deadline counts=%v", r.counts())
	}
	r, err = newInboundRecovery(path, dead)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return now }
	var order []string
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		order = append(order, e.Inbound.ProviderMessageID)
		if !strings.HasPrefix(e.Inbound.Text, "[delivered late: dead-lettered at ") {
			t.Error("missing late marker")
		}
		if e.Inbound.originalReceiptID != "original-"+e.Inbound.ProviderMessageID {
			t.Error("missing original receipt")
		}
		return recoveryReceipt("new", "s1", true), nil
	}
	r.observe(recoveryReceipt("unrelated", "s2", true))
	r.tick(false)
	if len(order) != 0 {
		t.Fatal("unrelated session triggered replay")
	}
	r.observe(recoveryReceipt("healthy", "s1", true))
	r.tick(false)
	if !reflect.DeepEqual(order, []string{"100.0", "101.0", "102.0"}) {
		t.Fatalf("order=%v", order)
	}
	r.tick(true)
	if len(order) != 3 {
		t.Fatal("duplicate replay")
	}
	data, err := os.ReadFile(filepath.Join(dead, "C1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.Split(bytes.TrimSpace(data), []byte{'\n'})) != 6 {
		t.Fatalf("history must retain three originals and three completions: %s", data)
	}
	// A lost state snapshot still reconstructs completion from history.
	r2, _ := newInboundRecovery("", dead)
	if err := r2.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	r2.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		t.Error("history redelivered a completed ID")
		return deliveryReceipt{}, nil
	}
	r2.tick(true)
}

func TestInboundRecoveryNoticeOnePostAndEdit(t *testing.T) {
	now := time.Now()
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	var calls []string
	r.notice = func(channel, thread, ts, text string) (string, error) {
		calls = append(calls, ts+"|"+thread+"|"+text)
		return "notice.1", nil
	}
	r.hold(testPending("D1", "100.0", "first"), recoveryReceipt("r1", "s1", false), false)
	r.hold(testPending("D1", "101.0", "second"), recoveryReceipt("r2", "s1", false), false)
	r.updateNotices()
	if len(calls) != 0 {
		t.Fatal("early notice")
	}
	now = now.Add(time.Minute)
	r.updateNotices()
	r.updateNotices()
	if len(calls) != 1 || !strings.Contains(calls[0], "Delivery has been held since") || !strings.HasPrefix(calls[0], "|100.0|") {
		t.Fatalf("notices=%v", calls)
	}
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return recoveryReceipt("ok", "s1", true), nil }
	r.tick(false)
	r.updateNotices()
	if len(calls) != 2 || !strings.HasPrefix(calls[1], "notice.1|") || !strings.Contains(calls[1], "handed to the session's terminal by") {
		t.Fatalf("edits=%v", calls)
	}
}

func TestInboundRecoveryNoticeRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, channel, rooms, knob string
		wantRoom                   bool
	}{
		{"unlisted channel", "CROOM", "", "", false},
		{"direct message", "DDIRECT", "", "", true},
		{"listed conversation", "CLISTED", " COTHER, CLISTED ,", "", true},
		{"private channel", "GPRIVATE", "COTHER", "", false},
		{"group direct message", "GGROUPDM", "", "", false},
		{"unknown conversation", "UNKNOWN", "", "", false},
		{"disabled direct message", "DDIRECT", "", "off", false},
		{"disabled listed channel", "CLISTED", "CLISTED", "off", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := recoveryTestConfig(t, "")
			t.Setenv("SLACK_INBOUND_HOLD_NOTICE", tc.knob)
			t.Setenv("SLACK_INBOUND_HOLD_NOTICE_ROOMS", tc.rooms)
			dir := t.TempDir()
			mailPath := filepath.Join(dir, "mail")
			t.Setenv("HOLD_TEST_MAIL", mailPath)
			if err := os.WriteFile(filepath.Join(dir, "gc"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HOLD_TEST_MAIL\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			var calls []string
			slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				fmt.Fprint(w, `{"ok":true,"ts":"111.3"}`)
			}))
			t.Cleanup(slack.Close)
			old := slackAPIBase
			slackAPIBase = slack.URL
			t.Cleanup(func() { slackAPIBase = old })
			cfg.slackBotToken = "example-token"
			if err := installInboundRecovery(&cfg); err != nil {
				t.Fatal(err)
			}
			r := cfg.inboundRecovery
			now := time.Now()
			r.now = func() time.Time { return now }
			p := testPending(tc.channel, "111.2", "private message body")
			p.inbound.ReplyToMessageID = "111.1"
			if err := r.hold(p, recoveryReceipt("r1", "s1", false), false); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			check := func(wantCalls []string, phaseText string, rows int) {
				t.Helper()
				if !reflect.DeepEqual(calls, wantCalls) {
					t.Errorf("Slack calls = %v, want %v", calls, wantCalls)
				}
				b, err := os.ReadFile(mailPath)
				if err != nil {
					t.Fatal(err)
				}
				body := string(b)
				if strings.Count(body, "mail send mayor --dedup") != rows ||
					strings.Count(body, "channel="+tc.channel+" thread_ts=111.1 message_ts=111.2") != rows ||
					!strings.Contains(body, phaseText) || strings.Contains(body, p.inbound.Text) {
					t.Fatalf("unexpected operator mail: %q", body)
				}
			}
			var wantCalls []string
			if tc.wantRoom {
				wantCalls = append(wantCalls, "/chat.postMessage")
			}
			r.updateNotices()
			r.updateNotices()
			check(wantCalls, "Delivery has been held since", 1)
			r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return recoveryReceipt("ok", "s1", true), nil }
			r.tick(false)
			r.updateNotices()
			if tc.wantRoom {
				wantCalls = append(wantCalls, "/chat.update")
			}
			check(wantCalls, "handed to the session's terminal", 2)
		})
	}
}

func TestInboundRecoveryNoticePolicyAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		postedBefore bool
		allowAfter   bool
	}{
		{"legacy room removed", true, false},
		{"internal incident stays internal", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, now := round2Recovery(t)
			p := testPending("CROOM", "111.2", "message")
			p.inbound.ReplyToMessageID = "111.1"
			if err := r.hold(p, recoveryReceipt("r1", "s1", false), false); err != nil {
				t.Fatal(err)
			}
			n := inboundHoldNotice{Since: *now, ThreadTS: "111.1"}
			if tc.postedBefore {
				n.TS = "111.3"
			} else {
				n.HoldQueued, n.InternalOnly, n.MessageTS = true, true, "111.2"
			}
			r.saveNotice("CROOM", n)
			r, err := newInboundRecovery(r.path, r.deadDir)
			if err != nil {
				t.Fatal(err)
			}
			if tc.allowAfter {
				r.noticeRooms = []string{"CROOM"}
			}
			r.notice = func(string, string, string, string) (string, error) {
				t.Fatal("restart published an internal outcome")
				return "", nil
			}
			records := 0
			r.recordNotice = func(channel string, report holdNoticeReport) error {
				records++
				if channel != "CROOM" || report.ThreadTS != "111.1" || report.MessageTS != "111.2" || report.Phase != "outcome" {
					t.Fatalf("wrong outcome coordinates: %s %+v", channel, report)
				}
				return nil
			}
			if _, err := r.acknowledge("CROOM", "111.2"); err != nil {
				t.Fatal(err)
			}
			r.updateNotices()
			r.updateNotices()
			if records != 1 {
				t.Fatalf("outcome records = %d", records)
			}
		})
	}
}

func TestInboundRecoveryNoticeFallback(t *testing.T) {
	r, now := round2Recovery(t)
	t.Setenv("PATH", t.TempDir()) // The mail executable is unavailable.
	r.recordNotice = holdNoticeRecorder(t.TempDir(), r.deadDir)
	round2Hold(t, r, "111.2")
	*now = now.Add(time.Minute)
	r.updateNotices()
	if _, err := r.acknowledge("C1", "111.2"); err != nil {
		t.Fatal(err)
	}
	r.updateNotices()
	r.updateNotices()
	dir := filepath.Join(r.deadDir, "hold-notices")
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("fallback files = %v, error = %v", files, err)
	}
	phases := map[string]string{}
	for _, file := range files {
		b, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]string
		if err := json.Unmarshal(b, &row); err != nil {
			t.Fatal(err)
		}
		if row["channel"] != "C1" || row["thread_ts"] != "111.2" || row["message_ts"] != "111.2" {
			t.Fatalf("wrong fallback coordinates: %v", row)
		}
		phases[row["phase"]] = row["text"]
	}
	if !strings.Contains(phases["held"], "Delivery has been held") || !strings.Contains(phases["outcome"], "acknowledged by hand") {
		t.Fatalf("fallback phases = %v", phases)
	}
}

func TestInboundRecoveryNoticeReportingFailureSurvivesSettlement(t *testing.T) {
	r, now := round2Recovery(t)
	round2Hold(t, r, "111.2")
	*now = now.Add(time.Minute)
	r.recordNotice = func(string, holdNoticeReport) error { return errors.New("storage unavailable") }
	r.updateNotices()
	if _, err := r.acknowledge("C1", "111.2"); err != nil {
		t.Fatal(err)
	}
	r.updateNotices()
	r, err := newInboundRecovery(r.path, r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return now.Add(time.Hour) }
	var recorded []string
	r.recordNotice = func(_ string, report holdNoticeReport) error {
		recorded = append(recorded, report.Phase)
		return nil
	}
	r.updateNotices()
	r.updateNotices()
	if !reflect.DeepEqual(recorded, []string{"held", "outcome"}) {
		t.Fatalf("reports after storage recovery = %v", recorded)
	}
}

func TestInboundRecoveryNoticeReportingFailurePreservesRoomDelivery(t *testing.T) {
	r, now := round2Recovery(t)
	r.recordNotice = func(string, holdNoticeReport) error { return errors.New("mail and disk unavailable") }
	var requests []string
	r.notice = func(_, _, ts, _ string) (string, error) {
		requests = append(requests, ts)
		return "111.3", nil
	}
	round2Hold(t, r, "111.2")
	*now = now.Add(time.Minute)
	r.updateNotices()
	if _, err := r.acknowledge("C1", "111.2"); err != nil {
		t.Fatal(err)
	}
	r.updateNotices()
	if !reflect.DeepEqual(requests, []string{"", "111.3"}) {
		t.Fatalf("room post and edit during operator outage = %v", requests)
	}
}

func TestInboundRecoveryNoticeUncertainMail(t *testing.T) {
	dir := t.TempDir()
	mail := filepath.Join(dir, "gc")
	t.Setenv("PATH", dir)
	if err := os.WriteFile(mail, []byte("#!/bin/sh\necho 'connection reset' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(dir, "dead")
	report := holdNoticeRecorder(dir, dead)
	err := report("CROOM", holdNoticeReport{ThreadTS: "111.1", MessageTS: "111.2", Phase: "held", Text: "hold text"})
	if err == nil {
		t.Error("uncertain acceptance must retry the same mail key")
	}
	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("uncertain acceptance wrote a second report: %v", err)
	}
}

func TestInboundRecoveryNoticeUncertainThenRefusedAfterRestart(t *testing.T) {
	r, now := round2Recovery(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "gc")
	t.Setenv("PATH", dir)
	r.recordNotice = holdNoticeRecorder(dir, r.deadDir)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'connection reset' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	round2Hold(t, r, "111.2")
	*now = now.Add(time.Minute)
	r.updateNotices()
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'connection refused' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := newInboundRecovery(r.path, r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	r.recordNotice = holdNoticeRecorder(dir, r.deadDir)
	r.now = func() time.Time { return now.Add(time.Hour) }
	r.updateNotices()
	if _, err := os.Stat(filepath.Join(r.deadDir, "hold-notices")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later refusal duplicated possibly accepted mail: %v", err)
	}
	if len(r.state.Notices["C1"].Reports) != 1 {
		t.Fatal("lost uncertain mail retry")
	}
}

func TestInboundRecoveryNoticeSuccessiveIncidentsDuringEditFailure(t *testing.T) {
	r, now := round2Recovery(t)
	var edits, records []string
	fail := true
	r.notice = func(_, thread, ts, _ string) (string, error) {
		if ts != "" && fail {
			return "", errors.New("rate_limited")
		}
		if ts != "" {
			edits = append(edits, thread)
		}
		return "notice-" + thread, nil
	}
	r.recordNotice = func(_ string, report holdNoticeReport) error {
		records = append(records, report.MessageTS+" "+report.Phase)
		return nil
	}
	for _, ts := range []string{"111.1", "111.2"} {
		round2Hold(t, r, ts)
		*now = now.Add(time.Minute)
		r.updateNotices()
		if _, err := r.acknowledge("C1", ts); err != nil {
			t.Fatal(err)
		}
		r.updateNotices()
	}
	fail = false
	*now = now.Add(5 * time.Minute)
	r.updateNotices()
	if !reflect.DeepEqual(records, []string{"111.1 held", "111.1 outcome", "111.2 held", "111.2 outcome"}) ||
		!reflect.DeepEqual(edits, []string{"111.1", "111.2"}) {
		t.Fatalf("reports=%v edits=%v", records, edits)
	}
}

func TestInboundRecoveryNoticeSuccessiveIncidents(t *testing.T) {
	type scenario struct{ channel, failure, placement, replyTo string }
	cases := []scenario{{"CUNLISTED", "none", "top", ""}}
	for _, channel := range []string{"DDIRECT", "CLISTED"} {
		for _, failure := range []string{"retryable-post", "permanent-post", "closing-edit"} {
			cases = append(cases,
				scenario{channel, failure, "same", "110.0"},
				scenario{channel, failure, "other", "120.0"},
				scenario{channel, failure, "top", ""})
		}
	}
	for _, tc := range cases {
		t.Run(tc.channel+"/"+tc.failure+"/"+tc.placement, func(t *testing.T) {
			r, now := round2Recovery(t)
			r.noticeRooms = []string{"CLISTED"}
			type request struct{ thread, ts string }
			var posts, edits []request
			r.notice = func(channel, thread, ts, text string) (string, error) {
				if channel != tc.channel {
					t.Errorf("Slack channel = %q, want %q", channel, tc.channel)
				}
				if ts != "" {
					edits = append(edits, request{thread, ts})
					if len(edits) == 1 && tc.failure == "closing-edit" {
						return "", errors.New("rate_limited")
					}
					return ts, nil
				}
				posts = append(posts, request{thread, ts})
				if len(posts) == 1 {
					switch tc.failure {
					case "retryable-post":
						return "", errors.New("rate_limited")
					case "permanent-post":
						return "", errors.New("thread_not_found")
					}
				}
				return fmt.Sprintf("notice-%d", len(posts)), nil
			}
			var reports []holdNoticeReport
			r.recordNotice = func(channel string, report holdNoticeReport) error {
				if channel != tc.channel {
					t.Errorf("operator channel = %q, want %q", channel, tc.channel)
				}
				reports = append(reports, report)
				return nil
			}
			hold := func(ts, replyTo string) time.Time {
				t.Helper()
				p := testPending(tc.channel, ts, "held line")
				p.inbound.ReplyToMessageID = replyTo
				if err := r.hold(p, recoveryReceipt("r"+ts, "s1", false), false); err != nil {
					t.Fatal(err)
				}
				return *now
			}
			first := hold("111.1", "110.0")
			*now = now.Add(time.Minute)
			r.updateNotices()
			*now = now.Add(time.Second)
			if _, err := r.acknowledge(tc.channel, "111.1"); err != nil {
				t.Fatal(err)
			}
			r.updateNotices()
			if len(reports) != 2 || reports[1].Phase != "outcome" {
				t.Errorf("first settlement tick: got %d reports, want held and outcome", len(reports))
			}

			*now = now.Add(time.Second) // Admit the next hold before the first retry.
			second := hold("121.1", tc.replyTo)
			thread := tc.replyTo
			if thread == "" {
				thread = "121.1"
			}
			r.updateNotices()
			*now = now.Add(time.Minute)
			r.updateNotices()
			r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return recoveryReceipt("ok", "s1", true), nil }
			*now = now.Add(time.Second)
			r.tick(false)
			r.updateNotices()

			opening := func(start time.Time) string {
				return fmt.Sprintf("Delivery has been held since %s; retrying for up to %s.", start.Format(time.RFC3339), r.bound)
			}
			wantReports := []holdNoticeReport{
				{Since: first, ThreadTS: "110.0", MessageTS: "111.1", Phase: "held", Text: opening(first)},
				{Since: first, ThreadTS: "110.0", MessageTS: "111.1", Phase: "outcome", Text: "The held incident was acknowledged by hand; this does not confirm terminal delivery or reading."},
				{Since: second, ThreadTS: thread, MessageTS: "121.1", Phase: "held", Text: opening(second)},
				{Since: second, ThreadTS: thread, MessageTS: "121.1", Phase: "outcome", Text: fmt.Sprintf("Held since %s; handed to the session's terminal by %s, with reading unconfirmed.", second.Format(time.RFC3339), now.Format(time.RFC3339))},
			}
			if len(reports) != len(wantReports) {
				t.Errorf("operator reports = %d, want %d", len(reports), len(wantReports))
			}
			for i := 0; i < min(len(reports), len(wantReports)); i++ {
				if reports[i] != wantReports[i] {
					t.Errorf("report %d = %+v, want %+v", i, reports[i], wantReports[i])
				}
			}
			var wantPosts, wantEdits []request
			if tc.channel != "CUNLISTED" {
				wantPosts = []request{{"110.0", ""}, {thread, ""}}
				if tc.failure == "closing-edit" {
					wantEdits = append(wantEdits, request{"110.0", "notice-1"}, request{"110.0", "notice-1"})
				}
				wantEdits = append(wantEdits, request{thread, "notice-2"})
			}
			if !reflect.DeepEqual(posts, wantPosts) || !reflect.DeepEqual(edits, wantEdits) {
				t.Errorf("Slack posts = %v, edits = %v; want %v, %v", posts, edits, wantPosts, wantEdits)
			}
		})
	}
}

func TestInboundRecoveryNoticeFallbackPreservesExistingRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	report := holdNoticeReport{ThreadTS: "111.1", MessageTS: "111.2", Phase: "held", Text: "hold text"}
	id := ingestionDigest(strings.Join([]string{"CROOM", report.Since.UTC().Format(time.RFC3339Nano), report.ThreadTS, report.MessageTS, report.Phase}, "|"))
	target := filepath.Join(dir, "hold-notices", id+".json")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_FALLBACK", target)
	// Another writer claims the final name while mail is in flight.
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte("#!/bin/sh\necho winner > \"$TEST_FALLBACK\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := holdNoticeRecorder(dir, dir)("CROOM", report); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "winner\n" {
		t.Fatalf("existing fallback replaced: %q %v", got, err)
	}
}

func TestInboundRecoveryPendingReceiptPollsWithoutRepost(t *testing.T) {
	now := time.Now()
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		t.Error("reposted a running receipt")
		return deliveryReceipt{}, nil
	}
	var polls int
	r.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		polls++
		if id != "pending-1" {
			t.Errorf("poll id=%s", id)
		}
		return receiptPollPending, deliveryReceipt{present: true, id: id, status: "pending"}, nil
	}
	r.hold(testPending("C1", "100.0", "held"), deliveryReceipt{present: true, id: "pending-1", status: "pending"}, false)
	now = now.Add(time.Minute)
	r.tick(false)
	if polls != 1 {
		t.Fatalf("polls=%d", polls)
	}
	// Expiry is durable uncertainty, never a license to repost an active send.
	now = now.Add(30 * time.Minute)
	r.tick(false)
	r.tick(true)
	if polls != 2 {
		t.Fatalf("redrive did not poll original receipt: %d", polls)
	}
}

func TestInboundRecoveryConcludedPendingNeverReposts(t *testing.T) {
	now := time.Now()
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		t.Fatal("concluded pending is not proof of loss")
		return deliveryReceipt{}, nil
	}
	r.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		return receiptPollConcluded, deliveryReceipt{present: true, id: id, status: "pending"}, nil
	}
	if err := r.hold(testPending("C1", "100.0", "held"), deliveryReceipt{present: true, id: "pending-1", status: "pending"}, false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	r.tick(false)
	if got := r.state.Entries["C1|100.0"]; !got.PendingReceipt || got.PendingReceiptID != "pending-1" || !got.DeliveredAt.IsZero() {
		t.Fatalf("lost uncertainty: %+v", got)
	}
}

func TestInboundRecoveryExplicitHeldContract(t *testing.T) {
	receipt := parseDeliveryReceipt([]byte(`{"delivery":{"receipt_id":"h1","status":"held","members":[{"session_id":"s1","status":"held","held":{"state":"unowned_draft","since":"2026-11-09T20:00:00Z","age_ms":60001}}]}}`))
	if receipt.verdict(true) != receiptUnconfirmed || receipt.members[0].Held.AgeMS != 60001 {
		t.Fatalf("held contract: %+v", receipt)
	}
	now := time.Now()
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return receipt, nil }
	r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
		t.Fatal("held means no new payload landed, retry instead of polling")
		return receiptPollUnknown, deliveryReceipt{}, nil
	}
	var notice string
	r.noticeRooms = []string{"C1"}
	r.notice = func(_, _, _, text string) (string, error) { notice = text; return "notice", nil }
	if err := r.hold(testPending("C1", "100.0", "held"), receipt, false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	r.tick(false)
	if !strings.Contains(notice, "retrying for up to") || r.state.Entries["C1|100.0"].Attempts != 1 {
		t.Fatalf("notice=%q state=%+v", notice, r.state.Entries["C1|100.0"])
	}
}

func TestInboundRecoveryReceiptPolicyCompatibility(t *testing.T) {
	for _, mode := range []string{"gate-off-pending", "gate-off-unvouched", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			r, _ := newInboundRecovery("", t.TempDir())
			r.now = func() time.Time { return now }
			receipt := recoveryReceipt("r1", "s1", false)
			if mode == "gate-off-pending" {
				receipt.status = "pending"
			}
			if err := r.hold(testPending("C1", "100.0", "held"), receipt, false); err != nil {
				t.Fatal(err)
			}
			r.gated = !strings.HasPrefix(mode, "gate-off")
			r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				if !r.gated {
					t.Fatal("gate-off reposted an accepted hold")
				}
				return deliveryReceipt{}, nil
			}
			now = now.Add(time.Minute)
			if !r.gated {
				// Gate-off preserves accepted uncertainty without resending,
				// even when the deadline elapsed while offline.
				now = now.Add(time.Hour)
			}
			r.tick(false)
			if !r.state.Entries["C1|100.0"].DeliveredAt.IsZero() || r.counts()["C1"].ByHand != 1 || len(r.sessions) != 0 {
				t.Fatalf("unknown settlement must stay visible without announcing a vouch: %+v sessions=%v", r.state.Entries, r.sessions)
			}
		})
	}
}

func TestInboundRecoveryPendingChannelStillDispatchesAlias(t *testing.T) {
	var aliases atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/messages"):
			aliases.Add(1)
			fmt.Fprint(w, `{"delivery":{"receipt_id":"alias","status":"delivered"}}`)
		case strings.Contains(req.URL.Path, "/bindings"):
			fmt.Fprint(w, `{"items":[]}`)
		default:
			fmt.Fprint(w, `{"delivery":{"receipt_id":"channel","status":"pending"}}`)
		}
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	cfg.bindingCheck = newBindingCheckCache()
	aliasesReg := newTestHandleAliasRegistry(t)
	if err := aliasesReg.Set("mayor", "sess-mayor-1"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	env := botMentionEnvelope(t, "message", "alias-event", "C1", "100.1", "", "@mayor: please handle this", true)
	processSlackEvent(cfg, aliasesReg, nil, nil, nil, nil, env, func() { close(done) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("alias dispatch did not conclude")
	}
	if aliases.Load() != 1 || cfg.inboundRecovery.counts()["C1"].Held != 1 {
		t.Fatalf("aliases=%d holds=%v", aliases.Load(), cfg.inboundRecovery.counts())
	}
	if cfg.deliveredIDs.seen("", "C1", "100.1") {
		t.Fatal("held channel was marked delivered")
	}
}

func TestInboundRecoveryPeerPendingIsDurable(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		posts.Add(1)
		fmt.Fprint(w, `{"delivery":{"receipt_id":"peer","status":"pending"}}`)
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	cfg.peerBots = newTestPeerBotsRegistry(t, peerTestAllowlist)
	cfg.peerContext, cfg.peerAuthors = newPeerContextBuffer(), citadelResolver()
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
		peerBotEnvelope(t, "message", "E1", "C1", "100.1", "", "peer text", peerTestPeerBotID, ""), func() {})
	if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
		t.Fatal("active pending peer returned to resend FIFO")
	}
	r, err := newInboundRecovery(cfg.inboundRecovery.path, cfg.inboundRecovery.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || !r.state.Entries["C1|100.1"].PendingReceipt {
		t.Fatalf("posts=%d entries=%+v", posts.Load(), r.state.Entries)
	}
}

func TestInboundRecoveryPendingBatchJournalIdentitySurvivesRestart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"delivery":{"receipt_id":"batch","status":"pending"}}`)
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	cfg.peerContext = newPeerContextBuffer()
	peer := testPending("C1", "100.0", "peer rider").inbound
	cfg.peerContext.add(peerContextItem{Channel: "C1", TS: "100.0", Text: "peer rider", inbound: peer})
	copyCfg := cfg
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
		return deliverCoalescedBatch(copyCfg, channel, batch)
	}
	cfg.coalescer.enqueue("C1", testPending("C1", "100.1", "first"))
	cfg.coalescer.enqueue("C1", testPending("C1", "100.2", "second"))
	cfg.coalescer.flushAheadOf("C1", "")
	if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
		t.Fatal("pending batch rider returned to resend FIFO")
	}
	if err := installInboundRecovery(&cfg); err != nil {
		t.Fatal(err)
	}
	r := cfg.inboundRecovery
	r.now = func() time.Time { return time.Now().Add(time.Minute) }
	r.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		return receiptPollConcluded, recoveryReceipt(id, "s1", true), nil
	}
	r.tick(false)
	files, _ := filepath.Glob(filepath.Join(cfg.inboundJournal.dir, "*.jsonl"))
	var accepted *inboundJournalRecord
	concluded := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			var rec inboundJournalRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatal(err)
			}
			if rec.ReceiptID != "batch" {
				continue
			}
			if rec.Phase == "accepted" {
				copy := rec
				accepted = &copy
				continue
			}
			if rec.Phase == "concluded" {
				concluded++
				if accepted == nil || rec.PayloadSHA256 != accepted.PayloadSHA256 || rec.MessageTS != accepted.MessageTS || !reflect.DeepEqual(rec.Windows, accepted.Windows) {
					t.Fatalf("batch completion identity changed: accepted=%+v concluded=%+v", accepted, rec)
				}
			}
		}
	}
	if concluded != 3 {
		t.Fatalf("concluded=%d", concluded)
	}
}

func TestInboundRecoveryAliasHoldDedupsAfterRestart(t *testing.T) {
	var aliases atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/messages"):
			aliases.Add(1)
			fmt.Fprint(w, `{"delivery":{"receipt_id":"alias","status":"pending"}}`)
		case strings.Contains(req.URL.Path, "/bindings"):
			fmt.Fprint(w, `{"items":[]}`)
		default:
			fmt.Fprint(w, `{"delivery":{"receipt_id":"channel","status":"delivered"}}`)
		}
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	cfg.bindingCheck = newBindingCheckCache()
	reg := newTestHandleAliasRegistry(t)
	if err := reg.Set("mayor", "sess-mayor-1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if i == 1 {
			if err := installInboundRecovery(&cfg); err != nil {
				t.Fatal(err)
			}
			cfg.channelClaims = newEventDedupCache(eventDedupTTL)
			cfg.deliveredIDs = newDeliveredIDs()
		}
		done := make(chan struct{})
		env := botMentionEnvelope(t, "message", fmt.Sprintf("alias-event-%d", i), "C1", "100.1", "", "@mayor: please handle this", true)
		processSlackEvent(cfg, reg, nil, nil, nil, nil, env, func() { close(done) })
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("dispatch did not conclude")
		}
	}
	if aliases.Load() != 1 || cfg.inboundRecovery.counts()["C1"].Held != 1 {
		t.Fatalf("aliases=%d holds=%v", aliases.Load(), cfg.inboundRecovery.counts())
	}
}

func TestInboundRecoveryAliasDeadLetterKeepsCurrentReceipt(t *testing.T) {
	for _, originalLost := range []bool{false, true} {
		t.Run(fmt.Sprint(originalLost), func(t *testing.T) {
			dir := t.TempDir()
			msg := testPending("C1", "100.1", "alias").inbound
			msg.originalReceiptID = "original"
			f := newReceiptFollowUps(nil, true, nil)
			f.deadline = 0
			f.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
				return receiptPollConcluded, deliveryReceipt{present: true, id: id, status: "pending"}, nil
			}
			h := heldDelivery{receiptID: "original", repost: func() (deliveryReceipt, error) {
				return deliveryReceipt{present: true, id: "recovery", status: "pending"}, nil
			},
				deadLetter: func(cause error) bool {
					_, err := writeAliasDeadLetter(dir, "C1", msg, "s1", "mayor", "exact body", cause)
					return err == nil
				}}
			if originalLost {
				f.lost(h, 1, errors.New("definite loss"))
			} else {
				f.stuck(h, errors.New("timeout"))
			}
			r, err := newInboundRecovery("", dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.importDeadLetters(); err != nil {
				t.Fatal(err)
			}
			want := "original"
			if originalLost {
				want = "recovery"
			}
			r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				t.Fatal("reposted pending alias")
				return deliveryReceipt{}, nil
			}
			var polled string
			r.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
				polled = id
				return receiptPollPending, deliveryReceipt{}, nil
			}
			r.tick(true)
			e := r.state.Entries["C1|100.1|alias:s1"]
			if polled != want || e.OriginalReceiptID != "original" || !e.PendingReceipt {
				t.Fatalf("polled=%s entry=%+v", polled, e)
			}
		})
	}
}

func TestInboundRecoveryProbeCountsDurableMisses(t *testing.T) {
	r, _ := newInboundRecovery("", t.TempDir())
	r.hold(testPending("C1", "100.0", "held"), recoveryReceipt("r1", "s1", false), false)
	r.hold(testPending("C2", "101.0", "dead"), recoveryReceipt("r2", "s2", false), false)
	e := r.state.Entries["C2|101.0"]
	e.DeadLetteredAt = time.Now()
	r.state.Entries["C2|101.0"] = e
	l := newInboundLiveness(config{inboundRecovery: r}, newFakeHistory())
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	l.probe(context.Background(), probeModeWatchdog, "test")
	if l.lastMissed.Load() != 2 || strings.Contains(output.String(), "nothing missed") {
		t.Fatalf("missed=%d log=%s", l.lastMissed.Load(), output.String())
	}
	for _, want := range []string{"C1 held=1 dead-lettered=0", "C2 held=0 dead-lettered=1", "MISSED"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q: %s", want, output.String())
		}
	}
}

func TestInboundRecoverySlackNoticeIsAdapterAuthored(t *testing.T) {
	var paths []string
	var bodies []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]string
		json.NewDecoder(req.Body).Decode(&body)
		paths, bodies = append(paths, req.URL.Path), append(bodies, body)
		fmt.Fprint(w, `{"ok":true,"ts":"notice.1"}`)
	}))
	defer srv.Close()
	old := slackAPIBase
	slackAPIBase = srv.URL
	defer func() { slackAPIBase = old }()
	post := inboundHoldPoster("test-token")
	ts, err := post("C1", "100.0", "", "held")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := post("C1", "100.0", ts, "delivered late"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"/chat.postMessage", "/chat.update"}) || bodies[0]["thread_ts"] != "100.0" || bodies[1]["ts"] != ts {
		t.Fatalf("calls=%v %v", paths, bodies)
	}
	if validIngestionAct(ingestionSessionAct{Kind: "reply", Source: "adapter_hold", SessionID: "s1", ThreadTS: "100.0"}) {
		t.Fatal("adapter post counted as session proof")
	}
}

func TestInboundRecoveryRestartKeepsDeadlineAndNotice(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	r, _ := newInboundRecovery(path, filepath.Join(dir, "dead"))
	r.now = func() time.Time { return now }
	if err := r.hold(testPending("C1", "100.0", "held"), recoveryReceipt("original", "s1", false), false); err != nil {
		t.Fatal(err)
	}
	r.notice = func(string, string, string, string) (string, error) { return "notice.1", nil }
	r.noticeRooms = []string{"C1"}
	now = now.Add(time.Minute)
	r.updateNotices()
	stored := r.state.Entries["C1|100.0"]
	r2, err := newInboundRecovery(path, r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := r2.state.Entries["C1|100.0"]; !got.HeldAt.Equal(stored.HeldAt) || !got.NextAttemptAt.Equal(stored.NextAttemptAt) {
		t.Fatal("restart reset hold clock")
	}
	r2.now = func() time.Time { return now }
	r2.notice = func(string, string, string, string) (string, error) {
		t.Error("restart duplicated notice")
		return "", nil
	}
	r2.noticeRooms = []string{"C1"}
	r2.updateNotices()
	// A refused write reports lost durability but keeps recovery ownership.
	r2.path = dir
	if err := r2.hold(testPending("C1", "101.0", "new"), recoveryReceipt("new", "s1", false), false); err == nil {
		t.Fatal("hold succeeded without durable state")
	}
	if !r2.known("C1", "101.0") || !r2.dirty {
		t.Fatal("failed durable admission discarded the in-memory hold or its write retry")
	}
}

func TestInboundRecoveryPayloadRejectionAfterHold(t *testing.T) {
	now := time.Now()
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	p := testPendingWithAttachment("C1", "100.0", "message", "/tmp/audio.m4a")
	r.hold(p, recoveryReceipt("original", "s1", false), false)
	var attachmentCounts []int
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		attachmentCounts = append(attachmentCounts, len(e.Inbound.Attachments))
		return deliveryReceipt{}, permanent422()
	}
	now = now.Add(20 * time.Second)
	r.tick(false)
	now = now.Add(time.Minute)
	r.tick(false)
	if !reflect.DeepEqual(attachmentCounts, []int{1, 0}) || r.counts()["C1"].ByHand != 1 {
		t.Fatalf("rejection ladder=%v counts=%v", attachmentCounts, r.counts())
	}
}

func TestInboundRecoveryAdminAndLegacyAliasDestination(t *testing.T) {
	dir := t.TempDir()
	inbound := testPending("C1", "100.0", "alias message").inbound
	if _, err := writeAliasDeadLetter(dir, "C1", inbound, "original-session", "handle", "original rendered body", &receiptFollowUpLoss{cause: errReceiptFollowUpLost, resolved: true}); err != nil {
		t.Fatal(err)
	}
	r, _ := newInboundRecovery("", dir)
	request := httptest.NewRequest(http.MethodPost, "/internal/inbound/redrive", strings.NewReader(`{"conversation_id":"C1"}`))
	response := httptest.NewRecorder()
	r.handleRedrive(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatal(response.Code, response.Body.String())
	}
	var got inboundRecoveryEntry
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		got = e
		return recoveryReceipt("replay", "original-session", true), nil
	}
	r.tick(false)
	if got.AliasSessionID != "original-session" || !strings.Contains(got.AliasBody, "original rendered body") || !strings.HasPrefix(got.AliasBody, "[delivered late:") {
		t.Fatalf("wrong alias destination/body: %+v", got)
	}
	if err := r.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	if r.counts()["C1"].Dead != 0 {
		t.Fatal("completed alias still dead")
	}
}

func TestInboundRecoveryLegacyReceiptProvenance(t *testing.T) {
	dir := t.TempDir()
	dead, journal := filepath.Join(dir, "dead"), filepath.Join(dir, "journal")
	_, err := writeInboundDeadLetter(dead, "C1", []pendingChannelInbound{testPending("C1", "100.0", "legacy")}, errDeliveryUnvouched)
	if err != nil {
		t.Fatal(err)
	}
	j := &inboundJournal{dir: journal}
	if err := j.append(inboundJournalRecord{Phase: "accepted", Channel: "C1", MessageTS: "100.0", ReceiptID: "legacy-original", TargetSessions: []string{"s1"}}); err != nil {
		t.Fatal(err)
	}
	r, _ := newInboundRecovery("", dead)
	if err := r.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	if err := r.enrichFromJournal(journal); err != nil {
		t.Fatal(err)
	}
	e := r.state.Entries["C1|100.0"]
	if e.OriginalReceiptID != "legacy-original" || !reflect.DeepEqual(e.TargetSessions, []string{"s1"}) {
		t.Fatalf("missing provenance: %+v", e)
	}
}

func TestInboundRecoveryConfigAndPayloadRejection(t *testing.T) {
	cfg := recoveryTestConfig(t, "http://unused.invalid")
	if cfg.inboundRecovery.bound != 30*time.Minute || cfg.inboundRecovery.ceiling != 15*time.Minute {
		t.Fatal("wrong defaults")
	}
	cfg.coalescer.charge("C1", testPending("C1", "100.0", "rejected"), permanent422())
	if cfg.inboundRecovery.counts()["C1"].Held != 0 {
		t.Fatal("payload refusal became a hold")
	}
	if _, err := os.Stat(filepath.Join(cfg.inboundDeadLetterDir, "C1.jsonl")); err != nil {
		t.Fatal(err)
	}
	for i, want := range []time.Duration{20 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		if got := retryLadderDelay(inboundHoldBackoff, i, 15*time.Minute); got != want {
			t.Fatalf("step %d=%s want %s", i, got, want)
		}
	}
	if got := retryLadderDelay(inboundHoldBackoff, 5, 3*time.Minute); got != 3*time.Minute {
		t.Fatal(got)
	}
}

func TestInboundRecoveryFailedReplayKeepsFollowersPaused(t *testing.T) {
	now := time.Now()
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	for _, ts := range []string{"100.0", "101.0"} {
		r.hold(testPending("C1", ts, "held"), recoveryReceipt("original", "s1", false), false)
	}
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		return recoveryReceipt("failed", "s1", false), nil
	}
	now = now.Add(30 * time.Minute)
	r.tick(false)
	var calls []string
	healthy := false
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		calls = append(calls, e.Inbound.ProviderMessageID)
		return recoveryReceipt("replay", "s1", healthy), nil
	}
	r.tick(true)
	r.tick(false)
	r.tick(false)
	if !reflect.DeepEqual(calls, []string{"100.0"}) {
		t.Fatalf("replay overtook failed head: %v", calls)
	}
	healthy = true
	now = now.Add(15 * time.Minute)
	r.observe(recoveryReceipt("vouched", "s1", true))
	r.tick(false)
	if !reflect.DeepEqual(calls, []string{"100.0", "100.0", "101.0"}) {
		t.Fatalf("recovery order=%v", calls)
	}
}

func TestInboundRecoveryDeadLetterHistorySupersedesStaleHold(t *testing.T) {
	dir := t.TempDir()
	path, dead := filepath.Join(dir, "state.json"), filepath.Join(dir, "dead")
	r, err := newInboundRecovery(path, dead)
	if err != nil {
		t.Fatal(err)
	}
	r.hold(testPending("C1", "100.0", "held"), recoveryReceipt("original", "s1", false), false)
	e := r.state.Entries["C1|100.0"]
	e.DeadLetteredAt = time.Now()
	if err := r.writeDead(e, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Crash before the queue snapshot can record retirement.
	restarted, err := newInboundRecovery(path, dead)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	if got := restarted.counts()["C1"]; got.Held != 0 || got.Dead != 1 {
		t.Fatalf("stale snapshot won: %+v", got)
	}
}

func TestInboundRecoveryRetirementWriteFailureNeverReposts(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	bad := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(bad, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := newInboundRecovery(filepath.Join(dir, "state.json"), bad)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return now }
	r.hold(testPending("C1", "100.0", "held"), recoveryReceipt("original", "s1", false), false)
	posts := 0
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { posts++; return deliveryReceipt{}, permanent422() }
	now = now.Add(20 * time.Second)
	r.tick(false)
	if posts != 1 || !r.state.Entries["C1|100.0"].Retiring {
		t.Fatal("lost durable retirement")
	}
	restarted, err := newInboundRecovery(r.path, filepath.Join(dir, "repaired"))
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = func() time.Time { return now }
	restarted.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		t.Error("reposted rejected payload after restart")
		return deliveryReceipt{}, nil
	}
	now = now.Add(15 * time.Minute)
	restarted.tick(false)
	if restarted.counts()["C1"].ByHand != 1 {
		t.Fatal("write did not recover")
	}
}

func TestInboundRecoveryPendingWithoutIDNeverReposts(t *testing.T) {
	now := time.Now()
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	r.hold(testPending("C1", "100.0", "pending"), deliveryReceipt{present: true, status: "pending"}, false)
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		t.Error("reposted pending delivery with no pollable ID")
		return deliveryReceipt{}, nil
	}
	now = now.Add(time.Minute)
	r.tick(false)
	if r.counts()["C1"].ByHand != 1 {
		t.Fatal("pending entry lost")
	}
}

func TestInboundRecoveryDelayedNoticeEditKeepsDeliveryTime(t *testing.T) {
	now := time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	r.hold(testPending("C1", "100.0", "held"), recoveryReceipt("original", "s1", false), false)
	failEdit := true
	var edited string
	r.noticeRooms = []string{"C1"}
	r.notice = func(_, _, ts, text string) (string, error) {
		if ts != "" {
			if failEdit {
				return "", errors.New("Slack temporarily unavailable")
			}
			edited = text
		}
		return "notice.1", nil
	}
	now = now.Add(time.Minute)
	r.updateNotices()
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return recoveryReceipt("ok", "s1", true), nil }
	r.tick(false)
	delivered := now
	now = now.Add(time.Hour)
	failEdit = false
	r.updateNotices()
	if !strings.Contains(edited, delivered.Format(time.RFC3339)) || strings.Contains(edited, now.Format(time.RFC3339)) {
		t.Fatalf("edit replaced delivery time: %s", edited)
	}
}

func TestInboundRecoveryRefusesFIFOsWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "C1.jsonl")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		if _, err := readRecoveryState(path); err == nil {
			done <- errors.New("FIFO state accepted")
			return
		}
		r, _ := newInboundRecovery("", dir)
		if err := r.importDeadLetters(); err != nil {
			done <- errors.New("FIFO dead-letter file disabled startup")
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recovery reader blocked on FIFO")
	}
}
