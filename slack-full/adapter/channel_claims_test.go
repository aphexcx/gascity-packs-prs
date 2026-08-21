package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Coverage for the per-(channel, ts) channel-audience delivery claims
// (gp-ios, pc_c920ff5fe90c): a bot-mention twin pair — `message` +
// `app_mention`, same ts, distinct event_ids — must deliver the
// channel copy exactly once, whether the twins race concurrently,
// arrive sequentially, or split across the urgent and coalesced paths.

// flakyInboundStub captures /extmsg/inbound POSTs and can fail the
// first N of them with a 500 so takeover paths can be exercised.
type flakyInboundStub struct {
	mu       sync.Mutex
	failNext int
	attempts int
	inbounds []externalInboundMessage
}

func (s *flakyInboundStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Message externalInboundMessage `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		s.mu.Lock()
		s.attempts++
		if s.failNext > 0 {
			s.failNext--
			s.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s.inbounds = append(s.inbounds, env.Message)
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *flakyInboundStub) snapshot() []externalInboundMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]externalInboundMessage, len(s.inbounds))
	copy(out, s.inbounds)
	return out
}

func (s *flakyInboundStub) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// claimsTestConfig is the minimal urgent-path config with the claims
// cache wired: no bot token / busy reaction (no Slack API calls), no
// thread-context cache (no fetches), nil coalescer (nil-safe).
func claimsTestConfig(gcURL string) config {
	return config{
		gcAPIBase:     gcURL,
		cityName:      "test-city",
		provider:      "slack",
		accountID:     "T1",
		handlePrefix:  "@",
		dispatchSem:   defaultTestDispatchSem,
		deliveredIDs:  newDeliveredIDs(),
		channelClaims: newEventDedupCache(eventDedupTTL),
	}
}

// Both twins race the urgent path concurrently — the live 2026-08-20
// 06:44:02 shape, where the bound session read the same message id
// twice in one turn (bare + thread-context decorated). The claim
// serializes them: exactly one channel POST reaches gc.
func TestChannelClaims_ConcurrentTwinsDeliverOnce(t *testing.T) {
	stub := &flakyInboundStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := claimsTestConfig(gcSrv.URL)
	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> please take a look"

	var wg sync.WaitGroup
	for i, eventType := range []string{"message", "app_mention"} {
		wg.Add(1)
		env := botMentionEnvelope(t, eventType, "Ev"+string(rune('1'+i)), "C1", "100.000001", "", text, true)
		go func(env slackEventEnvelope) {
			defer wg.Done()
			processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
		}(env)
	}
	wg.Wait()

	if got := stub.snapshot(); len(got) != 1 {
		t.Fatalf("gc received %d channel inbounds for one ts, want exactly 1", len(got))
	}
}

// The trailing twin arrives after the leading one fully delivered —
// its claim reads committed and the channel copy is skipped without a
// second POST.
func TestChannelClaims_SequentialTwinSkipped(t *testing.T) {
	stub := &flakyInboundStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := claimsTestConfig(gcSrv.URL)
	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> sequential twin check"

	env1 := botMentionEnvelope(t, "message", "Ev1", "C1", "100.000002", "", text, true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env1, func() {})
	if got := stub.snapshot(); len(got) != 1 {
		t.Fatalf("after first twin: %d inbounds, want 1", len(got))
	}

	env2 := botMentionEnvelope(t, "app_mention", "Ev2", "C1", "100.000002", "", text, true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env2, func() {})
	if got := stub.snapshot(); len(got) != 1 {
		t.Fatalf("after trailing twin: %d inbounds, want still 1 (twin must skip)", len(got))
	}
}

// The claim owner's POST fails: the parked twin must take over and
// deliver — a skip there would lose the message (Slack already got
// its 200 for both events). Exactly one SUCCESSFUL delivery lands.
func TestChannelClaims_FailedOwnerHandsOverToParkedTwin(t *testing.T) {
	stub := &flakyInboundStub{failNext: 1}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := claimsTestConfig(gcSrv.URL)
	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> takeover check"

	var wg sync.WaitGroup
	for i, eventType := range []string{"message", "app_mention"} {
		wg.Add(1)
		env := botMentionEnvelope(t, eventType, "Ev"+string(rune('1'+i)), "C1", "100.000003", "", text, true)
		go func(env slackEventEnvelope) {
			defer wg.Done()
			processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
		}(env)
	}
	wg.Wait()

	if got := stub.snapshot(); len(got) != 1 {
		t.Fatalf("gc received %d successful inbounds, want exactly 1 (failed owner, twin takeover)", len(got))
	}
	if attempts := stub.attemptCount(); attempts != 2 {
		t.Errorf("POST attempts = %d, want 2 (one failure + one takeover success)", attempts)
	}
}

// A batch member whose (channel, ts) claim an urgent twin already
// committed is dropped at batch-delivery time even when the
// deliveredIDs record hasn't landed yet — the claims close the sliver
// between a twin's commit and its deliveredIDs record.
func TestCoalescer_BatchSkipsClaimCommittedMember(t *testing.T) {
	stub := &flakyInboundStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	deliverCfg := cfg
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) bool {
		return deliverCoalescedBatch(deliverCfg, channel, batch)
	}

	// Simulate an urgent twin that committed ts Y's claim but has not
	// (yet) recorded it in deliveredIDs.
	keyY := channelDeliveryClaimKey("C1", "100.000020")
	if proceed, _ := cfg.channelClaims.begin(keyY); !proceed {
		t.Fatal("setup: could not claim Y")
	}
	cfg.channelClaims.commit(keyY)

	batch := []pendingChannelInbound{
		{inbound: externalInboundMessage{ProviderMessageID: "100.000010", Text: "keep me",
			Conversation: conversationRef{ConversationID: "C1", Kind: "room"}}},
		{inbound: externalInboundMessage{ProviderMessageID: "100.000020", Text: "already delivered by twin",
			Conversation: conversationRef{ConversationID: "C1", Kind: "room"}}},
	}
	if ok := deliverCoalescedBatch(cfg, "C1", batch); !ok {
		t.Fatal("batch delivery reported failure")
	}

	got := stub.snapshot()
	if len(got) != 1 {
		t.Fatalf("gc received %d inbounds, want 1", len(got))
	}
	if got[0].ProviderMessageID != "100.000010" {
		t.Errorf("delivered provider id = %s, want 100.000010 (claim-committed member dropped)", got[0].ProviderMessageID)
	}
	// The channel's first delivery legitimately appends the once-per-
	// channel reply how-to; the survivor's own text must lead.
	if !strings.HasPrefix(got[0].Text, "keep me") {
		t.Errorf("delivered text = %q, want prefix %q (only the survivor's content)", got[0].Text, "keep me")
	}
	if strings.Contains(got[0].Text, "already delivered by twin") {
		t.Errorf("delivered text still carries the claim-committed member: %q", got[0].Text)
	}
}

// Regression (gp-ios): deliverCoalescedBatch used to compact the batch
// IN PLACE (batch[:0] aliasing) while its caller restores the original
// slice on failure — a dropped member plus a failed POST re-queued a
// corrupted batch (tail duplicated, dropped-position members lost).
// The filters now build fresh slices: after a failed delivery of a
// batch with an already-delivered member, the restored buffer must
// hold every original member intact and in order.
func TestCoalescer_FailedBatchRestoreKeepsMembersIntact(t *testing.T) {
	stub := &flakyInboundStub{failNext: 100} // every POST fails
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	deliverCfg := cfg
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) bool {
		return deliverCoalescedBatch(deliverCfg, channel, batch)
	}

	for _, ts := range []string{"100.000030", "100.000031", "100.000032"} {
		cfg.coalescer.enqueue("C1", pendingChannelInbound{inbound: externalInboundMessage{
			ProviderMessageID: ts, Text: "msg " + ts,
			Conversation: conversationRef{ConversationID: "C1", Kind: "room"},
		}})
	}
	// The first member was already delivered to the channel audience —
	// the delivery-time filter drops it from the batch.
	cfg.deliveredIDs.record("", "C1", "100.000030")

	cfg.coalescer.flushAll() // deliver fails; batch restored

	cfg.coalescer.mu.Lock()
	pending := append([]pendingChannelInbound(nil), cfg.coalescer.pending["C1"]...)
	cfg.coalescer.mu.Unlock()

	want := []string{"100.000030", "100.000031", "100.000032"}
	if len(pending) != len(want) {
		t.Fatalf("restored buffer holds %d members, want %d", len(pending), len(want))
	}
	for i, ts := range want {
		if pending[i].inbound.ProviderMessageID != ts {
			t.Errorf("restored[%d] = %s, want %s (restore must not see in-place compaction)",
				i, pending[i].inbound.ProviderMessageID, ts)
		}
		if wantText := "msg " + ts; pending[i].inbound.Text != wantText {
			t.Errorf("restored[%d] text = %q, want %q", i, pending[i].inbound.Text, wantText)
		}
	}
}

// flakyRouterStub serves the gc endpoints the alias-dispatch path
// touches, failing the first N session-message POSTs so the claim
// takeover leg can be exercised.
type flakyRouterStub struct {
	mu              sync.Mutex
	failMessages    int
	messageAttempts int
	inbounds        []externalInboundMessage
	sessionMessages []string
}

func (g *flakyRouterStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/extmsg/inbound"):
			var env struct {
				Message externalInboundMessage `json:"message"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			g.mu.Lock()
			g.inbounds = append(g.inbounds, env.Message)
			g.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		case strings.Contains(r.URL.Path, "/extmsg/bindings"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items": []}`))
		case strings.Contains(r.URL.Path, "/messages"):
			var req gcSessionMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			g.mu.Lock()
			g.messageAttempts++
			if g.failMessages > 0 {
				g.failMessages--
				g.mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			g.sessionMessages = append(g.sessionMessages, req.Message)
			g.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}
}

func (g *flakyRouterStub) counts() (inbounds, injected, attempts int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.inbounds), len(g.sessionMessages), g.messageAttempts
}

