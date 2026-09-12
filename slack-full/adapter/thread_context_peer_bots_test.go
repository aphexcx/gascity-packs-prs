package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
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

func TestThreadBotClassifier_WireFields(t *testing.T) {
	cases := []struct {
		name        string
		c           *threadBotClassifier
		m           slackThreadMessage
		wantInclude bool
		wantLabel   string
	}{
		{
			name: "peer classic bot_message quotes under its username",
			c:    tcSelfClassifier(), m: citadelShape("1.0", "hi"),
			wantInclude: true, wantLabel: "Citadel Mayor",
		},
		{
			name: "own post by app id drops",
			c:    tcSelfClassifier(), m: ownShape("1.0", "mine"),
			wantInclude: false,
		},
		{
			name:        "own post by bot user id alone drops",
			c:           &threadBotClassifier{selfUserIDs: []string{tcSelfBotUserID}},
			m:           slackThreadMessage{User: tcSelfBotUserID, BotID: tcOwnBotID, TS: "1.0", Text: "mine"},
			wantInclude: false,
		},
		{
			name: "own post identified only through bot_profile drops",
			c:    tcSelfClassifier(),
			m: slackThreadMessage{BotID: tcOwnBotID, TS: "1.0", Text: "mine",
				BotProfile: json.RawMessage(`{"app_id":"` + tcSelfAppID + `"}`)},
			wantInclude: false,
		},
		{
			name: "peer with bot_profile name prefers that name over username",
			c:    tcSelfClassifier(),
			m: slackThreadMessage{User: tcPeerBotUserID, BotID: tcPeerBotID, AppID: tcPeerAppID, TS: "1.0", Text: "x", Username: "legacy",
				BotProfile: json.RawMessage(`{"name":"  Citadel\nMayor ","app_id":"` + tcPeerAppID + `","user_id":"` + tcPeerBotUserID + `"}`)},
			wantInclude: true, wantLabel: "Citadel Mayor",
		},
		{
			name: "no self identity known: fail closed",
			c:    &threadBotClassifier{}, m: citadelShape("1.0", "hi"),
			wantInclude: false,
		},
		{
			name:        "self app known but post carries only a user id: unprovable, drops",
			c:           &threadBotClassifier{selfAppIDs: []string{tcSelfAppID}},
			m:           slackThreadMessage{User: tcPeerBotUserID, BotID: tcPeerBotID, TS: "1.0", Text: "x"},
			wantInclude: false,
		},
		{
			name:        "peer with no name at all falls back to its bot user id",
			c:           tcSelfClassifier(),
			m:           slackThreadMessage{User: tcPeerBotUserID, BotID: tcPeerBotID, AppID: tcPeerAppID, TS: "1.0", Text: "x"},
			wantInclude: true, wantLabel: tcPeerBotUserID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label, include := tc.c.classify(tc.m)
			if include != tc.wantInclude {
				t.Fatalf("include = %v, want %v (label %q)", include, tc.wantInclude, label)
			}
			if include && label != tc.wantLabel {
				t.Fatalf("label = %q, want %q", label, tc.wantLabel)
			}
		})
	}
}

func TestThreadBotClassifier_AllowlistLabelWins(t *testing.T) {
	reg := newTestPeerBotsRegistry(t, `{"peers": [{"label": "citadel-mayor", "app_id": "`+tcPeerAppID+`"}]}`)
	c := tcSelfClassifier()
	c.peers = reg
	label, include := c.classify(citadelShape("1.0", "hi"))
	if !include || label != "citadel-mayor" {
		t.Fatalf("got (%q, %v), want allowlist label", label, include)
	}

	// bot_user_id-only entries match through the resolved/wire user id.
	reg2 := newTestPeerBotsRegistry(t, `{"peers": [{"label": "by-user", "bot_user_id": "`+tcPeerBotUserID+`"}]}`)
	c.peers = reg2
	label, include = c.classify(slackThreadMessage{User: tcPeerBotUserID, BotID: tcPeerBotID, AppID: tcPeerAppID, TS: "1.0", Text: "x", Username: "wire"})
	if !include || label != "by-user" {
		t.Fatalf("got (%q, %v), want bot_user_id allowlist label", label, include)
	}
}

