package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Tests for human-reaction visibility (gp-by3): reaction_added /
// reaction_removed events forward to the conversation-bound session as
// tagged, non-anchoring notifications, while the fleet's own mechanical
// reaction traffic (busy hourglass, peer-bot acks, our own bot) drops.

const (
	reactionTestOwnBotUserID = "U_SELF_BOT"
	reactionTestReactor      = "U_ALICE"
	reactionTestItemAuthor   = "U_MAYOR_BOT"
)

// reactionEnvelope builds a reaction event_callback envelope carrying
// the adapter's own bot user id in authorizations (self-guard input).
func reactionEnvelope(t *testing.T, evType, eventID, channel, itemTS, reactor, emoji, itemUser string) slackEventEnvelope {
	t.Helper()
	ev := slackReactionEvent{
		Type:     evType,
		User:     reactor,
		Reaction: emoji,
		ItemUser: itemUser,
		EventTS:  itemTS + "99",
	}
	ev.Item.Type = "message"
	ev.Item.Channel = channel
	ev.Item.TS = itemTS
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal reaction event: %v", err)
	}
	return slackEventEnvelope{
		Type:           "event_callback",
		EventID:        eventID,
		Event:          raw,
		Authorizations: []slackEventAuthorization{{UserID: reactionTestOwnBotUserID, IsBot: true}},
	}
}

// reactionTestConfig is the minimal cfg for reaction-path tests: gc
// stub capture only — no bot token, so the target-snippet lookup is
// skipped and the test stays network-inert.
func reactionTestConfig(gcURL string) config {
	return config{
		gcAPIBase:   gcURL,
		cityName:    "test-city",
		provider:    "slack",
		accountID:   "T1",
		dispatchSem: defaultTestDispatchSem,
	}
}

func runReactionEvent(t *testing.T, cfg config, env slackEventEnvelope) {
	t.Helper()
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
}

func TestReaction_ForwardsTaggedNotification(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	env := reactionEnvelope(t, "reaction_added", "Ev1", "C1", "100.1", reactionTestReactor, "+1", reactionTestItemAuthor)
	runReactionEvent(t, cfg, env)

	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("captured %d inbounds, want 1", len(msgs))
	}
	got := msgs[0]
	if !got.Actor.IsBot {
		t.Error("Actor.IsBot must be true (anchoring exclusion for the intake helpers)")
	}
	if got.Actor.DisplayName != "reaction: "+reactionTestReactor {
		t.Errorf("Actor.DisplayName = %q, want %q", got.Actor.DisplayName, "reaction: "+reactionTestReactor)
	}
	if got.Actor.ID != reactionTestReactor {
		t.Errorf("Actor.ID = %q, want %q", got.Actor.ID, reactionTestReactor)
	}
	if got.ExplicitTarget != "" {
		t.Errorf("ExplicitTarget = %q, want empty (a reaction is never an ask)", got.ExplicitTarget)
	}
	wantLine := "reacted :+1: to " + reactionTestItemAuthor + "'s message at ts 100.1"
	if !strings.Contains(got.Text, wantLine) || !strings.Contains(got.Text, "no reply expected") {
		t.Errorf("Text missing notification line / no-reply frame:\n%s", got.Text)
	}
	if got.DedupKey != "slack-reaction-Ev1" {
		t.Errorf("DedupKey = %q, want %q", got.DedupKey, "slack-reaction-Ev1")
	}
	if got.ProviderMessageID != "100.199" {
		t.Errorf("ProviderMessageID = %q, want event_ts %q", got.ProviderMessageID, "100.199")
	}
	if got.Conversation.Kind != "room" || got.Conversation.ConversationID != "C1" {
		t.Errorf("Conversation = %+v, want room C1", got.Conversation)
	}
	if got.ReplyToMessageID != "" {
		t.Errorf("ReplyToMessageID = %q, want empty without a target lookup", got.ReplyToMessageID)
	}
}

