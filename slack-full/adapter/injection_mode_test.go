package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInjectionModeConfig(t *testing.T) {
	for _, mode := range []string{"", "full", "minimal", " Minimal "} {
		env := baseSlackEnv()
		env[injectionModeEnv] = mode
		cfg, err := loadConfigFromEnv(stubEnv(env))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(strings.ToLower(mode)) == "minimal" {
			if !cfg.minimalInjection() || cfg.replyInstructions != "reply-to {thread_ts}" {
				t.Fatalf("minimal config: %+v", cfg.replyInstructions)
			}
		} else if cfg.minimalInjection() || cfg.replyInstructions != slackReplyInstructionsTemplate {
			t.Fatal("default changed")
		}
	}
	for key, value := range map[string]string{injectionModeEnv: "typo", "SLACK_INBOUND_JOURNAL_RETENTION_DAYS": "0"} {
		env := baseSlackEnv()
		env[key] = value
		if _, err := loadConfigFromEnv(stubEnv(env)); err == nil {
			t.Fatalf("accepted invalid %s", key)
		}
	}
}

func TestMinimalInbounds(t *testing.T) {
	for _, shape := range []string{"human", "thread", "peer", "batch"} {
		t.Run(shape, func(t *testing.T) {
			capture := &inboundCapture{}
			gc := httptest.NewServer(capture.handler())
			defer gc.Close()
			slack, calls := fakeSlackRepliesServer(t, nil)
			withSlackAPIStub(t, slack)
			cfg := coalescingTestConfig(gc.URL, 0)
			if shape == "peer" {
				cfg = peerTestConfig(gc.URL, newTestPeerBotsRegistry(t, peerTestAllowlistImmediate), citadelResolver())
			}
			cfg.injectionMode = "minimal"
			cfg.slackBotToken = "test-token"
			cfg.threadContextCache = newThreadContextCache()
			cfg.injectionParents = &injectionParents{}
			channel := "C1"
			if shape == "peer" {
				channel = "C_IMM"
			}
			cfg.channelNames = newChannelNameCache()
			cfg.channelNames.put(channel, "example", time.Now(), time.Hour)
			cfg.peerContext.add(peerContextItem{Channel: channel, TS: "100.000001", Text: "buffered context must not appear"})
			switch shape {
			case "human":
				processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, humanEnvelope(t, "E1", channel, "100.000003", "synthetic body"), func() {})
			case "thread":
				cfg.injectionParents.remember(channel, "100.000001", "Parent", "synthetic parent")
				event, _ := json.Marshal(slackMessageEvent{Type: "message", Channel: channel, User: "U_ALICE", TS: "100.000003", ThreadTS: "100.000001", Text: "synthetic body"})
				processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, slackEventEnvelope{Type: "event_callback", Event: event}, func() {})
			case "peer":
				processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, peerBotEnvelope(t, "message", "E1", channel, "100.000003", "100.000001", "synthetic body", peerTestPeerBotID, ""), func() {})
			case "batch":
				var batch []pendingChannelInbound
				for _, ts := range []string{"100.000002", "100.000003"} {
					batch = append(batch, pendingChannelInbound{inbound: externalInboundMessage{Conversation: conversationRef{ConversationID: channel}, Actor: externalActor{ID: "U_ALICE", DisplayName: "Example"}, ProviderMessageID: ts, ReplyToMessageID: "100.000001", Text: "synthetic body"}, body: "synthetic body", threadAnchor: formatThreadReplyAnchor("100.000001", "", "")})
				}
				if err := deliverCoalescedBatch(cfg, channel, batch); err != nil {
					t.Fatal(err)
				}
			}
			got := capture.snapshot()
			if len(got) != 1 {
				t.Fatalf("deliveries=%d", len(got))
			}
			m := got[0]
			for _, want := range []string{channel, "#example", "100.000003", "synthetic body", "Context: gc slack read --conversation-id " + channel, "--thread-ts"} {
				if !strings.Contains(m.Text, want) {
					t.Fatalf("missing %q: %s", want, m.Text)
				}
			}
			if m.Actor.ID == "" || m.ProviderMessageID != "100.000003" {
				t.Fatalf("lost structured ids: %+v", m)
			}
			if !strings.Contains(m.Text, m.Actor.ID) {
				t.Fatal("sender id missing from reminder")
			}
			for _, bad := range []string{"Thread context", "buffered context must not appear", "full reply how-to", "--body-file", peerContextHeader} {
				if strings.Contains(m.Text, bad) {
					t.Fatalf("unexpected %q", bad)
				}
			}
			if atomic.LoadInt32(calls) != 0 {
				t.Fatal("minimal fetched thread context")
			}
			if cfg.threadContextCache.lastDeliveredFor("", channel, "100.000001") != "" {
				t.Fatal("minimal advanced full-mode watermark")
			}
			if shape == "thread" && !strings.Contains(m.Text, "parent @Parent: \"synthetic parent\"") {
				t.Fatal("cached parent missing")
			}
			if shape != "peer" {
				if items, _ := cfg.peerContext.flush(channel); len(items) != 0 {
					t.Fatal("peer buffer not drained")
				}
			}
			if shape == "batch" && !strings.Contains(m.Text, "100.000002") {
				t.Fatal("older batch id missing")
			}
		})
	}
}