func TestThreadBotClassifier_ResolverFallback(t *testing.T) {
	// Sparse wire: only bot_id. The resolver settles identity.
	sparse := slackThreadMessage{BotID: tcPeerBotID, TS: "1.0", Text: "x"}

	t.Run("resolved peer quotes under bots.info name", func(t *testing.T) {
		res := &stubAuthorResolver{info: companyBotInfo{UserID: tcPeerBotUserID, AppID: tcPeerAppID, Name: "Citadel Mayor"}, outcome: botResolveOK}
		c := tcSelfClassifier()
		c.authors = res
		label, include := c.classify(sparse)
		if !include || label != "Citadel Mayor" {
			t.Fatalf("got (%q, %v), want resolved peer", label, include)
		}
		if res.calls != 1 {
			t.Fatalf("resolver calls = %d, want 1", res.calls)
		}
	})
	t.Run("resolved self drops", func(t *testing.T) {
		res := &stubAuthorResolver{info: companyBotInfo{UserID: tcSelfBotUserID, AppID: tcSelfAppID}, outcome: botResolveOK}
		c := tcSelfClassifier()
		c.authors = res
		if _, include := c.classify(sparse); include {
			t.Fatal("resolved self identity must drop")
		}
	})
	t.Run("transient resolver failure drops, one call per bot per preamble", func(t *testing.T) {
		res := &stubAuthorResolver{outcome: botResolveTransient}
		c := tcSelfClassifier()
		c.authors = res
		for i := 0; i < 5; i++ {
			if _, include := c.classify(sparse); include {
				t.Fatal("unresolved bot must drop fail-closed")
			}
		}
		if res.calls != 1 {
			t.Fatalf("resolver calls = %d, want 1 (memoised per bot id)", res.calls)
		}
		other := slackThreadMessage{BotID: "B_OTHER", TS: "2.0", Text: "y"}
		c.classify(other)
		if res.calls != 2 {
			t.Fatalf("resolver calls = %d, want 2 (a second bot id resolves once)", res.calls)
		}
	})
	t.Run("wire contradicting the resolution drops", func(t *testing.T) {
		res := &stubAuthorResolver{info: companyBotInfo{UserID: tcPeerBotUserID, AppID: "A_OTHER"}, outcome: botResolveOK}
		c := &threadBotClassifier{selfUserIDs: []string{tcSelfBotUserID}, authors: res}
		// app id on the wire, but no self app id to compare → resolver
		// runs and contradicts the wire app id.
		m := slackThreadMessage{BotID: tcPeerBotID, AppID: tcPeerAppID, TS: "1.0", Text: "x"}
		if _, include := c.classify(m); include {
			t.Fatal("contradicted author must drop")
		}
	})
	t.Run("wire proof skips the resolver", func(t *testing.T) {
		res := &stubAuthorResolver{outcome: botResolveTransient}
		c := tcSelfClassifier()
		c.authors = res
		if _, include := c.classify(citadelShape("1.0", "hi")); !include {
			t.Fatal("wire-proven peer must quote without the resolver")
		}
		if res.calls != 0 {
			t.Fatalf("resolver calls = %d, want 0", res.calls)
		}
	})
}

