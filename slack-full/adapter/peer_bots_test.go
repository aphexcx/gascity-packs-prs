package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for legacy-path peer-bot visibility (gp-kop): allowlisted fleet-app
// posts deliver to the channel-bound session as tagged read-only context —
// buffered onto the next natural inbound by default, immediate for
// configured channels — while unknown bots and the adapter's own posts keep
// the historical drop behavior.

const (
	peerTestOwnBotUserID  = "U_SELF_BOT"
	peerTestPeerBotID     = "B0BGYLTM8NT"
	peerTestPeerAppID     = "A_CITADEL"
	peerTestPeerBotUserID = "U_CITADEL_BOT"
)

// stubAuthorResolver is a canned companyAuthorResolver for the peer path.
type stubAuthorResolver struct {
	info    companyBotInfo
	outcome botResolveOutcome
	calls   int
}

func (s *stubAuthorResolver) Resolve(botID string) (companyBotInfo, botResolveOutcome) {
	s.calls++
	return s.info, s.outcome
}

func writePeerBotsFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "peer_bots.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write peer bots seed: %v", err)
	}
	return path
}

func newTestPeerBotsRegistry(t *testing.T, content string) *peerBotsRegistry {
	t.Helper()
	path := writePeerBotsFile(t, t.TempDir(), content)
	reg, err := newPeerBotsRegistry(path)
	if err != nil {
		t.Fatalf("newPeerBotsRegistry: %v", err)
	}
	return reg
}

// peerTestConfig builds the minimal cfg for peer-path processSlackEvent
// tests: gc stub capture, a loaded allowlist, a live buffer, and a stub
// resolver that answers with the citadel peer's identity.
func peerTestConfig(gcURL string, reg *peerBotsRegistry, resolver companyAuthorResolver) config {
	return config{
		gcAPIBase:   gcURL,
		cityName:    "test-city",
		provider:    "slack",
		accountID:   "T1",
		dispatchSem: defaultTestDispatchSem,
		peerBots:    reg,
		peerContext: newPeerContextBuffer(),
		peerAuthors: resolver,
	}
}

