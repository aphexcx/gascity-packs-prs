package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Tests for jg-ure5r8 — peer-bot replies in the thread-context preamble.
//
// Measured defect (C0ASAPRETDK thread 1789073983.547099, 9/11): the
// preamble listed only the human's messages and dropped every reply by
// the sister city's mayor bot, so the human's short answer to that bot
// (「yes sling the fix bead」) was read as addressed to us. The blanket
// `bot_id != ""` filter is replaced by a classifier: own posts still
// drop, peer bots quote under their display name.

const (
	tcSelfAppID     = "A_SELF"
	tcSelfBotUserID = "U_SELF_BOT"
	tcPeerBotID     = "B0BGYLTM8NT"
	tcPeerAppID     = "A_CITADEL"
	tcPeerBotUserID = "U_CITADEL_BOT"
	tcOwnBotID      = "B_SELF"
)

// tcSelfClassifier knows both self identities and has no resolver, so
// only the wire fields can prove not-self.
func tcSelfClassifier() *threadBotClassifier {
	return &threadBotClassifier{selfAppIDs: []string{tcSelfAppID}, selfUserIDs: []string{tcSelfBotUserID}}
}

// citadelShape is the live conversations.replies shape of a sister-city
// adapter post (9/11 API read): classic bot_message, username, app_id,
// no user, no bot_profile.
func citadelShape(ts, text string) slackThreadMessage {
	return slackThreadMessage{Subtype: "bot_message", Username: "Citadel Mayor", BotID: tcPeerBotID, AppID: tcPeerAppID, TS: ts, Text: text}
}

// ownShape is the live shape of this adapter's own post: app user +
// bot_profile carrying name / app_id / user_id.
func ownShape(ts, text string) slackThreadMessage {
	return slackThreadMessage{
		User: tcSelfBotUserID, BotID: tcOwnBotID, AppID: tcSelfAppID, TS: ts, Text: text,
		BotProfile: json.RawMessage(`{"id":"` + tcOwnBotID + `","name":"Sinan Mayor","app_id":"` + tcSelfAppID + `","user_id":"` + tcSelfBotUserID + `"}`),
	}
}

func TestThreadBotClassifier_AllowlistLabelWins(t *testing.T) {
	reg := newTestPeerBotsRegistry(t, `{"peers": [{"label": "citadel-mayor", "app_id": "`+tcPeerAppID+`"}]}`)
	c := tcSelfClassifier()
	c.peers = reg
	label, include := c.classify(citadelShape("1.0", "hi"))
	if !include || label != "citadel-mayor (peer bot)" {
		t.Fatalf("got (%q, %v), want allowlist label", label, include)
	}

	// bot_user_id-only entries match through the resolved/wire user id.
	reg2 := newTestPeerBotsRegistry(t, `{"peers": [{"label": "by-user", "bot_user_id": "`+tcPeerBotUserID+`"}]}`)
	c.peers = reg2
	label, include = c.classify(slackThreadMessage{User: tcPeerBotUserID, BotID: tcPeerBotID, AppID: tcPeerAppID, TS: "1.0", Text: "x", Username: "wire"})
	if !include || label != "by-user (peer bot)" {
		t.Fatalf("got (%q, %v), want bot_user_id allowlist label", label, include)
	}
}

func TestPreamblePeerBotHonoursDeliveredCollapse(t *testing.T) {
	// A peer post this audience already received as its own inbound
	// (an immediate-mode peer wake) collapses like any other delivered
	// prior — the budget rule is unchanged by the classifier.
	replies := []slackThreadMessage{
		citadelShape("1.0", "already delivered peer post"),
		{User: "U_AFIK", Text: "reply", TS: "2.0"},
	}
	got := formatThreadContextPreamble(replies, "", "3.0", "", nil,
		func(ts string) bool { return ts == "1.0" }, tcSelfClassifier().classify)
	if !strings.Contains(got, "1 earlier message already delivered.") {
		t.Fatalf("delivered peer post must collapse:\n%s", got)
	}
	if strings.Contains(got, "already delivered peer post") {
		t.Fatalf("delivered peer post must not be re-quoted:\n%s", got)
	}
}