func TestPreambleQuotesPeerBotsAndDropsOwnPosts(t *testing.T) {
	replies := []slackThreadMessage{
		{User: "U_AFIK", Text: "this scene takes forever, what gives?", TS: "1.0"},
		citadelShape("2.0", "Looked. Ask: your word on a fix bead."),
		{User: "U_AFIK", Text: "yes sling the fix bead", TS: "3.0"},
		ownShape("4.0", "our own reflected reply"),
		{User: "U_AFIK", Text: "@jadegate what is that bead?", TS: "5.0"},
	}
	got := formatThreadContextPreamble(replies, "5.0", "", nil, nil, tcSelfClassifier().classify)

	if !strings.Contains(got, "Thread context (3 earlier messages):") {
		t.Fatalf("want human + peer priors counted, got:\n%s", got)
	}
	want := "@U_AFIK: this scene takes forever, what gives?\n@Citadel Mayor (bot): Looked. Ask: your word on a fix bead.\n@U_AFIK: yes sling the fix bead\n"
	if !strings.Contains(got, want) {
		t.Fatalf("peer post must sit in thread order under its label:\nwant %q\ngot  %q", want, got)
	}
	if strings.Contains(got, "reflected reply") {
		t.Fatalf("own post must not be re-quoted:\n%s", got)
	}

	// nil classifier keeps the pre-fix behaviour (every bot drops).
	legacy := formatThreadContextPreamble(replies, "5.0", "", nil, nil, nil)
	if strings.Contains(legacy, "(bot)") || !strings.Contains(legacy, "Thread context (2 earlier messages):") {
		t.Fatalf("nil classifier must drop every bot post:\n%s", legacy)
	}
}

func TestPreambleClipsLongPeerBotPosts(t *testing.T) {
	// Eight ~1 KB peer reports in one thread (the 9/11 shape) must not
	// push the preamble over the channel reminder budget, where it is
	// shed whole: bot posts clip per post, human posts do not.
	long := strings.Repeat("报告 report line. ", 60) // ~1.3 KB, CJK + ASCII
	humanLong := strings.Repeat("h", 400)
	replies := []slackThreadMessage{
		citadelShape("1.0", long),
		{User: "U_AFIK", Text: humanLong, TS: "2.0"},
	}
	got := formatThreadContextPreamble(replies, "3.0", "", nil, nil, tcSelfClassifier().classify)
	lines := strings.Split(got, "\n")
	var botLine, humanLine string
	for _, l := range lines {
		if strings.HasPrefix(l, "@Citadel Mayor (bot): ") {
			botLine = strings.TrimPrefix(l, "@Citadel Mayor (bot): ")
		}
		if strings.HasPrefix(l, "@U_AFIK: ") {
			humanLine = strings.TrimPrefix(l, "@U_AFIK: ")
		}
	}
	if botLine == "" || humanLine == "" {
		t.Fatalf("both priors must quote:\n%s", got)
	}
	if !strings.HasSuffix(botLine, "…") || len(botLine) > threadContextBotPostBytes+len("…") || !utf8.ValidString(botLine) {
		t.Fatalf("peer post must clip rune-safe to <= %d bytes + ellipsis, got %d bytes %q", threadContextBotPostBytes, len(botLine), botLine)
	}
	if humanLine != humanLong {
		t.Fatalf("human post must stay unclipped, got %q", humanLine)
	}
	// Eight such peer posts plus the human lines now fit the default
	// budget with room for anchor + body.
	var many []slackThreadMessage
	for i := 0; i < 8; i++ {
		many = append(many, citadelShape(fmt.Sprintf("1.%d", i), long), slackThreadMessage{User: "U_AFIK", Text: "short human reply", TS: fmt.Sprintf("2.%d", i)})
	}
	got = formatThreadContextPreamble(many, "9.0", "", nil, nil, tcSelfClassifier().classify)
	if len(got) > defaultReminderTextBudget-500 {
		t.Fatalf("clipped preamble is %d bytes, must leave headroom under the %d budget", len(got), defaultReminderTextBudget)
	}
}