func TestThreadWindowKeepsRootAndNewestReplies(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		start, end := 1, 20
		if r.URL.Query().Get("cursor") == "page2" {
			start, end = 21, 25
		}
		response := slackConversationsRepliesResp{OK: true}
		for i := start; i <= end; i++ {
			response.Messages = append(response.Messages, slackThreadMessage{TS: fmt.Sprintf("100.%06d", i), User: "U_TEST", Text: "synthetic"})
		}
		if start == 1 {
			response.HasMore = true
			response.ResponseMetadata.NextCursor = "page2"
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()
	withSlackAPIStub(t, srv)
	got, err := fetchThreadReplies(context.Background(), "test", "C1", "100.000001", 20)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(got) != 20 || got[0].TS != "100.000001" || got[1].TS != "100.000007" || got[19].TS != "100.000025" {
		t.Fatalf("wrong window: calls=%d, %+v", calls, got)
	}
	got, err = fetchThreadReplies(context.Background(), "test", "C1", "100.000001", 20, "100.000010")
	if err != nil || len(got) != 9 || got[len(got)-1].TS != "100.000009" {
		t.Fatalf("current/later replies displaced priors: %v %+v", err, got)
	}
}

func TestThreadWindowRejectsBrokenPagination(t *testing.T) {
	for _, cursor := range []string{"", "repeat"} {
		t.Run(cursor, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "has_more": true, "response_metadata": map[string]string{"next_cursor": cursor}})
			}))
			defer srv.Close()
			withSlackAPIStub(t, srv)
			if _, err := fetchThreadReplies(context.Background(), "test", "C1", "1.0", 20); err == nil {
				t.Fatal("accepted incomplete window")
			}
		})
	}
}

func TestThreadContextIncidentMechanisms(t *testing.T) {
	cache := newThreadContextCache()
	cache.markDelivered("", "C1", "100.000001", "100.000003")
	replies := []slackThreadMessage{{TS: "100.000001", User: "U_ROOT", Text: "parent"}, {TS: "100.000002", User: "U_OLD", Text: "earlier context"}, {TS: "100.000004", BotID: "B_PEER", Text: strings.Repeat("word ", 339)}}
	window := injectionWindow{Known: true}
	got := formatThreadContextPreamble(replies, "100.000001", "100.000005", cache.lastDeliveredFor("", "C1", "100.000001"), nil, nil, func(slackThreadMessage) (string, bool) { return "Peer", true }, &window)
	if !strings.Contains(got, "Thread context (2 earlier messages)") || strings.Contains(got, "earlier context") || !strings.Contains(got, strings.Repeat("word ", 55)+"word…") || strings.Contains(got, strings.Repeat("word ", 57)) {
		t.Fatalf("incident mechanisms changed: %s", got)
	}
	if window.Oldest != "100.000001" || window.Newest != "100.000004" {
		t.Fatalf("wrong window %+v", window)
	}
	// Same target after a session reset still reads the watermark; a different
	// target does not. The adapter has no session-reset signal for this cache.
	if cache.lastDeliveredFor("new-target", "C1", "100.000001") != "" {
		t.Fatal("target scopes conflated")
	}
	peer := formatPeerContextLine(peerContextItem{Text: strings.Repeat("x", 2050)})
	if !strings.Contains(peer, strings.Repeat("x", 2000)+" …[truncated]") {
		t.Fatal("buffer clip reproduction missing")
	}
}

