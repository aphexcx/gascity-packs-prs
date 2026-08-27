package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Integration coverage for the gp-729 token-efficiency pass at the
// processSlackEvent seam: burst coalescing (item 1), the once-per-
// channel reply how-to (item 3), and alias-dispatch turn-dedup (item 5).

// gcRouterStub serves the three gc endpoints processSlackEvent touches:
// /extmsg/inbound (captured), /session/{id}/messages (captured), and
// /extmsg/bindings (canned payload).
type gcRouterStub struct {
	mu              sync.Mutex
	inbounds        []externalInboundMessage
	sessionMessages []string
	bindingsPayload string
}

func (g *gcRouterStub) handler() http.HandlerFunc {
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
			payload := g.bindingsPayload
			if payload == "" {
				payload = `{"items": []}`
			}
			_, _ = w.Write([]byte(payload))
		case strings.Contains(r.URL.Path, "/messages"):
			var req gcSessionMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			g.mu.Lock()
			g.sessionMessages = append(g.sessionMessages, req.Message)
			g.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}
}

func (g *gcRouterStub) snapshotInbounds() []externalInboundMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]externalInboundMessage, len(g.inbounds))
	copy(out, g.inbounds)
	return out
}

func (g *gcRouterStub) snapshotSessionMessages() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.sessionMessages))
	copy(out, g.sessionMessages)
	return out
}

// coalescingTestConfig wires a config the way main() does: coalescer
// deliver closure over the completed cfg copy.
func coalescingTestConfig(gcURL string, window time.Duration) config {
	cfg := config{
		gcAPIBase:    gcURL,
		cityName:     "test-city",
		provider:     "slack",
		accountID:    "T1",
		handlePrefix: "@",
		dispatchSem:  defaultTestDispatchSem,
		peerContext:  newPeerContextBuffer(),
		deliveredIDs: newDeliveredIDs(),
		replyHelp:    newOncePerChannel(),
		bindingCheck: newBindingCheckCache(),
		coalescer:    newInboundCoalescer(window, nil),
	}
	deliverCfg := cfg
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
		return deliverCoalescedBatch(deliverCfg, channel, batch)
	}
	return cfg
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestCoalescer_BurstDeliversAsOneInbound(t *testing.T) {
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, 40*time.Millisecond)
	aliasReg := newTestHandleAliasRegistry(t)
	for i, text := range []string{"one thought", "split across", "three sends"} {
		env := humanEnvelope(t, "Ev"+string(rune('1'+i)), "C1", "100.00000"+string(rune('1'+i)), text)
		processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
	}
	// Nothing forwards inside the window...
	if got := stub.snapshotInbounds(); len(got) != 0 {
		t.Fatalf("messages forwarded before the window elapsed: %d", len(got))
	}
	// ...and the whole burst lands as ONE inbound after it.
	waitFor(t, "coalesced flush", func() bool { return len(stub.snapshotInbounds()) == 1 })
	got := stub.snapshotInbounds()[0]
	for _, want := range []string{"3 messages in C1, coalesced", "one thought", "split across", "three sends"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("coalesced text missing %q:\n%s", want, got.Text)
		}
	}
	if got.ProviderMessageID != "100.000003" {
		t.Fatalf("envelope ts = %q, want the newest 100.000003", got.ProviderMessageID)
	}
	// Item 3: the channel's first delivery carries the full how-to once.
	if !strings.Contains(got.Text, "full reply how-to") {
		t.Fatalf("first delivery missing help block:\n%s", got.Text)
	}
	if len(stub.snapshotInbounds()) != 1 {
		t.Fatalf("burst must deliver exactly once")
	}
}

func TestCoalescer_DMsNeverBuffer(t *testing.T) {
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	// Hour-long window: if a DM entered the buffer this test would see
	// zero inbounds.
	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	env := humanEnvelope(t, "Ev1", "D0DMCHANNEL", "100.000001", "direct message")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
	got := stub.snapshotInbounds()
	if len(got) != 1 {
		t.Fatalf("DM must forward immediately, captured %d", len(got))
	}
	if !strings.Contains(got[0].Text, "direct message") {
		t.Fatalf("unexpected DM text: %q", got[0].Text)
	}
}

func TestCoalescer_BotMentionFlushesBufferAheadThenDeliversOwn(t *testing.T) {
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	// Hour-long window: only the urgent flush-ahead can drain it.
	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	aliasReg := newTestHandleAliasRegistry(t)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		humanEnvelope(t, "Ev1", "C1", "100.000001", "buffered chatter"), func() {})
	if got := stub.snapshotInbounds(); len(got) != 0 {
		t.Fatalf("buffered message forwarded early: %d", len(got))
	}

	rawMsg, _ := json.Marshal(slackMessageEvent{
		Type: "app_mention", Channel: "C1", User: "U_ALICE", TS: "100.000002", Text: "urgent ask",
	})
	env := slackEventEnvelope{Type: "event_callback", EventID: "Ev2", Event: rawMsg}
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})

	got := stub.snapshotInbounds()
	if len(got) != 2 {
		t.Fatalf("captured %d inbounds, want 2 (flush-ahead then urgent)", len(got))
	}
	if !strings.Contains(got[0].Text, "buffered chatter") {
		t.Fatalf("first delivery must be the buffered batch:\n%s", got[0].Text)
	}
	if !strings.Contains(got[1].Text, "urgent ask") {
		t.Fatalf("second delivery must be the urgent message:\n%s", got[1].Text)
	}
	// Help block rode with the first (flush-ahead) delivery only.
	if !strings.Contains(got[0].Text, "full reply how-to") || strings.Contains(got[1].Text, "full reply how-to") {
		t.Fatalf("help block must appear exactly once, on the first delivery")
	}
}