func TestReaction_RemovedRendersWithdrawal(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	env := reactionEnvelope(t, "reaction_removed", "Ev1", "C1", "100.1", reactionTestReactor, "+1", "")
	runReactionEvent(t, cfg, env)

	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("captured %d inbounds, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "removed their :+1: reaction from a message at ts 100.1") ||
		!strings.Contains(msgs[0].Text, "withdrawn") {
		t.Errorf("removal text wrong:\n%s", msgs[0].Text)
	}
}

func TestReaction_OwnBotReactorDropped(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	env := reactionEnvelope(t, "reaction_added", "Ev1", "C1", "100.1", reactionTestOwnBotUserID, "eyes", "")
	runReactionEvent(t, cfg, env)

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("own-bot reaction forwarded %d inbounds, want 0", len(msgs))
	}
}

func TestReaction_CompanySelfBotReactorDropped(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	cfg.companySelfBotUserID = "U_COMPANY_SELF"
	env := reactionEnvelope(t, "reaction_added", "Ev1", "C1", "100.1", "U_COMPANY_SELF", "eyes", "")
	runReactionEvent(t, cfg, env)

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("company-self reaction forwarded %d inbounds, want 0", len(msgs))
	}
}

func TestReaction_BusyEmojiDroppedRegardlessOfReactor(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	cfg.busyReaction = "hourglass"
	env := reactionEnvelope(t, "reaction_added", "Ev1", "C1", "100.1", reactionTestReactor, "hourglass", "")
	runReactionEvent(t, cfg, env)
	// The remove side of a fleet busy cycle is equally mechanical.
	env2 := reactionEnvelope(t, "reaction_removed", "Ev2", "C1", "100.1", reactionTestReactor, "hourglass", "")
	runReactionEvent(t, cfg, env2)

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("busy-emoji reactions forwarded %d inbounds, want 0", len(msgs))
	}
}

func TestReaction_PeerBotReactorDropped(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	cfg.peerBots = newTestPeerBotsRegistry(t, `{
  "peers": [
    {"label": "citadel", "bot_user_id": "U_CITADEL_BOT"}
  ]
}`)
	env := reactionEnvelope(t, "reaction_added", "Ev1", "C1", "100.1", "U_CITADEL_BOT", "white_check_mark", "")
	runReactionEvent(t, cfg, env)

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("peer-bot reaction forwarded %d inbounds, want 0", len(msgs))
	}
}

func TestReaction_MalformedShapesDropped(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)
	cfg := reactionTestConfig(gcStub.URL)

	// File reactions (legacy shape) carry no conversation to route.
	fileEnv := reactionEnvelope(t, "reaction_added", "Ev1", "C1", "100.1", reactionTestReactor, "+1", "")
	var raw map[string]any
	_ = json.Unmarshal(fileEnv.Event, &raw)
	raw["item"] = map[string]any{"type": "file", "file": "F123"}
	fileEnv.Event, _ = json.Marshal(raw)
	runReactionEvent(t, cfg, fileEnv)

	for _, e := range []slackEventEnvelope{
		reactionEnvelope(t, "reaction_added", "Ev2", "", "100.1", reactionTestReactor, "+1", ""), // no channel
		reactionEnvelope(t, "reaction_added", "Ev3", "C1", "", reactionTestReactor, "+1", ""),    // no ts
		reactionEnvelope(t, "reaction_added", "Ev4", "C1", "100.1", "", "+1", ""),                // no reactor
		reactionEnvelope(t, "reaction_added", "Ev5", "C1", "100.1", reactionTestReactor, "", ""), // no emoji
	} {
		runReactionEvent(t, cfg, e)
	}

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("malformed reactions forwarded %d inbounds, want 0", len(msgs))
	}
}

func TestReaction_SnippetAndThreadRootFromTargetLookup(t *testing.T) {
	slackStub, calls := fakeSlackRepliesServer(t, []slackThreadMessage{
		{User: reactionTestItemAuthor, Text: "should I proceed with the outreach list?", TS: "100.1", ThreadTS: "99.5"},
	})
	withSlackAPIStub(t, slackStub)

	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	cfg.slackBotToken = "xoxb-fake"
	env := reactionEnvelope(t, "reaction_added", "Ev1", "C1", "100.1", reactionTestReactor, "+1", reactionTestItemAuthor)
	runReactionEvent(t, cfg, env)

	if got := *calls; got != 1 {
		t.Fatalf("conversations.replies calls = %d, want 1", got)
	}
	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("captured %d inbounds, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, `("should I proceed with the outreach list?")`) {
		t.Errorf("Text missing target snippet:\n%s", msgs[0].Text)
	}
	if msgs[0].ReplyToMessageID != "99.5" {
		t.Errorf("ReplyToMessageID = %q, want thread root %q", msgs[0].ReplyToMessageID, "99.5")
	}
}