func TestInboundJournalAcceptedDeliveryOnlyAndNoText(t *testing.T) {
	dir := t.TempDir()
	status := http.StatusAccepted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(`{"delivery":{"id":"r1","status":"delivered","members":[{"session_id":"session1","status":"delivered"}]}}`))
	}))
	defer srv.Close()
	cfg := config{gcAPIBase: srv.URL, cityName: "test", injectionMode: "minimal", inboundJournal: &inboundJournal{dir: dir}}
	msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, ProviderMessageID: "100.2", ReplyToMessageID: "100.1", Actor: externalActor{ID: "U_PRIVATE", DisplayName: "private author"}, Text: "private message text"}
	if _, err := postInboundWithReceipt(cfg, msg); err != nil {
		t.Fatal(err)
	}
	status = http.StatusBadRequest
	if _, err := postInboundWithReceipt(cfg, msg); err == nil {
		t.Fatal("expected rejection")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files=%v", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "\n") != 1 {
		t.Fatal("expected one record per accepted delivery")
	}
	for _, bad := range []string{msg.Text, msg.Actor.ID, msg.Actor.DisplayName} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("journal leaked %q", bad)
		}
	}
	var rec inboundJournalRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Bytes != len(msg.Text) || rec.Channel != "C1" || rec.MessageTS != "100.2" || rec.ThreadTS != "100.1" || rec.Mode != "minimal" || len(rec.TargetSessions) != 1 {
		t.Fatalf("incorrect metadata: %+v", rec)
	}
}

func TestInboundJournalRotationRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	j := &inboundJournal{dir: dir, retentionDays: 2, maxBytes: 1, now: func() time.Time { return now }}
	if err := j.append(inboundJournalRecord{}); err != nil {
		t.Fatal(err)
	}
	if err := j.append(inboundJournalRecord{}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "20261025*.jsonl"))
	if len(files) != 2 {
		t.Fatalf("rotation files=%v", files)
	}
	now = now.AddDate(0, 0, 2)
	if err := j.append(inboundJournalRecord{}); err != nil {
		t.Fatal(err)
	}
	files, _ = filepath.Glob(filepath.Join(dir, "20261025*.jsonl"))
	if len(files) != 0 {
		t.Fatal("expired segments retained")
	}
}

func TestInjectionWindowSurvivesSpoolReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	s := newInboundSpool(path)
	p := testPending("C1", "100.3", "synthetic body")
	p.inbound.injectionWindows = []injectionWindow{{MessageTS: "100.3", ThreadTS: "100.1", Oldest: "100.1", Newest: "100.2", Collapsed: 2, Known: true}}
	p.inbound.injectionLeanWindows = []injectionWindow{{MessageTS: "100.3", ThreadTS: "100.1", Oldest: "100.1", Newest: "100.1", Collapsed: 2, Known: true}}
	if !s.spillBatch("C1", []pendingChannelInbound{p}) {
		t.Fatal("spill failed")
	}
	c := newInboundCoalescer(time.Hour, nil)
	deliver, got := collectingDeliver()
	c.deliver = deliver
	if s.replayInto(c) != 1 {
		t.Fatal("replay failed")
	}
	c.flushAll()
	select {
	case batch := <-got:
		m := batch[0].inbound
		if len(m.injectionWindows) != 1 || m.injectionWindows[0] != p.inbound.injectionWindows[0] || len(m.injectionLeanWindows) != 1 || m.injectionLeanWindows[0] != p.inbound.injectionLeanWindows[0] {
			t.Fatal("spool lost window metadata")
		}
	case <-time.After(time.Second):
		t.Fatal("no replay")
	}
}

func TestInjectionWindowBudgetSelection(t *testing.T) {
	m := externalInboundMessage{injectionWindows: []injectionWindow{{Oldest: "1", Newest: "3", Collapsed: 2, Known: true}}, injectionLeanWindows: []injectionWindow{{Oldest: "1", Newest: "2", Collapsed: 2, Known: true}}}
	if got := selectedInjectionWindows(m, "full priors", "human priors", "anchor\nhuman priors\nbody"); len(got) != 1 || got[0].Newest != "2" {
		t.Fatal("lean context not recorded")
	}
	if got := selectedInjectionWindows(m, "full priors", "human priors", "anchor\nbody"); len(got) != 1 || got[0].Oldest != "" || got[0].Newest != "" || got[0].Collapsed != 0 {
		t.Fatal("omitted context recorded as quoted")
	}
}

