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
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
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
	if err := deliverCoalescedBatch(cfg, "C1", batch); err != nil {
		t.Fatalf("batch delivery reported failure: %v", err)
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
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
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

	// Trigger one delivery attempt that restores on failure. (flushAll
	// is no longer a restore trigger: since gp-9e7 fix round 2a' it
	// drains to its retry bound and spools the residue instead of
	// leaving it in the maps.)
	cfg.coalescer.flushAheadOf("C1", "") // deliver fails; batch restored

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

// A copy the rejection ladder OWNS is not dropped on a same-ts claim
// that was concluded without a confirmed delivery (gp-sgu7, codex r26
// finding 4): the drain spool commits a failed urgent twin's claim so no
// takeover re-posts a copy the next startup replays — but a batch probe
// that dropped its owned member on that claim returned nil, the ladder
// recorded "delivered", and the replay discarded both recoverable
// copies. Only deliveredIDs — recorded after gc vouched — drops an owned
// copy; on a bare claim the ladder's copy posts and gc's dedup key
// bounds the duplicate. An unowned member keeps the gp-ios contract
// above (TestCoalescer_BatchSkipsClaimCommittedMember).
func TestCoalescer_OwnedMemberPostsOnAnUnconfirmedClaim(t *testing.T) {
	stub := &flakyInboundStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)

	keyY := channelDeliveryClaimKey("C1", "100.000020")
	if proceed, _ := cfg.channelClaims.begin(keyY); !proceed {
		t.Fatal("setup: could not claim Y")
	}
	cfg.channelClaims.commit(keyY) // concluded — by the drain spool, not by a delivery: no deliveredIDs record

	owned := pendingChannelInbound{inbound: externalInboundMessage{ProviderMessageID: "100.000020", Text: "the ladder's owned copy",
		Conversation: conversationRef{ConversationID: "C1", Kind: "room"}}, isolate: true, attempts: 1}
	if err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{owned}); err != nil {
		t.Fatalf("batch delivery reported failure: %v", err)
	}
	got := stub.snapshot()
	if len(got) != 1 || !strings.Contains(got[0].Text, "the ladder's owned copy") {
		t.Fatalf("the owned copy must post on an unconfirmed claim (a false 'delivered' verdict would discard it): got %d inbound(s)", len(got))
	}
}

// --- codex r32 finding 1, r33 finding 1 -------------------------------------

