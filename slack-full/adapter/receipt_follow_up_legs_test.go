package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- the three claim-holding legs arm the follow-up on HELD (gp-3yg) ---------

// heldThenLostGC is a gc whose FIRST inbound POST answers pending (a busy
// session), whose receipt endpoint then reports that fan-out concluded
// FAILED, and whose later POSTs (the recovery copy) are vouched for. It
// is the pending → lost shape end to end, at the wire.
type heldThenLostGC struct {
	mu        sync.Mutex
	posts     []externalInboundMessage
	polls     []string
	pollState string // "concluded-failed" (default) | "concluded-delivered" | "pending"
}

func (g *heldThenLostGC) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/extmsg/inbound/receipts/") {
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			g.mu.Lock()
			g.polls = append(g.polls, id)
			state := g.pollState
			g.mu.Unlock()
			switch state {
			case "pending":
				_, _ = w.Write([]byte(`{"receipt_id":"` + id + `","state":"pending"}`))
			case "concluded-delivered":
				_, _ = w.Write([]byte(`{"receipt_id":"` + id + `","state":"concluded","delivery":{"receipt_id":"` + id +
					`","status":"delivered","delivered_bytes":790,"expected_bytes":790}}`))
			default:
				_, _ = w.Write([]byte(`{"receipt_id":"` + id + `","state":"concluded","delivery":{"receipt_id":"` + id +
					`","status":"failed","delivered_bytes":0,"expected_bytes":790,"members":[{"session_id":"gc__mayor","status":"failed","error":"nudge lock timeout"}]}}`))
			}
			return
		}
		var env struct {
			Message externalInboundMessage `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		g.mu.Lock()
		g.posts = append(g.posts, env.Message)
		n := len(g.posts)
		g.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			_, _ = w.Write([]byte(`{"TargetSessionID":"sess-1","delivery":{"receipt_id":"ir-1-` + strconv.Itoa(n) + `","status":"pending"}}`))
			return
		}
		expected := len(env.Message.Text)
		_, _ = w.Write([]byte(`{"TargetSessionID":"sess-1","delivery":{"receipt_id":"ir-1-` + strconv.Itoa(n) +
			`","status":"delivered","delivered_bytes":` + strconv.Itoa(expected) + `,"expected_bytes":` + strconv.Itoa(expected) + `}}`))
	}
}

func (g *heldThenLostGC) snapshot() ([]externalInboundMessage, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]externalInboundMessage(nil), g.posts...), append([]string(nil), g.polls...)
}

func fastFollowUps(cfg config) *receiptFollowUps {
	f := receiptFollowUpsFor(cfg)
	f.interval = 5 * time.Millisecond
	f.deadline = 2 * time.Second
	return f
}