func TestInboundJournalHeldReceiptConclusion(t *testing.T) {
	poll := newScriptedPoll()
	r := concludedReceipt("held1", "delivered", 10, 10)
	r.members = []deliveryReceiptMember{{SessionID: "session1", Status: "delivered"}}
	poll.on("held1", receiptPollAnswer{state: receiptPollPending}, receiptPollAnswer{state: receiptPollConcluded, receipt: r})
	f := testFollowUps(poll.poll, nil)
	j := &inboundJournal{dir: t.TempDir()}
	msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, ProviderMessageID: "100.2", Text: "synthetic"}
	j.record(config{}, msg, pendingReceipt("held1"))
	f.note(heldDelivery{receiptID: "held1", observeReceipt: func(r deliveryReceipt) { j.record(config{}, msg, r, "concluded") }})
	if !f.wait(time.Second) {
		t.Fatal("receipt did not conclude")
	}
	files, _ := filepath.Glob(filepath.Join(j.dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatal("journal missing")
	}
	data, _ := os.ReadFile(files[0])
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want acceptance and conclusion; lines=%d", len(lines))
	}
	var rec inboundJournalRecord
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Phase != "concluded" || rec.ReceiptID != "held1" || len(rec.TargetSessions) != 1 || rec.TargetSessions[0] != "session1" {
		t.Fatalf("missing final target: %+v", rec)
	}
}

func TestMinimalDirectSessionPathsHaveCoordinates(t *testing.T) {
	cfg := config{injectionMode: "minimal"}
	m := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, Actor: externalActor{ID: "U_TEST", DisplayName: "Example"}, ProviderMessageID: "100.2", ReplyToMessageID: "100.1", Text: "synthetic body"}
	for _, prepared := range []bool{false, true} {
		msg := m
		if prepared {
			msg.Text = minimalDispatchText(cfg, msg)
			msg.injectionPrepared = true
		}
		for _, body := range []string{renderAliasDispatch(cfg, msg, "example"), formatMentionOnlyReminder(cfg, msg, "mention", "example")} {
			for _, want := range []string{"C1", "U_TEST", "Example", "100.2", "100.1", "synthetic body", "Context: gc slack read", "reply-to 100.1"} {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %q", want)
				}
			}
			if strings.Count(body, "Context: gc slack read") != 1 || strings.Contains(body, "--body-file") {
				t.Fatal("double decoration or reply how-to")
			}
		}
	}
}

func TestMentionOnlyJournalRecordsTargetAndRenderedBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	j := &inboundJournal{dir: t.TempDir()}
	cfg := config{gcAPIBase: srv.URL, cityName: "test", injectionMode: "minimal", inboundJournal: j}
	m := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, ProviderMessageID: "100.2", ReplyToMessageID: "100.1", Text: "private payload"}
	body := formatMentionOnlyReminder(cfg, m, "mention", "example")
	if _, ok := postMentionOnlyReminder(cfg, "session1", body, nil, m); !ok {
		t.Fatal("delivery failed")
	}
	files, _ := filepath.Glob(filepath.Join(j.dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatal("journal missing")
	}
	data, _ := os.ReadFile(files[0])
	var r inboundJournalRecord
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.Bytes != len(body) || len(r.TargetSessions) != 1 || r.TargetSessions[0] != "session1" || r.MessageTS != "100.2" || strings.Contains(string(data), m.Text) {
		t.Fatalf("wrong journal: %+v", r)
	}
}

func TestOwnershipScanRetainsEarlyAndLateEvidence(t *testing.T) {
	for _, ownIndex := range []int{2, ownThreadScanLimit + 2} {
		t.Run(fmt.Sprint(ownIndex), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := 0
				if r.URL.Query().Get("cursor") == "page2" {
					page = 1
				}
				sr := slackConversationsRepliesResp{OK: true}
				for i := page*ownThreadScanLimit + 1; i <= (page+1)*ownThreadScanLimit; i++ {
					user := "U_OTHER"
					if i == ownIndex {
						user = "U_OWN"
					}
					sr.Messages = append(sr.Messages, slackThreadMessage{TS: fmt.Sprintf("100.%06d", i), User: user})
				}
				if page == 0 {
					sr.HasMore = true
					sr.ResponseMetadata.NextCursor = "page2"
				}
				json.NewEncoder(w).Encode(sr)
			}))
			defer srv.Close()
			withSlackAPIStub(t, srv)
			found, err := fetchThreadHasOwnBotPost(context.Background(), "test", "C1", "100.000001", "U_OWN", "101.000001")
			if err != nil || !found {
				t.Fatalf("lost ownership at %d: %v", ownIndex, err)
			}
		})
	}
}