// A targeted twin pair (codex review P1): the alias injection carries
// its own claim, so when the owning twin's session-message POST fails,
// the other twin — whose channel copy was skipped — takes the
// injection over instead of being discarded. Exactly one channel copy
// and exactly one successful injection land.
func TestChannelClaims_AliasInjectionRecoveredByTwin(t *testing.T) {
	stub := &flakyRouterStub{failMessages: 1}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := claimsTestConfig(gcSrv.URL)
	cfg.bindingCheck = newBindingCheckCache()
	aliasReg := newTestHandleAliasRegistry(t)
	if err := aliasReg.Set("mayor", "sess-mayor-1"); err != nil {
		t.Fatalf("alias set: %v", err)
	}

	var wg sync.WaitGroup
	for i, eventType := range []string{"message", "app_mention"} {
		wg.Add(1)
		env := botMentionEnvelope(t, eventType, "Ev"+string(rune('1'+i)), "C1", "100.000040", "",
			"@mayor: please handle this", true)
		go func(env slackEventEnvelope) {
			defer wg.Done()
			processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
		}(env)
	}
	wg.Wait()

	waitFor(t, "one successful alias injection", func() bool {
		_, injected, attempts := stub.counts()
		return injected == 1 && attempts == 2
	})
	inbounds, injected, attempts := stub.counts()
	if inbounds != 1 {
		t.Errorf("channel inbounds = %d, want 1", inbounds)
	}
	if injected != 1 || attempts != 2 {
		t.Errorf("alias injections = %d (attempts %d), want 1 successful of 2 attempts", injected, attempts)
	}
}