func TestCoalescer_UrgentTwinNotDoubleDeliveredViaBatch(t *testing.T) {
	// pc_c920ff5fe90c: a bot-mention pair (message + app_mention, same
	// ts, distinct event ids) can split when the bot user id is unknown:
	// the message twin buffers as plain chatter, the app_mention twin
	// takes the urgent path. The flushed-ahead batch must NOT carry the
	// twin — its batch dedup key can't collide with the urgent copy's
	// "slack-<ts>", so gc would hand the session the same message id
	// twice in one turn with different decoration.
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	aliasReg := newTestHandleAliasRegistry(t)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		humanEnvelope(t, "Ev1", "C1", "100.000001", "buffered chatter"), func() {})
	// The message twin: no envelope authorizations, so the bot user id is
	// unknown and the raw mention text does not read as a bot mention.
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		humanEnvelope(t, "Ev2", "C1", "100.000002", "urgent ask"), func() {})

	rawMsg, _ := json.Marshal(slackMessageEvent{
		Type: "app_mention", Channel: "C1", User: "U_ALICE", TS: "100.000002", Text: "urgent ask",
	})
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		slackEventEnvelope{Type: "event_callback", EventID: "Ev3", Event: rawMsg}, func() {})

	got := stub.snapshotInbounds()
	if len(got) != 2 {
		t.Fatalf("captured %d inbounds, want 2 (flush-ahead then urgent)", len(got))
	}
	if !strings.Contains(got[0].Text, "buffered chatter") || strings.Contains(got[0].Text, "urgent ask") {
		t.Fatalf("flush-ahead batch must hold the chatter and drop the twin:\n%s", got[0].Text)
	}
	if got[0].DedupKey != "slack-100.000001" {
		t.Fatalf("twin-stripped single-message batch keeps its own dedup key, got %q", got[0].DedupKey)
	}
	if !strings.Contains(got[1].Text, "urgent ask") || got[1].DedupKey != "slack-100.000002" {
		t.Fatalf("urgent copy must deliver with its own key: %q / %q", got[1].Text, got[1].DedupKey)
	}
}

func TestCoalescer_TrailingTwinSkippedAfterUrgentDelivery(t *testing.T) {
	// The reverse ordering of the twin split: the app_mention copy
	// delivered first (urgent), so the trailing message copy must not
	// buffer — a later multi-message batch would redeliver the id under
	// a batch key gc cannot dedup against "slack-<ts>".
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	aliasReg := newTestHandleAliasRegistry(t)
	rawMsg, _ := json.Marshal(slackMessageEvent{
		Type: "app_mention", Channel: "C1", User: "U_ALICE", TS: "100.000001", Text: "urgent ask",
	})
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		slackEventEnvelope{Type: "event_callback", EventID: "Ev1", Event: rawMsg}, func() {})
	if got := stub.snapshotInbounds(); len(got) != 1 {
		t.Fatalf("urgent copy must deliver immediately, captured %d", len(got))
	}

	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		humanEnvelope(t, "Ev2", "C1", "100.000001", "urgent ask"), func() {})
	if cfg.coalescer.pendingContains("C1", "100.000001") {
		t.Fatal("trailing twin must not enter the buffer")
	}
	if got := stub.snapshotInbounds(); len(got) != 1 {
		t.Fatalf("trailing twin must not deliver again, captured %d", len(got))
	}
}