func TestPreambleBotAllowanceKeepsHumanLines(t *testing.T) {
	// codex r2 P2: twelve long peer reports and one short human reply
	// (inside the 20-message window) must not push the preamble over
	// the reminder budget, where the composer sheds it whole. Newest
	// bot posts are kept, the older overflow collapses to a count, and
	// every human line survives.
	long := strings.Repeat("report line. ", 100) // ~1.3 KB each
	var replies []slackThreadMessage
	for i := 0; i < 12; i++ {
		replies = append(replies, citadelShape(fmt.Sprintf("1.%02d", i), fmt.Sprintf("peer post %d %s", i, long)))
	}
	replies = append(replies, slackThreadMessage{User: "U_AFIK", Text: "yes sling the fix bead", TS: "2.00"})
	got := formatThreadContextPreamble(replies, "3.00", "", nil, nil, tcSelfClassifier().classify)

	if len(got) > defaultReminderTextBudget-800 {
		t.Fatalf("preamble is %d bytes, must leave headroom under the %d budget:\n%s", len(got), defaultReminderTextBudget, got)
	}
	if !strings.Contains(got, "@U_AFIK: yes sling the fix bead") {
		t.Fatalf("human line must survive the bot allowance:\n%s", got)
	}
	// 1400 / (280 + ellipsis) → the newest 4 peer posts fit; 8 omitted.
	for _, want := range []string{"peer post 11 ", "peer post 10 ", "peer post 9 ", "peer post 8 "} {
		if !strings.Contains(got, want) {
			t.Fatalf("newest peer post %q must be kept:\n%s", want, got)
		}
	}
	if strings.Contains(got, "peer post 7 ") || strings.Contains(got, "peer post 0 ") {
		t.Fatalf("older peer posts must be omitted once the allowance is spent:\n%s", got)
	}
	if !strings.Contains(got, "Thread context (5 earlier messages; 8 older peer-bot posts omitted for budget):") {
		t.Fatalf("header must count kept lines and omitted bot posts:\n%s", got)
	}
	// Composed through the real channel composer the preamble is used,
	// not replaced by the omission notice.
	text, _, _, usedPreamble, _ := composeChannelReminderText(channelReminderParts{preamble: got, body: strings.Repeat("b", 200)}, "", "", defaultReminderTextBudget)
	if !usedPreamble || strings.Contains(text, preambleOmittedNotice) {
		t.Fatalf("composer must keep the bounded preamble under the default budget (usedPreamble=%v)", usedPreamble)
	}

	// A thread of only omitted bot posts still renders the count.
	onlyBots := replies[:12]
	got = formatThreadContextPreamble(onlyBots, "3.00", "", nil, nil, tcSelfClassifier().classify)
	if !strings.Contains(got, "; 8 older peer-bot posts omitted for budget):") || !strings.Contains(got, "peer post 11 ") {
		t.Fatalf("bot-only thread must keep its newest posts and count the rest:\n%s", got)
	}
}