// A trailing twin that skips its channel copy must not touch the
// thread-context machinery (codex review P2): pre-claim ordering let a
// losing twin advance threadContextCache without delivering, silently
// dropping the decorated copy. The claim now precedes the preamble, so
// the skipping twin performs no conversations.replies fetch at all.
func TestChannelClaims_SkippingTwinLeavesThreadContextAlone(t *testing.T) {
	var fetches atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/conversations.replies") {
			fetches.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok": true, "messages": [
				{"ts": "100.000001", "user": "U_ROOT", "text": "thread root"},
				{"ts": "100.000050", "user": "U_ALICE", "text": "the ask"}
			]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(slackSrv.Close)
	withSlackAPIStub(t, slackSrv)

	stub := &flakyInboundStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := claimsTestConfig(gcSrv.URL)
	cfg.slackBotToken = "xoxb-fake"
	cfg.threadContextCache = newThreadContextCache()
	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> the ask"

	env1 := botMentionEnvelope(t, "message", "Ev1", "C1", "100.000050", "100.000001", text, true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env1, func() {})
	if got := stub.snapshot(); len(got) != 1 {
		t.Fatalf("after first twin: %d inbounds, want 1", len(got))
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("after first twin: %d thread fetches, want 1", n)
	}

	env2 := botMentionEnvelope(t, "app_mention", "Ev2", "C1", "100.000050", "100.000001", text, true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env2, func() {})
	if got := stub.snapshot(); len(got) != 1 {
		t.Fatalf("after trailing twin: %d inbounds, want still 1", len(got))
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("trailing twin fetched thread context (%d fetches, want 1) — a skipping twin must not touch the cache", n)
	}
}