func TestBatchJournalIncludesDeletedAndUnknownMembers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	defer srv.Close()
	j := &inboundJournal{dir: t.TempDir()}
	cfg := coalescingTestConfig(srv.URL, 0)
	cfg.inboundJournal = j
	p := testPending("C1", "100.2", "synthetic")
	p.inbound.injectionWindows = []injectionWindow{{MessageTS: "100.2", Oldest: "99.1", Newest: "99.2", Collapsed: 3, Known: true}}
	applyDeletion(&p)
	if err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{p, testPending("C1", "100.3", "legacy")}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(j.dir, "*.jsonl"))
	data, _ := os.ReadFile(files[0])
	var r inboundJournalRecord
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Windows) != 2 || !r.Windows[0].Known || r.Windows[0].Oldest != "" || r.Windows[0].Collapsed != 0 || r.Windows[1].Known || r.Windows[1].MessageTS != "100.3" {
		t.Fatalf("false/missing windows: %+v", r.Windows)
	}
}

func TestMinimalPeerWithoutBotUserIDKeepsBotID(t *testing.T) {
	capture := &inboundCapture{}
	srv := httptest.NewServer(capture.handler())
	defer srv.Close()
	resolver := &stubAuthorResolver{info: companyBotInfo{AppID: "A_OTHER"}, outcome: botResolveOK}
	registry := newTestPeerBotsRegistry(t, `{"peers":[{"label":"Example peer","bot_id":"B_TEST","wake":true}]}`)
	cfg := peerTestConfig(srv.URL, registry, resolver)
	cfg.injectionMode = "minimal"
	cfg.slackAppID = "A_SELF"
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, peerBotEnvelope(t, "message", "E1", "C1", "100.2", "100.1", "synthetic body", "B_TEST", "A_OTHER"), func() {})
	got := capture.snapshot()
	if len(got) != 1 || got[0].Actor.ID != "B_TEST" || !strings.Contains(got[0].Text, "B_TEST") {
		t.Fatalf("missing bot coordinate: %+v", got)
	}
}

func TestMinimalCompanyMentionPreservesFilesWithOversizedCaption(t *testing.T) {
	gc := newFakeGC(t)
	df, bf := baseDirectoryFile(), baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())
	h.gw.cfg.injectionMode = "minimal"
	h.gw.cfg.reminderTextBudget = defaultReminderTextBudget
	ev := humanMessage("1700000000.000500", "<@"+companyMOBotUserID+"> "+strings.Repeat("caption ", 1000))
	ev.Files = []slackFile{{ID: "F_TEST", Name: "synthetic-report.txt", MIMEType: "text/plain"}}
	admitCompanyRoomMessage(t, h, ev)
	h.wait()
	if !awaitWaitGroup(&h.gw.mentionOnlyWG, 3*time.Second) {
		t.Fatal("mention-only delivery did not finish")
	}
	found := false
	for _, call := range gc.sessionCalls() {
		if !strings.Contains(call.path, "/session/outsider-session/") {
			continue
		}
		found = true
		for _, want := range []string{"synthetic-report.txt", "F_TEST", "message trimmed", "Context: gc slack read"} {
			if !strings.Contains(call.body, want) {
				t.Fatalf("missing protected content %q", want)
			}
		}
	}
	if !found {
		t.Fatal("mention-only session not reached")
	}
}

func TestMinimalParentMutationDropsCachedExcerpt(t *testing.T) {
	for _, subtype := range []string{"message_changed", "message_deleted"} {
		t.Run(subtype, func(t *testing.T) {
			capture := &inboundCapture{}
			gc := httptest.NewServer(capture.handler())
			defer gc.Close()
			cfg := coalescingTestConfig(gc.URL, 0)
			cfg.injectionMode = "minimal"
			cfg.injectionParents = &injectionParents{}
			cfg.injectionParents.remember("C1", "100.1", "Parent", "superseded excerpt")
			raw, _ := json.Marshal(map[string]any{"type": "message", "subtype": subtype, "channel": "C1", "deleted_ts": "100.1", "message": map[string]string{"ts": "100.1", "text": "replacement excerpt"}})
			processSlackEvent(cfg, nil, nil, nil, nil, nil, slackEventEnvelope{Type: "event_callback", Event: raw}, func() {})
			reply, _ := json.Marshal(slackMessageEvent{Type: "message", Channel: "C1", User: "U_TEST", TS: "100.2", ThreadTS: "100.1", Text: "synthetic reply"})
			processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, slackEventEnvelope{Type: "event_callback", Event: reply}, func() {})
			got := capture.snapshot()
			if len(got) != 1 || strings.Contains(got[0].Text, "excerpt") || !strings.Contains(got[0].Text, "[reply in thread 100.1]") {
				t.Fatalf("stale parent or lost thread coordinate: %+v", got)
			}
		})
	}
}