func TestClipBotPost(t *testing.T) {
	if got := clipBotPost("abc", 5); got != "abc" {
		t.Fatalf("short string must pass through, got %q", got)
	}
	if got := clipBotPost("abcdef", 0); got != "abcdef" {
		t.Fatalf("zero cap disables clipping, got %q", got)
	}
	// 7 bytes lands inside the second CJK rune (3 bytes each): the cut
	// backs up to the rune boundary, then trims the separator residue.
	if got := clipBotPost("司南 | mayor", 7); got != "司南…" {
		t.Fatalf("rune-safe clip + trailing separator trim, got %q", got)
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
	got := formatThreadContextPreamble(replies, "3.0", "", nil,
		func(ts string) bool { return ts == "1.0" }, tcSelfClassifier().classify)
	if !strings.Contains(got, "1 earlier message already delivered (newest ts 1.0) — not re-quoted.") {
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
	if strings.Contains(text, "reflected reply") {
		t.Fatalf("own post must not be re-quoted; got %q", text)
	}
	if strings.Index(text, "Citadel Mayor (bot)") > strings.Index(text, "yes sling the fix bead") {
		t.Fatalf("peer reply must precede the human's answer to it; got %q", text)
	}
}

func TestComposeChannelReminder_ShedsBotQuotesBeforePreamble(t *testing.T) {
	// codex r3 P2: a large unseen human prior plus bot quotes overflows
	// the budget; the composer must fall back to the lean preamble (the
	// pre-fix rendering) under a notice, not drop the human context.
	humanLine := "@alice: " + strings.Repeat("h", 2200) + "\n"
	botLines := ""
	for i := 0; i < 4; i++ {
		botLines += "@Citadel Mayor (bot): " + strings.Repeat("r", threadContextBotPostBytes) + "…\n"
	}
	full := "Thread context (5 earlier messages):\n" + humanLine + botLines + "\n---\n\n"
	lean := "Thread context (1 earlier message):\n" + humanLine + "\n---\n\n"
	parts := channelReminderParts{
		anchor:       "[thread reply — reply with --thread-ts 99.1]",
		preamble:     full,
		preambleLean: lean,
		botQuotes:    true,
		body:         strings.Repeat("b", 200),
		ts:           "100.000001",
		channelID:    "C1",
	}
	got, _, _, usedPreamble, trimmed := composeChannelReminderText(parts, "", "", defaultReminderTextBudget)
	if !usedPreamble || trimmed {
		t.Fatalf("usedPreamble=%v trimmed=%v, want true/false", usedPreamble, trimmed)
	}
	if !strings.HasPrefix(got, parts.anchor+"\n"+botQuotesOmittedNotice+lean+parts.body) {
		t.Fatalf("expected anchor + bot-quotes notice + lean preamble + body, got %q", got)
	}
	if strings.Contains(got, "(bot)") || strings.Contains(got, preambleOmittedNotice) || len(got) > defaultReminderTextBudget {
		t.Fatalf("bot quotes must be shed, human context kept, within budget (len=%d): %q", len(got), got)
	}

	// Lean unit within notice-length of the budget (codex r4 P2): the
	// notice yields and the bare lean preamble delivers as before.
	tight := parts
	tight.preambleLean = "Thread context (1 earlier message):\n@alice: " + strings.Repeat("h", defaultReminderTextBudget-len(parts.anchor)-1-len("Thread context (1 earlier message):\n@alice: ")-len("\n\n---\n\n")-len(parts.body)-10) + "\n\n---\n\n"
	tight.preamble = tight.preambleLean + "@Citadel Mayor (bot): x\n"
	bare := tight.anchor + "\n" + tight.preambleLean + tight.body
	if len(bare) > defaultReminderTextBudget || len(bare)+len(botQuotesOmittedNotice) <= defaultReminderTextBudget {
		t.Fatalf("fixture must fit bare (%d) but not with the notice (%d)", len(bare), len(bare)+len(botQuotesOmittedNotice))
	}
	got, _, _, usedPreamble, _ = composeChannelReminderText(tight, "", "", defaultReminderTextBudget)
	if !usedPreamble || got != bare {
		t.Fatalf("notice must yield before the human context: usedPreamble=%v got %q", usedPreamble, got)
	}

	// The lean form itself over budget: the pre-existing omission
	// notice path is unchanged.
	parts.preambleLean = "Thread context (1 earlier message):\n@alice: " + strings.Repeat("h", 5000) + "\n\n---\n\n"
	parts.preamble = parts.preambleLean + "@Citadel Mayor (bot): x\n"
	got, _, _, usedPreamble, _ = composeChannelReminderText(parts, "", "", defaultReminderTextBudget)
	if usedPreamble || !strings.HasPrefix(got, parts.anchor+"\n"+preambleOmittedNotice+parts.body) {
		t.Fatalf("lean preamble over budget must still fall to the omission notice, got usedPreamble=%v %q", usedPreamble, got)
	}

	// No bot quotes (flag unset): the fallback step is a no-op and the
	// omission path behaves exactly as before.
	parts.botQuotes = false
	parts.preambleLean = ""
	got2, _, _, usedPreamble2, _ := composeChannelReminderText(parts, "", "", defaultReminderTextBudget)
	if usedPreamble2 || got2 != got {
		t.Fatalf("no-bot-quotes parts must be byte-identical to the legacy omission path")
	}

	// Bot-only delta (codex r5 P2): the lean rendering is legitimately
	// EMPTY. With anchor + body within the budget bare, the bot quotes
	// and then the notice yield and the body arrives intact, untrimmed —
	// exactly the pre-fix delivery.
	botOnly := channelReminderParts{
		anchor:       "[thread reply — reply with --thread-ts 99.1]",
		preamble:     "Thread context (1 earlier message):\n@Citadel Mayor (bot): report\n\n---\n\n",
		preambleLean: "",
		botQuotes:    true,
		body:         strings.Repeat("b", defaultReminderTextBudget-len("[thread reply — reply with --thread-ts 99.1]")-1-10),
		ts:           "100.000001",
		channelID:    "C1",
	}
	got3, _, _, usedPreamble3, trimmed3 := composeChannelReminderText(botOnly, "", "", defaultReminderTextBudget)
	if trimmed3 || !usedPreamble3 || got3 != botOnly.anchor+"\n"+botOnly.body {
		t.Fatalf("bot-only preamble must yield to an intact body: trimmed=%v usedPreamble=%v got %q", trimmed3, usedPreamble3, got3)
	}
}

func TestThreadContext_BotQuotesShedBeforeHumanContext(t *testing.T) {
	// End to end (codex r3 scenario): an unseen 2200-byte human prior,
	// four long peer reports, a 200-byte current body, default budget.
	// Before the shed step the composer dropped the whole preamble; now
	// the human prior survives and only the bot quotes are shed.
	currentTS := "100.000009"
	humanPrior := strings.Repeat("h", 2200)
	prior := []slackThreadMessage{
		{User: "U_AFIK", Text: humanPrior, TS: "100.000001"},
	}
	for i := 0; i < 4; i++ {
		prior = append(prior, citadelShape(fmt.Sprintf("100.00000%d", i+2), strings.Repeat("report ", 200)))
	}
	prior = append(prior, slackThreadMessage{User: "U_AFIK", Text: "@mayor " + strings.Repeat("b", 193), TS: currentTS})
	slackStub, _ := fakeSlackRepliesServer(t, prior)
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
		reminderTextBudget:      defaultReminderTextBudget,
	}
	rawMsg, _ := json.Marshal(slackMessageEvent{
		Type:     "message",
		Channel:  "C1",
		User:     "U_AFIK",
		TS:       currentTS,
		ThreadTS: "100.000001",
		Text:     "@mayor " + strings.Repeat("b", 193),
	})
	env := slackEventEnvelope{
		Type:           "event_callback",
		Event:          rawMsg,
		APIAppID:       tcSelfAppID,
		Authorizations: []slackEventAuthorization{{UserID: tcSelfBotUserID, IsBot: true}},
	}

	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})

	msgs := capture.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("captured %d inbound messages, want 1", len(msgs))
	}
	text := msgs[0].Text
	if strings.Contains(text, preambleOmittedNotice) {
		t.Fatalf("whole preamble must not be dropped while a lean form fits: %q", text)
	}
	if !strings.Contains(text, botQuotesOmittedNotice) || !strings.Contains(text, "@U_AFIK: "+humanPrior) {
		t.Fatalf("expected bot-quotes notice + intact human prior; got %q", text)
	}
	if strings.Contains(text, "(bot)") {
		t.Fatalf("bot quotes must be shed in this scenario; got %q", text)
	}
	if len(text) > defaultReminderTextBudget {
		t.Fatalf("delivery is %d bytes, over the %d budget", len(text), defaultReminderTextBudget)
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