func TestReaction_TargetLookupFailureDegradesToNoSnippet(t *testing.T) {
	slackStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(slackStub.Close)
	withSlackAPIStub(t, slackStub)

	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	cfg.slackBotToken = "xoxb-fake"
	env := reactionEnvelope(t, "reaction_added", "Ev1", "C1", "100.1", reactionTestReactor, "+1", "")
	runReactionEvent(t, cfg, env)

	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("captured %d inbounds, want 1 (lookup failure must not drop the notification)", len(msgs))
	}
	if strings.Contains(msgs[0].Text, `("`) {
		t.Errorf("Text carries a snippet despite lookup failure:\n%s", msgs[0].Text)
	}
	if msgs[0].ReplyToMessageID != "" {
		t.Errorf("ReplyToMessageID = %q, want empty", msgs[0].ReplyToMessageID)
	}
}

// gp-9e7 item 1: a reaction NEVER wakes a session solo. It buffers in
// the coalescer's no-wake side lane — arming no timer — and delivers
// only by riding the channel's next real delivery.
func TestReaction_BuffersNoWakeInRooms(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	cfg.coalescer = newInboundCoalescer(defaultCoalesceWindow, nil)
	env := reactionEnvelope(t, "reaction_added", "Ev1", "C1", "100.1", reactionTestReactor, "+1", "")
	runReactionEvent(t, cfg, env)

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Fatalf("room reaction posted immediately: %d inbounds", len(msgs))
	}
	cfg.coalescer.mu.Lock()
	buffered := len(cfg.coalescer.reactions["C1"])
	_, timerArmed := cfg.coalescer.timers["C1"]
	cfg.coalescer.mu.Unlock()
	if buffered != 1 {
		t.Errorf("reaction side-buffer holds %d entries, want 1", buffered)
	}
	if timerArmed {
		t.Error("a reaction must never arm a flush timer (solo wake)")
	}
}

// DMs buffer too now — the reaction piggybacks on the next DM inbound's
// flush-ahead instead of waking the session by itself.
func TestReaction_DMBuffersNoWake(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	cfg.coalescer = newInboundCoalescer(defaultCoalesceWindow, nil)
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) bool {
		return deliverCoalescedBatch(cfg, channel, batch)
	}
	env := reactionEnvelope(t, "reaction_added", "Ev1", "D1", "100.1", reactionTestReactor, "+1", "")
	runReactionEvent(t, cfg, env)

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Fatalf("DM reaction posted immediately: %d inbounds, want 0 (no-wake buffer)", len(msgs))
	}
	got := cfg.coalescer.flushAheadOf("D1", "")
	if len(got) != 0 {
		t.Fatalf("withheld = %+v, want none", got)
	}
	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("flush-ahead delivered %d inbounds, want the buffered reaction", len(msgs))
	}
	if msgs[0].Conversation.Kind != "dm" || msgs[0].DedupKey != "slack-reaction-Ev1" {
		t.Errorf("delivered = kind %q dedup %q", msgs[0].Conversation.Kind, msgs[0].DedupKey)
	}
}

// Zero-window (coalescing disabled) deployments must still buffer:
// "never wake solo" is not conditional on the burst coalescer being on.
func TestReaction_BuffersEvenWithCoalescingDisabled(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	cfg.coalescer = newInboundCoalescer(0, nil)
	env := reactionEnvelope(t, "reaction_added", "Ev1", "C1", "100.1", reactionTestReactor, "+1", "")
	runReactionEvent(t, cfg, env)

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Fatalf("disabled-coalescer reaction posted immediately: %d inbounds", len(msgs))
	}
	cfg.coalescer.mu.Lock()
	buffered := len(cfg.coalescer.reactions["C1"])
	cfg.coalescer.mu.Unlock()
	if buffered != 1 {
		t.Errorf("reaction side-buffer holds %d entries, want 1", buffered)
	}
}