func TestInjectionParentInvalidationSuppressesLateRemember(t *testing.T) {
	for _, warm := range []bool{false, true} {
		c := &injectionParents{}
		if warm {
			c.remember("C1", "100.1", "Parent", "old excerpt")
		}
		c.invalidateMutation(slackMessageEvent{Subtype: "message_deleted", Channel: "C1", DeletedTS: "100.1"}, nil)
		// Model an older root handler completing after the deletion handler.
		c.remember("C1", "100.1", "Parent", "old excerpt")
		if author, line := c.get("C1", "100.1"); author != "" || line != "" {
			t.Fatalf("late root restored deleted excerpt (warm=%v)", warm)
		}
		for i := 0; i < threadContextCacheMaxEntries; i++ {
			c.invalidateMutation(slackMessageEvent{Subtype: "message_deleted", Channel: "C1", DeletedTS: fmt.Sprint(i)}, nil)
		}
		if len(c.entries) != threadContextCacheMaxEntries {
			t.Fatalf("unbounded tombstones: %d", len(c.entries))
		}
	}
}

func TestReactionTargetStopsAtFirstPage(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("cursor") != "" {
			http.Error(w, "later page unavailable", http.StatusTooManyRequests)
			return
		}
		page := slackConversationsRepliesResp{OK: true, HasMore: true, Messages: []slackThreadMessage{{TS: "100.1", Text: "synthetic target"}}}
		page.ResponseMetadata.NextCursor = "page2"
		json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()
	withSlackAPIStub(t, srv)
	snippet, root := fetchReactionTargetContext(config{slackBotToken: "test"}, "C1", "100.1")
	if snippet != "synthetic target" || root != "" || calls.Load() != 1 {
		t.Fatalf("target=%q root=%q requests=%d", snippet, root, calls.Load())
	}
}

func TestMinimalReminderBudgetIncludesPointer(t *testing.T) {
	body := strings.Repeat("synthetic message ", 1000)
	msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, Actor: externalActor{ID: "U_TEST"}, ProviderMessageID: "100.2", ReplyToMessageID: "100.1"}
	for _, budget := range []int{defaultReminderTextBudget, 1000, 1, 0} {
		got := minimalReminder(config{injectionMode: "minimal", reminderTextBudget: budget}, msg, body, "", "")
		if !strings.HasSuffix(got, "Context: gc slack read --conversation-id C1 --thread-ts 100.1") || !strings.Contains(got, body[:reminderBodyHeadRunes]) {
			t.Fatalf("budget %d lost pointer or protected head", budget)
		}
		if budget >= 1000 && len(got) > budget {
			t.Fatalf("budget %d rendered %d bytes", budget, len(got))
		}
		if budget > 0 && !strings.Contains(got, "message trimmed") {
			t.Fatalf("positive budget %d disabled trimming", budget)
		}
		if budget == 0 && !strings.Contains(got, body) {
			t.Fatal("disabled budget clipped body")
		}
	}
}

func TestMinimalReactionWithoutRootUsesChannelPointer(t *testing.T) {
	capture := &inboundCapture{}
	gc := httptest.NewServer(capture.handler())
	defer gc.Close()
	cfg := coalescingTestConfig(gc.URL, 0)
	cfg.injectionMode = "minimal"
	p := testPending("C1", "100.299", "reacted to message 100.2")
	p.inbound.Actor = externalActor{ID: "U_TEST", DisplayName: reactionActorPrefix + "Example", IsBot: true}
	if err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{p}); err != nil {
		t.Fatal(err)
	}
	got := capture.snapshot()
	if len(got) != 1 || !strings.Contains(got[0].Text, "Context: gc slack read --conversation-id C1") || strings.Contains(got[0].Text, "--thread-ts") {
		t.Fatalf("reaction pointer names a non-message: %+v", got)
	}
}