// Urgent leg (bot mention / DM): the hold concludes the claim exactly as
// before, and the follow-up then learns the send was lost and re-posts
// the identical channel envelope. Two POSTs reach gc; the claim stays
// concluded (no twin re-post) and nothing is dead-lettered.
func TestReceiptFollowUp_UrgentLegRecoversLostHold(t *testing.T) {
	gc := &heldThenLostGC{}
	gcSrv := httptest.NewServer(gc.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	cfg.inboundDeadLetterDir = t.TempDir()
	cfg.receiptFollowUps = fastFollowUps(cfg)
	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> the LightIC / NDAA ask"
	ts := "1787901226.718729"

	env := botMentionEnvelope(t, "message", "Ev1", "C0B0Y964Q1Z", ts, "", text, true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
	if !cfg.receiptFollowUps.wait(3 * time.Second) {
		t.Fatal("follow-up did not finish")
	}

	posts, polls := gc.snapshot()
	if len(posts) != 2 {
		t.Fatalf("gc saw %d POSTs, want 2 (the held original + one recovery copy)", len(posts))
	}
	if posts[0].ProviderMessageID != ts || !reflect.DeepEqual(posts[0], posts[1]) {
		t.Fatalf("recovery copy is not the identical envelope:\n%+v\n---\n%+v", posts[0], posts[1])
	}
	if len(polls) == 0 || polls[0] != "ir-1-1" {
		t.Fatalf("follow-up polled %v, want the held receipt ir-1-1", polls)
	}
	if !cfg.deliveredIDs.seen("", "C0B0Y964Q1Z", ts) {
		t.Error("the hold no longer concludes the claim — a same-ts twin would now re-post")
	}
	if entries, _ := readDeadLetterDir(t, cfg.inboundDeadLetterDir); len(entries) != 0 {
		t.Fatalf("a recovered message was dead-lettered: %v", entries)
	}
}

// Same leg, pending → landed late: exactly one POST, nothing else.
func TestReceiptFollowUp_UrgentLegLateLandingStaysQuiet(t *testing.T) {
	gc := &heldThenLostGC{pollState: "concluded-delivered"}
	gcSrv := httptest.NewServer(gc.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	cfg.inboundDeadLetterDir = t.TempDir()
	cfg.receiptFollowUps = fastFollowUps(cfg)
	aliasReg := newTestHandleAliasRegistry(t)
	env := botMentionEnvelope(t, "message", "Ev1", "C1", "100.000300", "", "<@"+testBotUserID+"> hi", true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
	if !cfg.receiptFollowUps.wait(3 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	posts, polls := gc.snapshot()
	if len(posts) != 1 || len(polls) == 0 {
		t.Fatalf("posts=%d polls=%d, want 1 post and at least one poll", len(posts), len(polls))
	}
}

// Coalesced leg: a held batch is committed and recorded as today; the
// follow-up recovers it with one re-post of the batch envelope.
func TestReceiptFollowUp_CoalescedLegRecoversLostHold(t *testing.T) {
	gc := &heldThenLostGC{}
	gcSrv := httptest.NewServer(gc.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	cfg.deliveryReceiptGate = true
	cfg.inboundDeadLetterDir = t.TempDir()
	cfg.receiptFollowUps = fastFollowUps(cfg)

	batch := []pendingChannelInbound{
		{inbound: externalInboundMessage{ProviderMessageID: "100.000400", Text: "first",
			Conversation: conversationRef{ConversationID: "C1", Kind: "room"}}},
		{inbound: externalInboundMessage{ProviderMessageID: "100.000401", Text: "second",
			Conversation: conversationRef{ConversationID: "C1", Kind: "room"}}},
	}
	if err := deliverCoalescedBatch(cfg, "C1", batch); err != nil {
		t.Fatalf("deliverCoalescedBatch: %v (a hold must still conclude the batch)", err)
	}
	if !cfg.receiptFollowUps.wait(3 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	posts, _ := gc.snapshot()
	if len(posts) != 2 {
		t.Fatalf("gc saw %d POSTs, want 2 (held batch + recovery copy)", len(posts))
	}
	if !reflect.DeepEqual(posts[0], posts[1]) || posts[1].DedupKey == "" {
		t.Fatalf("recovery copy differs from the held batch envelope:\n%+v\n---\n%+v", posts[0], posts[1])
	}
	if !cfg.deliveredIDs.seen("", "C1", "100.000401") {
		t.Error("held batch no longer records its ids as delivered")
	}
}

// The loss STANDS (the recovery copy is not vouched for either): the
// urgent leg's dead letter lands in the channel's file with the envelope
// exactly as POSTed, so an operator can re-post it by hand.
func TestReceiptFollowUp_UrgentLegStandingLossIsDeadLettered(t *testing.T) {
	var posts int
	var mu sync.Mutex
	gcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"receipt_id":"ir-1-1","state":"unknown"}`))
			return
		}
		mu.Lock()
		posts++
		mu.Unlock()
		// Every POST is held, so the recovery copy is followed once more
		// and its unknown receipt ends the ladder in the dead letter.
		_, _ = w.Write([]byte(`{"TargetSessionID":"sess-1","delivery":{"receipt_id":"ir-1-1","status":"pending"}}`))
	}))
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	cfg.inboundDeadLetterDir = t.TempDir()
	cfg.receiptFollowUps = fastFollowUps(cfg)
	aliasReg := newTestHandleAliasRegistry(t)
	ts := "100.000500"
	env := botMentionEnvelope(t, "message", "Ev1", "C1", ts, "", "<@"+testBotUserID+"> lost twice", true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
	if !cfg.receiptFollowUps.wait(3 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	mu.Lock()
	n := posts
	mu.Unlock()
	if n != 1+receiptFollowUpReposts {
		t.Fatalf("gc saw %d POSTs, want %d", n, 1+receiptFollowUpReposts)
	}
	entries, reasons := readDeadLetterDir(t, cfg.inboundDeadLetterDir)
	if len(entries) != 1 || entries[0] != ts {
		t.Fatalf("dead-lettered %v, want [%s]", entries, ts)
	}
	if !strings.Contains(reasons[0], "lost") {
		t.Fatalf("dead-letter reason = %q, want it to say the delivery was lost", reasons[0])
	}
}

// Alias-injection leg (@handle: prefix): a held injection is followed up
// the same way; the recovery copy is a second injection into the aliased
// session, and the alias claim stays concluded.
func TestReceiptFollowUp_AliasLegRecoversLostHold(t *testing.T) {
	gc := &heldRouterStub{}
	gcSrv := httptest.NewServer(gc.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	cfg.bindingCheck = newBindingCheckCache()
	cfg.inboundDeadLetterDir = t.TempDir()
	// The rendered reminder embeds the channel's display name. Seed a
	// negative cache entry (renders the bare id, no Slack fetch) and let
	// the stub learn a real name between the two injections: a recovery
	// that RE-RENDERED would now say "#general (C1)" and differ from the
	// held bytes (codex r1 P2 #4).
	cfg.slackBotToken = "xoxb-test"
	cfg.channelNames = newChannelNameCache()
	cfg.channelNames.put("C1", "", time.Now(), time.Hour)
	gc.onFirstInjection = func() { cfg.channelNames.put("C1", "general", time.Now(), time.Hour) }
	cfg.receiptFollowUps = fastFollowUps(cfg)
	aliasReg := newTestHandleAliasRegistry(t)
	if err := aliasReg.Set("mayor", "sess-mayor-1"); err != nil {
		t.Fatalf("alias set: %v", err)
	}
	env := botMentionEnvelope(t, "message", "Ev1", "C1", "100.000600", "", "@mayor: please handle this", true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
	// The alias leg dispatches on a detached goroutine after
	// processSlackEvent returns, so wait for the recovery injection
	// itself rather than for a follow-up that may not be noted yet.
	waitFor(t, "held injection recovered", func() bool {
		injected, _ := gc.snapshot()
		return len(injected) >= 2
	})
	if !cfg.receiptFollowUps.wait(3 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	injected, polls := gc.snapshot()
	if len(injected) != 2 {
		t.Fatalf("aliased session received %d injections, want 2 (held original + recovery copy)", len(injected))
	}
	if injected[0] != injected[1] {
		t.Fatalf("recovery injection differs from the held one:\n%s\n---\n%s", injected[0], injected[1])
	}
	if strings.Contains(injected[1], "#general") {
		t.Fatal("the recovery injection was re-rendered against the updated channel-name cache instead of re-sending the held bytes")
	}
	if len(polls) == 0 || polls[0] != "ir-9-1" {
		t.Fatalf("follow-up polled %v, want the held injection receipt ir-9-1", polls)
	}
	if entries, _ := readDeadLetterDir(t, cfg.inboundDeadLetterDir); len(entries) != 0 {
		t.Fatalf("a recovered injection was dead-lettered: %v", entries)
	}
}

// heldRouterStub routes like flakyRouterStub, but the FIRST session-message
// injection is answered with a pending receipt whose follow-up concludes
// failed; later injections are vouched for.
type heldRouterStub struct {
	mu       sync.Mutex
	injected []string
	polls    []string
	// onFirstInjection runs once, after the first injection is recorded.
	onFirstInjection func()
}

func (g *heldRouterStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/extmsg/inbound/receipts/"):
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			g.mu.Lock()
			g.polls = append(g.polls, id)
			g.mu.Unlock()
			_, _ = w.Write([]byte(`{"receipt_id":"` + id + `","state":"concluded","delivery":{"receipt_id":"` + id +
				`","status":"failed","delivered_bytes":0,"expected_bytes":500}}`))
		case strings.Contains(r.URL.Path, "/extmsg/inbound"):
			_, _ = w.Write([]byte(`{"TargetSessionID":"sess-1","delivery":{"receipt_id":"ir-9-0","status":"delivered","delivered_bytes":10,"expected_bytes":10}}`))
		case strings.Contains(r.URL.Path, "/extmsg/bindings"):
			_, _ = w.Write([]byte(`{"items": []}`))
		case strings.Contains(r.URL.Path, "/messages"):
			var req gcSessionMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			g.mu.Lock()
			g.injected = append(g.injected, req.Message)
			n := len(g.injected)
			hook := g.onFirstInjection
			g.mu.Unlock()
			if n == 1 {
				if hook != nil {
					hook()
				}
				_, _ = w.Write([]byte(`{"delivery":{"receipt_id":"ir-9-1","status":"pending"}}`))
				return
			}
			size := strconv.Itoa(len(req.Message))
			_, _ = w.Write([]byte(`{"delivery":{"receipt_id":"ir-9-` + strconv.Itoa(n) + `","status":"delivered","delivered_bytes":` + size + `,"expected_bytes":` + size + `}}`))
		default:
			http.NotFound(w, r)
		}
	}
}

func (g *heldRouterStub) snapshot() ([]string, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.injected...), append([]string(nil), g.polls...)
}

// readDeadLetterDir returns the provider message ids and reasons of every
// record under dir, in file order.
func readDeadLetterDir(t *testing.T, dir string) ([]string, []string) {
	t.Helper()
	var ids, reasons []string
	for _, path := range globDeadLetterFiles(t, dir) {
		for _, line := range strings.Split(strings.TrimSpace(readFileString(t, path)), "\n") {
			if line == "" {
				continue
			}
			var rec inboundDeadLetterRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("dead-letter line %q: %v", line, err)
			}
			ids = append(ids, rec.Inbound.ProviderMessageID)
			reasons = append(reasons, rec.Reason)
		}
	}
	return ids, reasons
}

func globDeadLetterFiles(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	return paths
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// P2 #4: an alias injection whose loss stands is recorded with what a
// hand recovery actually needs — the aliased session, the handle and the
// exact rendered reminder — not as a plain channel inbound that a replay
// would route to the channel-bound session.
func TestReceiptFollowUp_AliasStandingLossRecordsInjectionDetails(t *testing.T) {
	gcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/extmsg/inbound/receipts/"):
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_, _ = w.Write([]byte(`{"receipt_id":"` + id + `","state":"unknown"}`))
		case strings.Contains(r.URL.Path, "/extmsg/inbound"):
			_, _ = w.Write([]byte(`{"TargetSessionID":"sess-1","delivery":{"receipt_id":"ir-9-0","status":"delivered","delivered_bytes":10,"expected_bytes":10}}`))
		case strings.Contains(r.URL.Path, "/extmsg/bindings"):
			_, _ = w.Write([]byte(`{"items": []}`))
		case strings.Contains(r.URL.Path, "/messages"):
			// Every injection is held and every poll says unknown, so the
			// ladder ends in the dead letter.
			_, _ = w.Write([]byte(`{"delivery":{"receipt_id":"ir-9-1","status":"pending"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	cfg.bindingCheck = newBindingCheckCache()
	cfg.inboundDeadLetterDir = t.TempDir()
	cfg.receiptFollowUps = fastFollowUps(cfg)
	aliasReg := newTestHandleAliasRegistry(t)
	if err := aliasReg.Set("mayor", "sess-mayor-1"); err != nil {
		t.Fatalf("alias set: %v", err)
	}
	env := botMentionEnvelope(t, "message", "Ev1", "C1", "100.000700", "", "@mayor: this one is lost twice", true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
	waitFor(t, "alias standing loss dead-lettered", func() bool {
		ids, _ := readDeadLetterDir(t, cfg.inboundDeadLetterDir)
		return len(ids) == 1
	})
	if !cfg.receiptFollowUps.wait(3 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	recs := readDeadLetterRecords(t, cfg.inboundDeadLetterDir)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Inbound.ProviderMessageID != "100.000700" {
		t.Fatalf("record inbound ts = %q", rec.Inbound.ProviderMessageID)
	}
	if rec.AliasSessionID != "sess-mayor-1" || rec.AliasHandle != "mayor" {
		t.Fatalf("record alias fields = session %q handle %q, want the aliased session and handle", rec.AliasSessionID, rec.AliasHandle)
	}
	if !strings.Contains(rec.AliasBody, "Slack address-by-handle: @mayor") || !strings.Contains(rec.AliasBody, "this one is lost twice") {
		t.Fatalf("record alias body does not carry the rendered reminder: %q", rec.AliasBody)
	}
	if !strings.Contains(rec.Reason, "lost") {
		t.Fatalf("reason = %q, want it to say the delivery was lost", rec.Reason)
	}
}

// readDeadLetterRecords decodes every record under dir, in file order.
func readDeadLetterRecords(t *testing.T, dir string) []inboundDeadLetterRecord {
	t.Helper()
	var out []inboundDeadLetterRecord
	for _, path := range globDeadLetterFiles(t, dir) {
		for _, line := range strings.Split(strings.TrimSpace(readFileString(t, path)), "\n") {
			if line == "" {
				continue
			}
			var rec inboundDeadLetterRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("dead-letter line %q: %v", line, err)
			}
			out = append(out, rec)
		}
	}
	return out
}
