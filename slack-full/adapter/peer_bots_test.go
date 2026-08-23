package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for legacy-path bot-post visibility (gp-kop, generalized by gp-9e7
// item 3): every bot post that survives the fail-closed author resolution
// delivers to the channel-bound session as tagged read-only context —
// buffered onto the next natural inbound by default; a WAKE (immediate
// forward) only for allowlisted peers granted one via per-entry "wake" or
// immediate_channels — while the adapter's own posts always drop.

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
}

func TestPeerBotsParseWakeFlag(t *testing.T) {
	reg := newTestPeerBotsRegistry(t, `{
	  "peers": [
	    {"label": "citadel", "bot_id": "B1", "wake": true},
	    {"label": "sinan", "bot_id": "B2"}
	  ]
	}`)
	if entry, ok := reg.matchPeer("B1", ""); !ok || !entry.Wake {
		t.Errorf("citadel entry = (%+v, %v), want matched with Wake=true", entry, ok)
	}
	if entry, ok := reg.matchPeer("B2", ""); !ok || entry.Wake {
		t.Errorf("sinan entry = (%+v, %v), want matched with Wake=false (default)", entry, ok)
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

// gp-9e7 item 3: a bot with no allowlist entry no longer drops — it
// buffers as tagged read-only context under a best-effort label, and it
// can never wake (no immediate forward, ever).
func TestPeerBot_UnknownBotBuffersNeverWakes(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	resolver := &stubAuthorResolver{
		info:    companyBotInfo{UserID: "U_GITHUB_BOT", AppID: "A_GITHUB", Name: "github"},
		outcome: botResolveOK,
	}
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), resolver)
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "PR #14 merged", "B_UNKNOWN", "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Fatalf("unknown bot forwarded %d inbounds, want 0 (buffer only, never a wake)", len(msgs))
	}
	if resolver.calls != 1 {
		t.Errorf("resolver called %d times, want 1 (fail-closed self-check runs for every bot post)", resolver.calls)
	}
	items, _ := cfg.peerContext.flush("C1")
	if len(items) != 1 {
		t.Fatalf("buffered %d items, want 1", len(items))
	}
	if items[0].Label != "github" {
		t.Errorf("label = %q, want bots.info name %q", items[0].Label, "github")
	}
	if items[0].Text != "PR #14 merged" {
		t.Errorf("text = %q", items[0].Text)
	}
}

// An unknown bot in an immediate_channels channel still only buffers:
// the wake grant belongs to allowlisted peers alone (safety rule 2).
func TestPeerBot_UnknownBotNoWakeEvenInImmediateChannel(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	resolver := &stubAuthorResolver{
		info:    companyBotInfo{UserID: "U_GITHUB_BOT", AppID: "A_GITHUB"},
		outcome: botResolveOK,
	}
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlistImmediate), resolver)
	env := peerBotEnvelope(t, "message", "Ev1", "C_IMM", "100.1", "", "nightly run green", "B_UNKNOWN", "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Fatalf("unknown bot woke the session in an immediate channel: %d inbounds", len(msgs))
	}
	items, _ := cfg.peerContext.flush("C_IMM")
	if len(items) != 1 || items[0].Label != "U_GITHUB_BOT" {
		t.Errorf("items = %+v, want one entry labeled by resolved bot user id", items)
	}
}