// Every acknowledged event is inside the coalescer's shutdown barrier
// from the handler's hand-off to its goroutine's return: the owner inside
// its POST, the same-ts twin parked at the claim wait (r32 finding 1) and
// the same-event-id redelivery parked at the event-id wait (r33 finding
// 1) are all registered, so the drain cannot conclude — and main cannot
// seal the spool — while the owner's failure hands the message to one of
// them for its last recovery POST.
func TestAcknowledgedEventsAreInsideTheShutdownBarrier(t *testing.T) {
	arrived := []chan struct{}{make(chan struct{}), make(chan struct{})}
	answer := []chan int{make(chan int), make(chan int)}
	stop := make(chan struct{}) // a failed assertion must not leave a handler parked forever (the server's Close would wait on it)
	var mu sync.Mutex
	posts := 0
	gcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := posts
		posts++
		mu.Unlock()
		if i > 1 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		close(arrived[i])
		select {
		case code := <-answer[i]:
			w.WriteHeader(code)
		case <-stop:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(gcSrv.Close)
	t.Cleanup(func() { close(stop) }) // runs before Close (LIFO)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	cfg.eventDedup = newEventDedupCache(eventDedupTTL)
	cfg.eventWG = &sync.WaitGroup{}
	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> takeover during the drain"
	inflight := func() int {
		cfg.coalescer.mu.Lock()
		defer cfg.coalescer.mu.Unlock()
		return cfg.coalescer.inflight
	}
	// The handler's hand-off, as handleSlackEvents performs it: the
	// event-id claim taken synchronously, then dispatchAcknowledgedEvent.
	dispatch := func(eventType, eventID string) {
		env := botMentionEnvelope(t, eventType, eventID, "C1", "100.000030", "", text, true)
		proceed, wait := cfg.eventDedup.begin(eventID)
		var release func()
		if proceed {
			release = func() {}
		}
		dispatchAcknowledgedEvent(cfg, aliasReg, nil, nil, nil, nil, env, "", proceed, wait, release)
	}
	dispatch("message", "Ev1")
	<-arrived[0]                   // the owner holds the claim inside its POST
	dispatch("app_mention", "Ev2") // the same-ts twin: parks at the claim wait
	dispatch("message", "Ev1")     // the same-event-id redelivery: parks at the event-id wait
	waitFor(t, "every acknowledged event registered with the shutdown barrier (inflight 3: the owner in its POST, the twin at the claim wait, the redelivery at the event-id wait)", func() bool { return inflight() == 3 })

	done := make(chan struct{})
	go func() { cfg.coalescer.flushAll(); close(done) }()
	answer[0] <- http.StatusInternalServerError // the owner fails and releases: a parked copy takes over
	<-arrived[1]                                // the takeover POST is under way
	select {
	case <-done:
		t.Fatal("flushAll returned while a parked copy's takeover POST was under way — the drain concluded, and main would have sealed the spool under the message's last recovery path")
	case <-time.After(100 * time.Millisecond):
	}
	answer[1] <- http.StatusAccepted
	cfg.eventWG.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flushAll did not return once every event had finished")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 2 {
		t.Fatalf("POST attempts = %d, want 2 (one failure + one takeover success; the third copy skips the committed claim)", posts)
	}
}

// --- codex r34 finding 1 ------------------------------------------------------

// The alias-dispatch leg — the goroutine processSlackEvent hands its
// slot to for a targeted inbound — is inside the coalescer's shutdown
// barrier from before the hand-off to its return (codex r34 finding 1):
// it can spool its failure during the drain, and with the parent's
// registration ending at the parent's return, flushAll could observe
// zero in-flight work and main seal the spool under its POST.
func TestAliasDispatchIsInsideTheShutdownBarrier(t *testing.T) {
	arrived := make(chan struct{})
	answer := make(chan int)
	stop := make(chan struct{})
	var once sync.Once
	gcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/extmsg/inbound"):
			w.WriteHeader(http.StatusAccepted)
		case strings.Contains(r.URL.Path, "/extmsg/bindings"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items": []}`))
		case strings.Contains(r.URL.Path, "/messages"):
			once.Do(func() { close(arrived) })
			select {
			case code := <-answer:
				w.WriteHeader(code)
			case <-stop:
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gcSrv.Close)
	t.Cleanup(func() { close(stop) }) // runs before Close (LIFO)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	aliasReg := newTestHandleAliasRegistry(t)
	if err := aliasReg.Set("mayor", "sess-mayor-1"); err != nil {
		t.Fatalf("alias set: %v", err)
	}
	inflight := func() int {
		cfg.coalescer.mu.Lock()
		defer cfg.coalescer.mu.Unlock()
		return cfg.coalescer.inflight
	}
	env := botMentionEnvelope(t, "message", "Ev1", "C1", "100.000050", "", "@mayor: please handle this", true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {}) // returns once the slot is handed to the alias leg
	<-arrived                                                            // the alias POST is under way
	if got := inflight(); got != 1 {
		t.Fatalf("the alias leg must be registered with the shutdown barrier before the hand-off: inflight %d, want 1", got)
	}
	done := make(chan struct{})
	go func() { cfg.coalescer.flushAll(); close(done) }()
	select {
	case <-done:
		t.Fatal("flushAll returned while the alias POST was under way — main would have sealed the spool under a delivery that can still fail and need it")
	case <-time.After(100 * time.Millisecond):
	}
	answer <- http.StatusAccepted
	dispatchInflightWG.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flushAll did not return once the alias leg finished")
	}
}