func TestCoalescer_BatchDropsAlreadyDeliveredTS(t *testing.T) {
	// Delivery-time twin filter: a twin that slipped into the buffer
	// while the urgent copy was mid-flight (the enqueue-time seen check
	// races the urgent POST) is dropped when its batch delivers, because
	// by then the urgent success is recorded. An unseen neighbor still
	// delivers.
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.coalescer.enqueue("C1", pendingChannelInbound{inbound: externalInboundMessage{
		ProviderMessageID: "100.000001",
		Conversation:      conversationRef{ConversationID: "C1", Provider: "slack", Kind: "room"},
		Actor:             externalActor{ID: "U1", DisplayName: "Afik"},
		Text:              "raced twin",
		DedupKey:          "slack-100.000001",
	}})
	cfg.coalescer.enqueue("C1", pendingChannelInbound{inbound: externalInboundMessage{
		ProviderMessageID: "100.000002",
		Conversation:      conversationRef{ConversationID: "C1", Provider: "slack", Kind: "room"},
		Actor:             externalActor{ID: "U1", DisplayName: "Afik"},
		Text:              "fresh chatter",
		DedupKey:          "slack-100.000002",
	}})
	cfg.deliveredIDs.record("", "C1", "100.000001")
	cfg.coalescer.flushAheadOf("C1", "")
	got := stub.snapshotInbounds()
	if len(got) != 1 {
		t.Fatalf("captured %d inbounds, want 1", len(got))
	}
	if strings.Contains(got[0].Text, "raced twin") || !strings.Contains(got[0].Text, "fresh chatter") {
		t.Fatalf("batch must drop the already-delivered ts and keep the fresh one:\n%s", got[0].Text)
	}

	// A batch reduced to nothing delivers nothing (and reports success —
	// there is no work left to retry).
	cfg.coalescer.enqueue("C1", pendingChannelInbound{inbound: externalInboundMessage{
		ProviderMessageID: "100.000001",
		Conversation:      conversationRef{ConversationID: "C1", Provider: "slack", Kind: "room"},
		Actor:             externalActor{ID: "U1", DisplayName: "Afik"},
		Text:              "raced twin again",
		DedupKey:          "slack-100.000001",
	}})
	cfg.coalescer.flushAheadOf("C1", "")
	if got := stub.snapshotInbounds(); len(got) != 1 {
		t.Fatalf("fully-filtered batch must not deliver, captured %d", len(got))
	}
}

func TestAliasDispatch_SuppressedWhenSessionChannelBound(t *testing.T) {
	stub := &gcRouterStub{bindingsPayload: `{"items": [
		{"Status": "active", "Conversation": {"Provider": "slack", "ConversationID": "C1"}}
	]}`}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, 0) // coalescing off; targeted anyway
	aliasReg := newTestHandleAliasRegistry(t)
	if err := aliasReg.Set("mayor", "sess-mayor"); err != nil {
		t.Fatalf("alias set: %v", err)
	}
	env := humanEnvelope(t, "Ev1", "C1", "100.000001", "@mayor: ship it")
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})

	inbounds := stub.snapshotInbounds()
	if len(inbounds) != 1 {
		t.Fatalf("captured %d channel inbounds, want 1", len(inbounds))
	}
	// The address marker is restored so addressed-ness survives the
	// suppressed direct copy.
	if !strings.HasPrefix(inbounds[0].Text, "@mayor: ship it") {
		t.Fatalf("suppressed dispatch must restore the address marker, got %q", inbounds[0].Text)
	}
	if inbounds[0].ExplicitTarget != "mayor" {
		t.Fatalf("ExplicitTarget = %q, want mayor", inbounds[0].ExplicitTarget)
	}
	// No direct session-message copy: one delivery per turn.
	dispatchInflightWG.Wait()
	if msgs := stub.snapshotSessionMessages(); len(msgs) != 0 {
		t.Fatalf("suppressed dispatch still POSTed %d session messages", len(msgs))
	}
}

func TestAliasDispatch_StillFiresWhenSessionNotBound(t *testing.T) {
	stub := &gcRouterStub{} // empty bindings → not bound
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, 0)
	aliasReg := newTestHandleAliasRegistry(t)
	if err := aliasReg.Set("mayor", "sess-mayor"); err != nil {
		t.Fatalf("alias set: %v", err)
	}
	env := humanEnvelope(t, "Ev1", "C1", "100.000001", "@mayor: ship it")
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})

	dispatchInflightWG.Wait()
	if msgs := stub.snapshotSessionMessages(); len(msgs) != 1 {
		t.Fatalf("unbound alias target must still get the direct copy, got %d", len(msgs))
	}
	inbounds := stub.snapshotInbounds()
	if len(inbounds) != 1 {
		t.Fatalf("captured %d channel inbounds, want 1", len(inbounds))
	}
	if strings.HasPrefix(inbounds[0].Text, "@mayor:") {
		t.Fatalf("non-suppressed path must keep the stripped text, got %q", inbounds[0].Text)
	}
	// The alias audience's delivered id is recorded for preamble dedup.
	waitFor(t, "alias delivered-id record", func() bool {
		return cfg.deliveredIDs.seen("mayor", "C1", "100.000001")
	})
}

func TestRegisterAdapterSendsReplyInstructionsTemplate(t *testing.T) {
	bodyCh := make(chan adapterRegisterRequest, 1)
	gcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req adapterRegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		bodyCh <- req
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(gcSrv.Close)

	cfg := config{gcAPIBase: gcSrv.URL, cityName: "test-city", provider: "slack", accountID: "T1"}
	if err := registerAdapter(cfg); err != nil {
		t.Fatalf("registerAdapter: %v", err)
	}
	got := <-bodyCh
	if got.ReplyInstructions != slackReplyInstructionsTemplate {
		t.Fatalf("reply_instructions = %q, want %q", got.ReplyInstructions, slackReplyInstructionsTemplate)
	}
	if !strings.Contains(got.ReplyInstructions, "{conversation_id}") {
		t.Fatal("template must carry the {conversation_id} placeholder")
	}
}