// Unknown-bot label preference: bot_profile.name beats the bots.info
// name, which beats the resolved ids.
func TestUnknownBotLabelPreference(t *testing.T) {
	withProfile := slackMessageEvent{BotProfile: json.RawMessage(`{"name": "Deploy Bot", "app_id": "A1"}`)}
	if got := unknownBotLabel(withProfile, "B1", companyBotInfo{Name: "deploybot", UserID: "U1"}); got != "Deploy Bot" {
		t.Errorf("label = %q, want bot_profile name", got)
	}
	if got := unknownBotLabel(slackMessageEvent{}, "B1", companyBotInfo{Name: "deploybot", UserID: "U1"}); got != "deploybot" {
		t.Errorf("label = %q, want bots.info name", got)
	}
	if got := unknownBotLabel(slackMessageEvent{}, "B1", companyBotInfo{UserID: "U1"}); got != "U1" {
		t.Errorf("label = %q, want resolved user id", got)
	}
	if got := unknownBotLabel(slackMessageEvent{}, "B1", companyBotInfo{}); got != "B1" {
		t.Errorf("label = %q, want bot id fallback", got)
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

// --- fail-closed self-guard (gp-9e7 fix round 3a) ---------------------------

// With NO usable self identity anywhere (no SLACK_APP_ID, no
// SLACK_SWITCHBOARD_BOT_USER_ID, no authorizations, no api_app_id on
// the envelope), the guard cannot prove the author is not self — so
// even an allowlisted bot with a wake grant must drop, not deliver.
func TestPeerBot_MissingSelfIdentityFailsClosed(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	resolver := citadelResolver()
	cfg := peerTestConfig(gcStub.URL,
		newTestPeerBotsRegistry(t, `{"peers": [{"label": "citadel", "bot_id": "B0BGYLTM8NT", "wake": true}]}`),
		resolver)
	rawMsg, err := json.Marshal(slackMessageEvent{
		Type: "message", Channel: "C1", BotID: peerTestPeerBotID,
		TS: "100.1", Text: "wake me",
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	// No Authorizations, no APIAppID: nothing to compare the resolved
	// author identity against.
	env := slackEventEnvelope{Type: "event_callback", EventID: "Ev1", Event: rawMsg}
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("unprovable author WOKE the session: %d inbounds, want 0 (fail closed)", len(msgs))
	}
	if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
		t.Errorf("unprovable author buffered %d items, want 0 (fail closed)", len(items))
	}
}

// bots.info can succeed WITHOUT a user_id (Slack-documented). With no
// resolved app id either, the resolution proves nothing — drop, even
// though the envelope carries self identities to compare against.
func TestPeerBot_ResolutionWithoutIdentitiesFailsClosed(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	resolver := &stubAuthorResolver{
		info:    companyBotInfo{Name: "mystery"}, // bots.info ok=true, no user_id, no app_id
		outcome: botResolveOK,
	}
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), resolver)
	cfg.slackAppID = "A_SELF"
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "who am i", "B_UNKNOWN", "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("identity-free resolution forwarded %d inbounds, want 0", len(msgs))
	}
	if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
		t.Errorf("identity-free resolution buffered %d items, want 0 (fail closed)", len(items))
	}
}

// A resolution that carries ONLY an app id still proves not-self when a
// self app id is known — the app-id pair alone is authoritative, so a
// user_id-less bots.info answer does not needlessly drop real peers.
func TestPeerBot_AppIDPairAloneProvesNotSelf(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	resolver := &stubAuthorResolver{
		info:    companyBotInfo{AppID: "A_GITHUB", Name: "github"}, // no user_id
		outcome: botResolveOK,
	}
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), resolver)
	cfg.slackAppID = "A_SELF"
	rawMsg, err := json.Marshal(slackMessageEvent{
		Type: "message", Channel: "C1", BotID: "B_UNKNOWN",
		TS: "100.1", Text: "PR merged",
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	env := slackEventEnvelope{Type: "event_callback", EventID: "Ev1", Event: rawMsg}
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("unknown bot woke the session: %d inbounds, want 0", len(msgs))
	}
	items, _ := cfg.peerContext.flush("C1")
	if len(items) != 1 || items[0].Label != "github" {
		t.Errorf("items = %+v, want the app-id-proven bot buffered", items)
	}
}