func TestThreadContext_InboundCarriesPeerBotReplies(t *testing.T) {
	// End to end through processSlackEvent with the live wire shapes:
	// the envelope's api_app_id and bot authorization are the self
	// identities, as on every real Events API / Socket Mode delivery.
	currentTS := "100.000005"
	prior := []slackThreadMessage{
		{User: "U_AFIK", Text: "this scene takes forever", TS: "100.000001"},
		citadelShape("100.000002", "Looked. Ask: your word on a fix bead."),
		{User: "U_AFIK", Text: "yes sling the fix bead", TS: "100.000003"},
		ownShape("100.000004", "our own reflected reply"),
		{User: "U_AFIK", Text: "@mayor what is that bead?", TS: currentTS},
	}
	slackStub, calls := fakeSlackRepliesServer(t, prior)
	withSlackAPIStub(t, slackStub)

	capture := &inboundCapture{}
	gcStub := httptest.NewServer(capture.handler())
	t.Cleanup(gcStub.Close)

	cfg := config{
		gcAPIBase:               gcStub.URL,
		cityName:                "test-city",
		provider:                "slack",
		accountID:               "T1",
		handlePrefix:            "@",
		slackBotToken:           "xoxb-fake",
		slackThreadContextLimit: 20,
		threadContextCache:      newThreadContextCache(),
		dispatchSem:             defaultTestDispatchSem,
		// No SLACK_APP_ID / switchboard user configured: the envelope
		// alone must be enough to prove the peer is not us.
	}
	rawMsg, _ := json.Marshal(slackMessageEvent{
		Type:     "message",
		Channel:  "C1",
		User:     "U_AFIK",
		TS:       currentTS,
		ThreadTS: "100.000001",
		Text:     "@mayor what is that bead?",
	})
	env := slackEventEnvelope{
		Type:           "event_callback",
		Event:          rawMsg,
		APIAppID:       tcSelfAppID,
		Authorizations: []slackEventAuthorization{{UserID: tcSelfBotUserID, IsBot: true}},
	}

	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("conversations.replies calls = %d, want 1", got)
	}
	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("captured %d inbound messages, want 1", len(msgs))
	}
	text := msgs[0].Text
	if !strings.Contains(text, "@Citadel Mayor (bot): Looked. Ask: your word on a fix bead.") {
		t.Fatalf("inbound must quote the peer bot reply under its label; got %q", text)
	}
	if !strings.Contains(text, "@U_AFIK: yes sling the fix bead") {
		t.Fatalf("human reply after the peer must still quote; got %q", text)
	}
	if !strings.Contains(text, "(own bot): our own reflected reply") {
		t.Fatalf("own history post must be quoted with provenance; got %q", text)
	}
	if strings.Index(text, "Citadel Mayor (bot)") > strings.Index(text, "yes sling the fix bead") {
		t.Fatalf("peer reply must precede the human's answer to it; got %q", text)
	}
}

func TestInboundSpoolRoundTripsLeanPreamble(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	s := newInboundSpool(path)
	withBots := testPending("C1", "1.0", "a message")
	withBots.preamble, withBots.preambleLean, withBots.botQuotes, withBots.body = "full\n", "lean\n", true, "a message"
	// A bot-only delta: lean is legitimately empty but still a fallback.
	botOnly := testPending("C1", "2.0", "b message")
	botOnly.preamble, botOnly.preambleLean, botOnly.botQuotes, botOnly.body = "bots\n", "", true, "b message"
	// Human-only: no duplicate lean copy on the line (codex r5 P2).
	humanOnly := testPending("C1", "3.0", "c message")
	humanOnly.preamble, humanOnly.body = "human\n", "c message"
	if !s.spillBatch("C1", []pendingChannelInbound{withBots, botOnly, humanOnly}) {
		t.Fatal("spillBatch reported failure for a healthy spool")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "preamble_lean") != 1 {
		t.Fatalf("only the entry with bot quotes and a non-empty lean writes preamble_lean:\n%s", raw)
	}
	entries, done, err := s.consume()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if len(entries) != 3 {
		t.Fatalf("consumed %d entries, want 3", len(entries))
	}
	byTS := map[string]spooledInbound{}
	for _, e := range entries {
		byTS[e.Inbound.ProviderMessageID] = e
	}
	if e := byTS["1.0"]; e.Preamble != "full\n" || e.PreambleLean != "lean\n" || !e.BotQuotes {
		t.Fatalf("bot-quotes entry must carry both preambles + flag: %+v", e)
	}
	if e := byTS["2.0"]; e.Preamble != "bots\n" || e.PreambleLean != "" || !e.BotQuotes {
		t.Fatalf("bot-only entry must keep the flag with an empty lean: %+v", e)
	}
	if e := byTS["3.0"]; e.Preamble != "human\n" || e.PreambleLean != "" || e.BotQuotes {
		t.Fatalf("human-only entry must carry no lean copy: %+v", e)
	}
	replayed := pendingChannelInbound{threadAnchor: byTS["2.0"].ThreadAnchor, preamble: byTS["2.0"].Preamble, preambleLean: byTS["2.0"].PreambleLean, botQuotes: byTS["2.0"].BotQuotes, body: byTS["2.0"].Body}
	if parts, ok := replayed.reminderParts("C1"); !ok || !parts.botQuotes {
		t.Fatalf("replayed bot-only entry must keep its fallback flag: %+v", parts)
	}
}