// peerBotEnvelope builds an event_callback envelope for a bot-authored
// channel message, carrying the adapter's own bot user id in
// authorizations (so the self-guard has something to compare against).
func peerBotEnvelope(t *testing.T, eventType, eventID, channel, ts, threadTS, text, botID, appID string) slackEventEnvelope {
	t.Helper()
	rawMsg, err := json.Marshal(slackMessageEvent{
		Type:     eventType,
		Channel:  channel,
		BotID:    botID,
		AppID:    appID,
		TS:       ts,
		ThreadTS: threadTS,
		Text:     text,
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return slackEventEnvelope{
		Type:           "event_callback",
		EventID:        eventID,
		Event:          rawMsg,
		Authorizations: []slackEventAuthorization{{UserID: peerTestOwnBotUserID, IsBot: true}},
	}
}

func humanEnvelope(t *testing.T, eventID, channel, ts, text string) slackEventEnvelope {
	t.Helper()
	rawMsg, err := json.Marshal(slackMessageEvent{
		Type:    "message",
		Channel: channel,
		User:    "U_ALICE",
		TS:      ts,
		Text:    text,
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return slackEventEnvelope{Type: "event_callback", EventID: eventID, Event: rawMsg}
}

const peerTestAllowlist = `{
  "peers": [
    {"label": "citadel", "bot_id": "B0BGYLTM8NT"}
  ]
}`

const peerTestAllowlistImmediate = `{
  "peers": [
    {"label": "citadel", "bot_id": "B0BGYLTM8NT"}
  ],
  "immediate_channels": ["C_IMM"]
}`

func citadelResolver() *stubAuthorResolver {
	return &stubAuthorResolver{
		info:    companyBotInfo{UserID: peerTestPeerBotUserID, AppID: peerTestPeerAppID},
		outcome: botResolveOK,
	}
}

// --- registry parsing --------------------------------------------------------

func TestPeerBotsParseValidFile(t *testing.T) {
	reg := newTestPeerBotsRegistry(t, `{
	  "peers": [
	    {"label": "citadel", "bot_id": "B1"},
	    {"label": "sinan", "app_id": "A2"},
	    {"label": "boomtown", "bot_user_id": "U3"}
	  ],
	  "immediate_channels": ["C9"]
	}`)
	if got := reg.Len(); got != 3 {
		t.Errorf("Len = %d, want 3", got)
	}
	if got := reg.sortedPeerLabels(); strings.Join(got, ",") != "boomtown,citadel,sinan" {
		t.Errorf("labels = %v", got)
	}
	if !reg.immediateChannel("C9") {
		t.Error("C9 should be an immediate channel")
	}
	if reg.immediateChannel("C1") {
		t.Error("C1 should not be an immediate channel")
	}
	if !reg.hasAnyBotUserIDOnly() {
		t.Error("boomtown is a bot_user_id-only entry; hasAnyBotUserIDOnly should be true")
	}
}

func TestPeerBotsParseMissingFileYieldsEmpty(t *testing.T) {
	reg, err := newPeerBotsRegistry(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if reg.Len() != 0 {
		t.Errorf("Len = %d, want 0", reg.Len())
	}
}

func TestPeerBotsParseRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"empty label":     `{"peers": [{"label": "", "bot_id": "B1"}]}`,
		"no identifiers":  `{"peers": [{"label": "x"}]}`,
		"unknown field":   `{"peers": [], "surprise": true}`,
		"empty immediate": `{"peers": [{"label": "x", "bot_id": "B1"}], "immediate_channels": [""]}`,
		"not json":        `{`,
	}
	for name, content := range cases {
		path := writePeerBotsFile(t, t.TempDir(), content)
		if _, err := newPeerBotsRegistry(path); err == nil {
			t.Errorf("%s: want parse error, got nil", name)
		}
	}
}

func TestPeerBotsReloadStageCommit(t *testing.T) {
	dir := t.TempDir()
	path := writePeerBotsFile(t, dir, `{"peers": [{"label": "citadel", "bot_id": "B1"}]}`)
	reg, err := newPeerBotsRegistry(path)
	if err != nil {
		t.Fatalf("newPeerBotsRegistry: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"peers": [{"label": "citadel", "bot_id": "B1"}, {"label": "sinan", "bot_id": "B2"}]}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	snap, err := reg.Stage()
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	reg.Commit(snap)
	if got := reg.Len(); got != 2 {
		t.Errorf("post-reload Len = %d, want 2", got)
	}
	// Corrupt rewrite: Stage errors, live state preserved.
	if err := os.WriteFile(path, []byte(`{"peers": [{"label": ""}]}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, err := reg.Stage(); err == nil {
		t.Fatal("Stage on corrupt file: want error")
	}
	if got := reg.Len(); got != 2 {
		t.Errorf("post-failed-Stage Len = %d, want 2 (preserved)", got)
	}
}

// --- buffer ---------------------------------------------------------------

func TestPeerContextBufferCapEvictsOldest(t *testing.T) {
	b := newPeerContextBuffer()
	for i := 0; i < maxPeerContextPerChannel+3; i++ {
		b.add(peerContextItem{Channel: "C1", TS: "100." + string(rune('a'+i)), Label: "p", Text: "t"})
	}
	items, dropped := b.flush("C1")
	if len(items) != maxPeerContextPerChannel {
		t.Errorf("flush kept %d items, want %d", len(items), maxPeerContextPerChannel)
	}
	if dropped != 3 {
		t.Errorf("dropped = %d, want 3", dropped)
	}
	// Flush drains.
	items, dropped = b.flush("C1")
	if len(items) != 0 || dropped != 0 {
		t.Errorf("second flush = (%d items, %d dropped), want empty", len(items), dropped)
	}
}

func TestPeerContextBufferRestoreRequeuesAhead(t *testing.T) {
	b := newPeerContextBuffer()
	b.add(peerContextItem{Channel: "C1", TS: "1", Label: "p", Text: "first"})
	items, dropped := b.flush("C1")
	b.add(peerContextItem{Channel: "C1", TS: "2", Label: "p", Text: "second"})
	b.restore("C1", items, dropped)
	got, _ := b.flush("C1")
	if len(got) != 2 || got[0].Text != "first" || got[1].Text != "second" {
		t.Errorf("restored order wrong: %+v", got)
	}
}

// --- end-to-end through processSlackEvent ---------------------------------

func TestPeerBot_UnknownBotKeepsDropBehavior(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	resolver := citadelResolver()
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), resolver)
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "hello", "B_UNKNOWN", "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("unknown bot forwarded %d inbounds, want 0", len(msgs))
	}
	if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
		t.Errorf("unknown bot buffered %d items, want 0", len(items))
	}
	if resolver.calls != 0 {
		t.Errorf("resolver called %d times for a no-direct-match allowlist with no bot_user_id-only entries, want 0", resolver.calls)
	}
}

func TestPeerBot_BufferedByDefaultAndFlushedOnNextInbound(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), citadelResolver())

	// Peer post: no inbound forwarded, buffered instead.
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "citadel shipped the fix", peerTestPeerBotID, peerTestPeerAppID)
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Fatalf("buffered peer post forwarded %d inbounds, want 0", len(msgs))
	}

	// Next human message in the channel carries the tagged block.
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
		humanEnvelope(t, "Ev2", "C1", "100.2", "hey team, status?"), func() {})
	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("captured %d inbounds, want 1", len(msgs))
	}
	text := msgs[0].Text
	if !strings.HasPrefix(text, peerContextHeader) {
		t.Errorf("text does not start with the peer context header:\n%s", text)
	}
	if !strings.Contains(text, "peer-bot citadel posted in C1 at ts 100.1: citadel shipped the fix") {
		t.Errorf("missing tagged peer line:\n%s", text)
	}
	if !strings.Contains(text, peerContextFooter) {
		t.Errorf("missing footer:\n%s", text)
	}
	if !strings.HasSuffix(text, "hey team, status?") {
		t.Errorf("human text must follow the block:\n%s", text)
	}
	// The human inbound keeps human actor semantics.
	if msgs[0].Actor.IsBot || msgs[0].Actor.ID != "U_ALICE" {
		t.Errorf("actor = %+v, want human U_ALICE", msgs[0].Actor)
	}

	// Buffer drained: a further message carries no block.
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
		humanEnvelope(t, "Ev3", "C1", "100.3", "third"), func() {})
	msgs = capture.snapshot()
	if len(msgs) != 2 {
		t.Fatalf("captured %d inbounds, want 2", len(msgs))
	}
	if strings.Contains(msgs[1].Text, peerContextHeader) {
		t.Errorf("drained buffer re-delivered peer context:\n%s", msgs[1].Text)
	}
}

func TestPeerBot_BufferIsPerChannel(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), citadelResolver())
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "in C1", peerTestPeerBotID, "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	// Human message in a DIFFERENT channel must not carry C1's context.
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
		humanEnvelope(t, "Ev2", "C2", "100.2", "unrelated"), func() {})
	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("captured %d inbounds, want 1", len(msgs))
	}
	if strings.Contains(msgs[0].Text, "peer-bot") {
		t.Errorf("C2 inbound leaked C1 peer context:\n%s", msgs[0].Text)
	}
}

func TestPeerBot_ImmediateChannelForwardsTaggedInbound(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlistImmediate), citadelResolver())
	env := peerBotEnvelope(t, "message", "Ev1", "C_IMM", "100.5", "100.4", "done, merged", peerTestPeerBotID, peerTestPeerAppID)
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("captured %d inbounds, want 1", len(msgs))
	}
	got := msgs[0]
	if !got.Actor.IsBot {
		t.Error("Actor.IsBot must be true (provenance for gc and the intake helpers)")
	}
	if got.Actor.DisplayName != "peer-bot citadel" {
		t.Errorf("Actor.DisplayName = %q, want %q", got.Actor.DisplayName, "peer-bot citadel")
	}
	if got.Actor.ID != peerTestPeerBotUserID {
		t.Errorf("Actor.ID = %q, want resolved bot user id %q", got.Actor.ID, peerTestPeerBotUserID)
	}
	if got.ExplicitTarget != "" {
		t.Errorf("ExplicitTarget = %q, want empty (peer posts are never asks)", got.ExplicitTarget)
	}
	if !strings.Contains(got.Text, peerContextHeader) ||
		!strings.Contains(got.Text, "peer-bot citadel posted in C_IMM (thread 100.4) at ts 100.5: done, merged") {
		t.Errorf("immediate text missing tag/read-only frame:\n%s", got.Text)
	}
	if got.ReplyToMessageID != "100.4" {
		t.Errorf("ReplyToMessageID = %q, want thread ts", got.ReplyToMessageID)
	}
	if got.ProviderMessageID != "100.5" || got.DedupKey != "slack-100.5" {
		t.Errorf("provider id/dedup = (%q, %q)", got.ProviderMessageID, got.DedupKey)
	}
	// Nothing buffered on top of the immediate forward.
	if items, _ := cfg.peerContext.flush("C_IMM"); len(items) != 0 {
		t.Errorf("immediate mode also buffered %d items", len(items))
	}
}

func TestPeerBot_NeverDeliversOwnPosts(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	// Operator misconfiguration: our own bot is allowlisted. The resolver
	// answers with OUR bot user id; the self-guard must still drop.
	resolver := &stubAuthorResolver{
		info:    companyBotInfo{UserID: peerTestOwnBotUserID, AppID: "A_SELF"},
		outcome: botResolveOK,
	}
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, `{"peers": [{"label": "self-oops", "bot_id": "B_SELF"}]}`), resolver)
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "echo", "B_SELF", "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("own post forwarded %d inbounds, want 0", len(msgs))
	}
	if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
		t.Errorf("own post buffered %d items, want 0", len(items))
	}
}

func TestPeerBot_OwnAppIDDroppedBeforeResolution(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	resolver := citadelResolver()
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), resolver)
	cfg.slackAppID = "A_SELF"
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "echo", peerTestPeerBotID, "A_SELF")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("own-app post forwarded %d inbounds, want 0", len(msgs))
	}
	if resolver.calls != 0 {
		t.Errorf("resolver called %d times, want 0 (declared-id self-guard fires first)", resolver.calls)
	}
}

func TestPeerBot_TransientResolutionDropsFailClosed(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	resolver := &stubAuthorResolver{outcome: botResolveTransient}
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), resolver)
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "hello", peerTestPeerBotID, "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("transient resolution forwarded %d inbounds, want 0", len(msgs))
	}
	if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
		t.Errorf("transient resolution buffered %d items, want 0 (fail closed)", len(items))
	}
}

func TestPeerBot_AppMentionDuplicateIgnored(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), citadelResolver())
	// Same ts delivered as message AND app_mention (distinct event ids);
	// only the message delivery may buffer.
	envMsg := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "ping <@U_SELF_BOT>", peerTestPeerBotID, "")
	envAM := peerBotEnvelope(t, "app_mention", "Ev2", "C1", "100.1", "", "ping <@U_SELF_BOT>", peerTestPeerBotID, "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, envMsg, func() {})
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, envAM, func() {})

	items, _ := cfg.peerContext.flush("C1")
	if len(items) != 1 {
		t.Errorf("buffered %d items for one peer post delivered twice, want 1", len(items))
	}
}

func TestPeerBot_MentionOfOurBotAddsNoBusyMark(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), citadelResolver())
	cfg.slackBotToken = "xoxb-fake"
	cfg.busyReaction = busyReactionDefault
	cfg.busyMarks = newBusyReactionRegistry()
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "<@U_SELF_BOT> fyi", peerTestPeerBotID, "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if _, ok := cfg.busyMarks.pending("C1", "100.1"); ok {
		t.Error("peer post must never busy-mark (no reply affordance)")
	}
}

func TestPeerBot_BotUserIDOnlyEntryMatchesViaResolver(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := peerTestConfig(gcStub.URL,
		newTestPeerBotsRegistry(t, `{"peers": [{"label": "boomtown", "bot_user_id": "U_CITADEL_BOT"}]}`),
		citadelResolver())
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "hi", peerTestPeerBotID, "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	items, _ := cfg.peerContext.flush("C1")
	if len(items) != 1 || items[0].Label != "boomtown" {
		t.Errorf("bot_user_id-only entry did not match: %+v", items)
	}
}

func TestPeerBot_ResolutionMismatchDrops(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	// Event declares app A_LIAR but bots.info resolves to A_CITADEL: the
	// corroboration mismatch must drop the post.
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), citadelResolver())
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "hi", peerTestPeerBotID, "A_LIAR")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
		t.Errorf("mismatched author buffered %d items, want 0", len(items))
	}
}

func TestPeerBot_EmptyAllowlistIsInert(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	resolver := citadelResolver()
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, `{"peers": []}`), resolver)
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "hi", peerTestPeerBotID, "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("empty allowlist forwarded %d inbounds, want 0", len(msgs))
	}
	if resolver.calls != 0 {
		t.Errorf("resolver called %d times on empty allowlist, want 0", resolver.calls)
	}
}

func TestFormatPeerContextBlockTruncatesLongPosts(t *testing.T) {
	long := strings.Repeat("x", maxPeerContextItemChars+50)
	block := formatPeerContextBlock([]peerContextItem{{Label: "p", Channel: "C1", TS: "1", Text: long}}, 0)
	if !strings.Contains(block, "…[truncated]") {
		t.Error("long post not truncated")
	}
	if strings.Contains(block, long) {
		t.Error("full text leaked past the cap")
	}
}

func TestFormatPeerContextBlockReportsDrops(t *testing.T) {
	block := formatPeerContextBlock([]peerContextItem{{Label: "p", Channel: "C1", TS: "1", Text: "t"}}, 4)
	if !strings.Contains(block, "(4 older peer post(s) were dropped from this buffer)") {
		t.Errorf("missing drop note:\n%s", block)
	}
}