// The envelope's api_app_id is authoritative self evidence even when
// SLACK_APP_ID is unset: a resolution matching it IS our own app.
func TestPeerBot_EnvelopeAPIAppIDDropsOwnApp(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	resolver := &stubAuthorResolver{
		info:    companyBotInfo{UserID: "U_SOMEONE", AppID: "A_SELF"},
		outcome: botResolveOK,
	}
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), resolver)
	rawMsg, err := json.Marshal(slackMessageEvent{
		Type: "message", Channel: "C1", BotID: "B_SELF",
		TS: "100.1", Text: "echo",
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	env := slackEventEnvelope{Type: "event_callback", EventID: "Ev1", Event: rawMsg, APIAppID: "A_SELF"}
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("own-app post (via api_app_id) forwarded %d inbounds, want 0", len(msgs))
	}
	if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
		t.Errorf("own-app post (via api_app_id) buffered %d items, want 0", len(items))
	}
}

// --- human corroboration (gp-9e7 fix round 3b) -------------------------------

// A bot-shaped event carrying a `user` the resolution does not
// corroborate may be a HUMAN message dressed as a bot: it must never be
// buffered or woken as one — mismatched AND absent resolved user ids
// both drop.
func TestPeerBot_BotShapedEventWithHumanUserDropped(t *testing.T) {
	for name, info := range map[string]companyBotInfo{
		"mismatched": {UserID: peerTestPeerBotUserID, AppID: peerTestPeerAppID},
		"absent":     {AppID: peerTestPeerAppID},
	} {
		t.Run(name, func(t *testing.T) {
			capture := &inboundCapture{}
			gcStub := httptest.NewServer(capture.handler())
			t.Cleanup(gcStub.Close)

			resolver := &stubAuthorResolver{info: info, outcome: botResolveOK}
			cfg := peerTestConfig(gcStub.URL,
				newTestPeerBotsRegistry(t, `{"peers": [{"label": "citadel", "bot_id": "B0BGYLTM8NT", "wake": true}]}`),
				resolver)
			rawMsg, err := json.Marshal(slackMessageEvent{
				Type: "message", Channel: "C1", BotID: peerTestPeerBotID,
				User: "U_HUMAN", TS: "100.1", Text: "looks like a bot",
			})
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}
			env := slackEventEnvelope{
				Type: "event_callback", EventID: "Ev1", Event: rawMsg,
				Authorizations: []slackEventAuthorization{{UserID: peerTestOwnBotUserID, IsBot: true}},
			}
			processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

			if msgs := capture.snapshot(); len(msgs) != 0 {
				t.Errorf("bot-shaped event with uncorroborated user WOKE the session: %d inbounds, want 0", len(msgs))
			}
			if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
				t.Errorf("bot-shaped event with uncorroborated user buffered %d items, want 0", len(items))
			}
		})
	}
}