// Founder-ack exception: a reaction ON one of the adapter's own
// outbound messages may ride an ALREADY-armed coalesce window — and
// only an already-armed one; without a window it buffers no-wake.
func TestReaction_OwnOutboundTargetRidesArmedWindow(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := reactionTestConfig(gcStub.URL)
	cfg.coalescer = newInboundCoalescer(time.Hour, nil)

	// No armed window: even an own-target reaction buffers no-wake.
	envCold := reactionEnvelope(t, "reaction_added", "Ev1", "C1", "50.1", reactionTestReactor, "+1", reactionTestOwnBotUserID)
	runReactionEvent(t, cfg, envCold)
	cfg.coalescer.mu.Lock()
	if _, armed := cfg.coalescer.timers["C1"]; armed {
		cfg.coalescer.mu.Unlock()
		t.Fatal("own-target reaction armed a window of its own")
	}
	if n := len(cfg.coalescer.reactions["C1"]); n != 1 {
		cfg.coalescer.mu.Unlock()
		t.Fatalf("cold own-target reaction: side-buffer holds %d, want 1", n)
	}
	cfg.coalescer.mu.Unlock()

	// Armed window (a real message is buffering): the own-target
	// reaction joins the pending batch and delivers with it.
	cfg.coalescer.enqueue("C1", testPending("C1", "100.0", "chatter"))
	envWarm := reactionEnvelope(t, "reaction_added", "Ev2", "C1", "100.0", reactionTestReactor, "tada", reactionTestOwnBotUserID)
	runReactionEvent(t, cfg, envWarm)
	cfg.coalescer.mu.Lock()
	pendingN := len(cfg.coalescer.pending["C1"])
	cfg.coalescer.mu.Unlock()
	if pendingN != 2 {
		t.Fatalf("pending = %d entries, want message + riding reaction", pendingN)
	}

	// A reaction on someone ELSE's message never rides, armed or not.
	envOther := reactionEnvelope(t, "reaction_added", "Ev3", "C1", "100.0", reactionTestReactor, "+1", reactionTestItemAuthor)
	runReactionEvent(t, cfg, envOther)
	cfg.coalescer.mu.Lock()
	pendingN = len(cfg.coalescer.pending["C1"])
	reactionsN := len(cfg.coalescer.reactions["C1"])
	cfg.coalescer.mu.Unlock()
	if pendingN != 2 || reactionsN != 2 {
		t.Fatalf("pending=%d reactions=%d, want 2/2 (other-target reaction stays in the side lane)", pendingN, reactionsN)
	}
}

// The coalesced-block header claims a reply anchor; bot-tagged items
// (reaction notifications) are excluded from anchoring by the intake
// helpers, so the header must name the newest HUMAN message even when
// a reaction is the newest item in the batch.
func TestCoalescedBlockAnchorSkipsBotItems(t *testing.T) {
	human := externalInboundMessage{
		ProviderMessageID: "100.1",
		Actor:             externalActor{ID: "U_ALICE", DisplayName: "Alice"},
		Text:              "hello",
	}
	reaction := externalInboundMessage{
		ProviderMessageID: "100.299",
		Actor:             externalActor{ID: "U_BOB", DisplayName: "reaction: Bob", IsBot: true},
		Text:              "reacted :+1: to Alice's message at ts 100.1 — ack/approval/emphasis signal; no reply expected",
	}
	block := formatCoalescedBlock(config{}, "C1", []pendingChannelInbound{
		{inbound: human}, {inbound: reaction},
	})
	if !strings.Contains(block, "--turn-ts 100.1 ") {
		t.Errorf("header does not anchor at the newest human message:\n%s", block)
	}
	if strings.Contains(block, "--turn-ts 100.299") {
		t.Errorf("header anchored on the bot reaction item:\n%s", block)
	}
}

func TestTruncateRunesClean(t *testing.T) {
	if got := truncateRunesClean("short", 10); got != "short" {
		t.Errorf("no-op truncation changed the string: %q", got)
	}
	if got := truncateRunesClean("héllo world", 5); got != "héllo…" {
		t.Errorf("rune truncation = %q, want héllo…", got)
	}
}