// A corroborated user (msg.user == resolved user_id) keeps delivering.
func TestPeerBot_BotEventWithCorroboratedUserStillBuffers(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), citadelResolver())
	rawMsg, err := json.Marshal(slackMessageEvent{
		Type: "message", Channel: "C1", BotID: peerTestPeerBotID,
		User: peerTestPeerBotUserID, TS: "100.1", Text: "hi from citadel",
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	env := slackEventEnvelope{
		Type: "event_callback", EventID: "Ev1", Event: rawMsg,
		Authorizations: []slackEventAuthorization{{UserID: peerTestOwnBotUserID, IsBot: true}},
	}
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	items, _ := cfg.peerContext.flush("C1")
	if len(items) != 1 || items[0].Label != "citadel" {
		t.Errorf("items = %+v, want the corroborated peer post buffered", items)
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

// gp-9e7 item 3: an empty allowlist no longer disables visibility — bot
// posts still buffer (the allowlist only grants wakes and labels).
func TestPeerBot_EmptyAllowlistStillBuffers(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	resolver := citadelResolver()
	cfg := peerTestConfig(gcStub.URL, newTestPeerBotsRegistry(t, `{"peers": []}`), resolver)
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "hi", peerTestPeerBotID, "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Errorf("empty allowlist forwarded %d inbounds, want 0 (buffer only)", len(msgs))
	}
	items, _ := cfg.peerContext.flush("C1")
	if len(items) != 1 || items[0].Label != peerTestPeerBotUserID {
		t.Errorf("items = %+v, want one entry labeled by resolved bot user id", items)
	}
}

// A wake-flagged allowlist entry forwards immediately in ANY channel.
func TestPeerBot_WakeEntryForwardsImmediately(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := peerTestConfig(gcStub.URL,
		newTestPeerBotsRegistry(t, `{"peers": [{"label": "citadel", "bot_id": "B0BGYLTM8NT", "wake": true}]}`),
		citadelResolver())
	env := peerBotEnvelope(t, "message", "Ev1", "C_ANY", "100.1", "", "urgent handoff", peerTestPeerBotID, "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("captured %d inbounds, want 1 (wake entry forwards immediately)", len(msgs))
	}
	if msgs[0].Actor.DisplayName != "peer-bot citadel" {
		t.Errorf("Actor.DisplayName = %q", msgs[0].Actor.DisplayName)
	}
	if items, _ := cfg.peerContext.flush("C_ANY"); len(items) != 0 {
		t.Errorf("wake entry also buffered %d items", len(items))
	}
}

// gp-bhq (D): an app_id-only allowlist entry can never match a SPARSE
// event (one carrying neither app_id nor bot_profile.app_id on the
// wire), but once bots.info resolves the author, the allowlist must be
// re-matched with the RESOLVED app id — otherwise the configured peer
// silently degrades to an unknown bot, losing its label and its
// wake grant.
func TestPeerBot_AppIDOnlyEntryRematchesViaResolvedAppID(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := peerTestConfig(gcStub.URL,
		newTestPeerBotsRegistry(t, `{"peers": [{"label": "citadel", "app_id": "A_CITADEL", "wake": true}]}`),
		citadelResolver())
	// Sparse event: bot_id only — no app_id, no bot_profile anywhere.
	env := peerBotEnvelope(t, "message", "Ev1", "C1", "100.1", "", "handoff ready", peerTestPeerBotID, "")
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("captured %d inbounds, want 1 — the resolved-app_id re-match must keep the wake grant", len(msgs))
	}
	if msgs[0].Actor.DisplayName != "peer-bot citadel" {
		t.Errorf("Actor.DisplayName = %q, want the allowlist label, not an unknown-bot fallback", msgs[0].Actor.DisplayName)
	}
	if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
		t.Errorf("wake peer also buffered %d item(s)", len(items))
	}
}

// The resolved-app_id re-match runs only AFTER not-self is proven: the
// same sparse event with NO comparable self identity (no envelope
// authorizations or api_app_id, no SLACK_APP_ID, no switchboard bot
// user id) must still drop fail-closed — never wake, never buffer.
func TestPeerBot_AppIDOnlyRematchNeverBypassesSelfProof(t *testing.T) {
	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := peerTestConfig(gcStub.URL,
		newTestPeerBotsRegistry(t, `{"peers": [{"label": "citadel", "app_id": "A_CITADEL", "wake": true}]}`),
		citadelResolver())
	rawMsg, err := json.Marshal(slackMessageEvent{
		Type: "message", Channel: "C1", BotID: peerTestPeerBotID, TS: "100.1", Text: "handoff ready",
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	// No Authorizations, no APIAppID: nothing to prove not-self against.
	env := slackEventEnvelope{Type: "event_callback", EventID: "Ev1", Event: rawMsg}
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if msgs := capture.snapshot(); len(msgs) != 0 {
		t.Fatalf("forwarded %d inbounds, want 0 — self-proof failure must drop before any allowlist match", len(msgs))
	}
	if items, _ := cfg.peerContext.flush("C1"); len(items) != 0 {
		t.Fatalf("buffered %d item(s), want 0 — self-proof failure must drop fail-closed", len(items))
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
