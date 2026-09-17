package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Round-5 repro for citadel gate r3's MAJOR (main.go:4058): an imported
// company room's events are consumed by the company gateway, so a
// mention-only-bound session OUTSIDE company membership used to receive
// neither @mentions nor own-thread follow-ups. These tests drive
// tryHandleEvent — the exact seam the gate named — with the company
// harness plus a mention-only binding for an outsider session.

const companyMOBotUserID = "U0SWITCHBOT"

// withMentionOnlyLane arms the harness gateway's cfg copy with the
// mention-only registries the live adapter constructs before the gateway.
func withMentionOnlyLane(t *testing.T, h *companyHarness, bindings ...mentionOnlyBinding) *mentionOnlyRegistry {
	t.Helper()
	reg, err := newMentionOnlyRegistry(filepath.Join(t.TempDir(), "mention_only.json"))
	if err != nil {
		t.Fatalf("newMentionOnlyRegistry: %v", err)
	}
	for _, b := range bindings {
		if err := reg.Set(testChannelID, b); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	own, err := newOwnThreadRegistry(filepath.Join(t.TempDir(), "own_threads.json"))
	if err != nil {
		t.Fatalf("newOwnThreadRegistry: %v", err)
	}
	h.gw.cfg.mentionOnly = reg
	h.gw.cfg.ownThreads = own
	h.gw.cfg.mentionOnlyDeliveries = newMentionOnlyDeliveryLog()
	h.gw.cfg.channelClaims = newEventDedupCache(time.Minute)
	h.gw.cfg.handlePrefix = "@"
	return reg
}

// admitCompanyRoomMessage drives tryHandleEvent with an envelope whose
// authorizations carry the adapter's bot user (as Slack's do), so a
// `<@bot>` in the text reads as a bot mention.
func admitCompanyRoomMessage(t *testing.T, h *companyHarness, ev slackMessageEvent) *httptest.ResponseRecorder {
	t.Helper()
	env := companyEnvelope(t, ev)
	env.Authorizations = []slackEventAuthorization{{UserID: companyMOBotUserID, IsBot: true}}
	req := httptest.NewRequest(http.MethodPost, "/slack/events", nil)
	w := httptest.NewRecorder()
	if handled := h.gw.tryHandleEvent(w, req, env, h.gw.agentAppsSnapshot()); !handled {
		t.Fatalf("company gateway must own the event for an imported room")
	}
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("admit status = %d, want 200", w.Result().StatusCode)
	}
	return w
}

func outsiderBinding() mentionOnlyBinding {
	return mentionOnlyBinding{SessionID: "outsider-session", SessionName: "outsider", Handle: "outsider"}
}

// mentionOnlyInjections splits the fake gc's session calls into the
// mention-only reminders (by session) and everything else.
func mentionOnlyInjections(calls []gcDelivery) (lane map[string][]string, other []gcDelivery) {
	lane = make(map[string][]string)
	for _, c := range calls {
		if strings.Contains(c.body, "Slack mention-only room delivery") {
			parts := strings.Split(strings.Trim(c.path, "/"), "/")
			sid := ""
			for i, p := range parts {
				if p == "session" && i+1 < len(parts) {
					sid = parts[i+1]
				}
			}
			lane[sid] = append(lane[sid], c.body)
			continue
		}
		other = append(other, c)
	}
	return lane, other
}

// (1) The gate's scenario: a bot @mention in an imported company room
// reaches the mention-only outsider, and the company members' own
// delivery is unchanged.
func TestCompanyRoomMentionOnlyOutsiderReceivesBotMention(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())

	ev := humanMessage("1700000000.000500", "<@"+companyMOBotUserID+"> can you look at this?")
	admitCompanyRoomMessage(t, h, ev)
	h.wait()

	lane, other := mentionOnlyInjections(gc.sessionCalls())
	got := lane["outsider-session"]
	if len(got) != 1 {
		t.Fatalf("mention-only injections to outsider = %d, want 1 (company gateway consumed the event; the lane must still run)", len(got))
	}
	if !strings.Contains(got[0], "you were @mentioned") || !strings.Contains(got[0], "Slack ts 1700000000.000500") {
		t.Errorf("reminder body wrong:\n%s", got[0])
	}
	if len(lane) != 1 {
		t.Errorf("mention-only reminders reached sessions %v, want only the outsider", lane)
	}
	// Company members still get exactly their gateway delivery (ollie is
	// the ambient-wake member of the fixture room).
	if len(other) != 1 || !strings.Contains(other[0].path, "/session/ollie-main/") {
		t.Errorf("company deliveries = %+v, want exactly ollie-main's", other)
	}
	log := h.gw.cfg.mentionOnlyDeliveries.forSession("outsider-session")
	if len(log) != 1 || log[0].ChannelID != testChannelID || log[0].TS != "1700000000.000500" || log[0].Reason != mentionOnlyReasonBotMention {
		t.Errorf("delivery log = %+v, want one bot_mention record for the reply tooling", log)
	}
}

// (2) The gate's second clause: a follow-up in a thread the outsider
// posted in (own-thread registry) reaches it, with no mention at all.
func TestCompanyRoomMentionOnlyOutsiderReceivesOwnThreadFollowUp(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())
	h.gw.cfg.ownThreads.record(testChannelID, "1700000000.000600", "outsider-session")

	ev := humanMessage("1700000000.000601", "thanks — one more question")
	ev.ThreadTS = "1700000000.000600"
	admitCompanyRoomMessage(t, h, ev)
	h.wait()

	lane, _ := mentionOnlyInjections(gc.sessionCalls())
	got := lane["outsider-session"]
	if len(got) != 1 {
		t.Fatalf("own-thread follow-up injections = %d, want 1", len(got))
	}
	if !strings.Contains(got[0], "a reply landed in a thread you posted in") || !strings.Contains(got[0], "--thread-ts 1700000000.000600") {
		t.Errorf("reminder must carry the own-thread reason and anchor at the root:\n%s", got[0])
	}
}

// (3) Everything else in the room stays silent for the outsider: a plain
// human message (no mention, no own thread) produces no injection, while
// the company delivery proceeds exactly as before.
func TestCompanyRoomMentionOnlyOutsiderStaysSilentOnPlainChatter(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())

	admitCompanyRoomMessage(t, h, humanMessage("1700000000.000700", "hello team, status?"))
	h.wait()

	lane, other := mentionOnlyInjections(gc.sessionCalls())
	if len(lane) != 0 {
		t.Errorf("plain chatter must not reach a mention-only session, got %v", lane)
	}
	if len(other) != 1 || !strings.Contains(other[0].path, "/session/ollie-main/") {
		t.Errorf("company deliveries = %+v, want exactly ollie-main's", other)
	}
}

// (4) No double delivery: a mention-only binding whose session IS a
// company member of the room is covered by the gateway's delivery — the
// lane must not inject a second copy.
func TestCompanyRoomMentionOnlyCompanyMemberNotDoubleDelivered(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h,
		outsiderBinding(),
		mentionOnlyBinding{SessionID: "ollie-main", Handle: "ollie"},
	)

	admitCompanyRoomMessage(t, h, humanMessage("1700000000.000800", "<@"+companyMOBotUserID+"> ping"))
	h.wait()

	lane, other := mentionOnlyInjections(gc.sessionCalls())
	if len(lane["outsider-session"]) != 1 {
		t.Errorf("outsider injections = %d, want 1", len(lane["outsider-session"]))
	}
	if n := len(lane["ollie-main"]); n != 0 {
		t.Errorf("company member ollie-main got %d mention-only copies on top of its gateway delivery, want 0", n)
	}
	ollie := 0
	for _, c := range other {
		if strings.Contains(c.path, "/session/ollie-main/") {
			ollie++
		}
	}
	if ollie != 1 {
		t.Errorf("ollie-main gateway deliveries = %d, want exactly 1", ollie)
	}
}

// (5) A Slack redelivery of the same event (duplicate origin) and the
// app_mention twin both enter the lane but inject nothing new (the
// delivered claim is committed); a bot-authored post in the room (the
// outsider's own threaded reply) never wakes it on itself.
func TestCompanyRoomMentionOnlyRunsOncePerOriginAndIgnoresBotPosts(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())
	h.gw.cfg.ownThreads.record(testChannelID, "1700000000.000900", "outsider-session")

	ev := humanMessage("1700000000.000901", "<@"+companyMOBotUserID+"> again")
	ev.ThreadTS = "1700000000.000900"
	admitCompanyRoomMessage(t, h, ev)
	admitCompanyRoomMessage(t, h, ev) // x-slack-retry twin: duplicate origin
	admitCompanyRoomAppMention(t, h, ev)
	// The outsider's own reply lands as a bot post in its thread.
	bot := slackMessageEvent{BotID: "B0OUTSIDER", Channel: testChannelID, TS: "1700000000.000902", ThreadTS: "1700000000.000900", Text: "on it"}
	admitCompanyRoomMessage(t, h, bot)
	h.wait()

	lane, _ := mentionOnlyInjections(gc.sessionCalls())
	if n := len(lane["outsider-session"]); n != 1 {
		t.Fatalf("outsider injections = %d, want exactly 1 (one per origin; bot posts ignored)", n)
	}
}

// (6) A company room with no mention-only bindings takes the path it
// always took: only the gateway's deliveries, nothing else.
func TestCompanyRoomWithoutMentionOnlyBindingsUnchanged(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h) // registries present, no bindings

	admitCompanyRoomMessage(t, h, humanMessage("1700000000.001000", "<@"+companyMOBotUserID+"> hello"))
	h.wait()

	lane, other := mentionOnlyInjections(gc.sessionCalls())
	if len(lane) != 0 || len(other) != 1 {
		t.Errorf("deliveries = lane %v other %+v, want no lane injections and exactly one gateway delivery", lane, other)
	}
}

// admitCompanyRoomAppMention drives the app_mention twin of ev through
// tryHandleEvent (the gateway acks it with no receipt).
func admitCompanyRoomAppMention(t *testing.T, h *companyHarness, ev slackMessageEvent) {
	t.Helper()
	ev.Type = "app_mention"
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal app_mention: %v", err)
	}
	env := slackEventEnvelope{
		Type: "event_callback", TeamID: testTeamID, APIAppID: "A0SWITCH", EventID: "EvAM-" + ev.TS, Event: raw,
		Authorizations: []slackEventAuthorization{{UserID: companyMOBotUserID, IsBot: true}},
	}
	w := httptest.NewRecorder()
	if handled := h.gw.tryHandleEvent(w, httptest.NewRequest(http.MethodPost, "/slack/events", nil), env, h.gw.agentAppsSnapshot()); !handled {
		t.Fatalf("company gateway must own the app_mention twin for an imported room")
	}
}

// (7) codex r5 P1: an injection gc rejects is retried in place (bounded
// backoff) and, independently, a later copy of the origin — the
// app_mention twin or a Slack redelivery — retakes the released claim. The
// receipt is admitted and acked on first arrival, so without these the
// outsider's message was lost for good.
func TestCompanyRoomMentionOnlyFailedInjectionRetried(t *testing.T) {
	prev := companyMentionOnlyRetryBackoff
	companyMentionOnlyRetryBackoff = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { companyMentionOnlyRetryBackoff = prev })

	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())
	// gc rejects the outsider's FIRST injection only.
	rejected := 0
	gc.respStatus = func(reqNum int) int {
		gc.mu.Lock()
		defer gc.mu.Unlock()
		if strings.Contains(gc.calls[reqNum].path, "/session/outsider-session/") && rejected == 0 {
			rejected++
			return http.StatusNotFound
		}
		return 0
	}

	admitCompanyRoomMessage(t, h, humanMessage("1700000000.001100", "<@"+companyMOBotUserID+"> retry me"))
	h.wait()

	outsiderPosts := 0
	for _, c := range gc.sessionCalls() {
		if strings.Contains(c.path, "/session/outsider-session/") {
			outsiderPosts++
		}
	}
	if rejected != 1 || outsiderPosts != 2 {
		t.Fatalf("outsider posts = %d (rejected %d), want 2: one rejected, one retried and accepted", outsiderPosts, rejected)
	}
	if got := h.gw.cfg.mentionOnlyDeliveries.forSession("outsider-session"); len(got) != 1 {
		t.Errorf("delivery log = %+v, want exactly the accepted injection", got)
	}

	// Exhausted retries: every attempt fails; the twin then retakes the
	// released claim and delivers.
	companyMentionOnlyRetryBackoff = []time.Duration{time.Millisecond}
	gc.respStatus = func(reqNum int) int {
		gc.mu.Lock()
		defer gc.mu.Unlock()
		if strings.Contains(gc.calls[reqNum].path, "/session/outsider-session/") && strings.Contains(gc.calls[reqNum].body, "Slack ts 1700000000.001200") && rejected < 3 {
			rejected++
			return http.StatusNotFound
		}
		return 0
	}
	ev := humanMessage("1700000000.001200", "<@"+companyMOBotUserID+"> and again")
	admitCompanyRoomMessage(t, h, ev)
	h.wait()
	if rejected != 3 {
		t.Fatalf("rejected = %d, want 3 (first attempt + 1 in-place retry + ...)", rejected)
	}
	admitCompanyRoomAppMention(t, h, ev)
	h.wait()
	log := h.gw.cfg.mentionOnlyDeliveries.forSession("outsider-session")
	found := false
	for _, d := range log {
		if d.TS == "1700000000.001200" {
			found = true
		}
	}
	if !found {
		t.Errorf("twin must retake the released claim and deliver ts .001200; log = %+v", log)
	}
}

// (8) codex r5 P2: a company binding that targets ANOTHER city names a
// session in that city's namespace. A local mention-only binding with the
// same session name is a different session and must still be delivered.
func TestCompanyRoomMentionOnlyRemoteCityMemberDoesNotShadowLocalSession(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	bf.Bindings[0] = CompanyBinding{Room: "orchestrator-team", Agent: "ollie", Session: "mayor", City: "other-city"}
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, mentionOnlyBinding{SessionID: "jg-mayor-local", SessionName: "mayor", Handle: "mayor"})

	admitCompanyRoomMessage(t, h, humanMessage("1700000000.001300", "<@"+companyMOBotUserID+"> local mayor?"))
	h.wait()

	lane, _ := mentionOnlyInjections(gc.sessionCalls())
	if len(lane["jg-mayor-local"]) != 1 {
		t.Fatalf("local mention-only session shadowed by a remote-city company binding of the same name: injections = %v", lane)
	}
}

// --- round 6 (citadel Fable read r1 MINORs) -----------------------------------

func outsiderPostCount(gc *fakeGC) int {
	n := 0
	for _, c := range gc.sessionCalls() {
		if strings.Contains(c.path, "/session/outsider-session/") {
			n++
		}
	}
	return n
}

// (9) MINOR 2: the lane's claim is in memory (10 minutes) while the receipt
// store is durable. A Slack redelivery of an already-delivered origin that
// arrives after a restart must not wake the outsider a second time: the
// receipt records the sessions the lane reached.
func TestCompanyRoomMentionOnlyRedeliveryAfterRestartNotDeliveredTwice(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())

	ev := humanMessage("1700000000.001400", "<@"+companyMOBotUserID+"> once only")
	admitCompanyRoomMessage(t, h, ev)
	h.wait()
	if got := outsiderPostCount(gc); got != 1 {
		t.Fatalf("first admission: outsider posts = %d, want 1", got)
	}
	r, err := h.gw.store().Get(ReceiptOrigin{TeamID: testTeamID, ChannelID: testChannelID, TS: ev.TS})
	if err != nil || r == nil {
		t.Fatalf("receipt: %v %v", r, err)
	}
	if len(r.MentionOnlyDelivered) != 1 || r.MentionOnlyDelivered[0] != "outsider-session" {
		t.Fatalf("receipt.MentionOnlyDelivered = %v, want [outsider-session]", r.MentionOnlyDelivered)
	}

	// "Restart": every in-memory guard is gone; the receipt store stays.
	h.gw.cfg.channelClaims = newEventDedupCache(time.Minute)
	h.gw.cfg.mentionOnlyDeliveries = newMentionOnlyDeliveryLog()
	admitCompanyRoomMessage(t, h, ev) // Slack's +1-minute redelivery: duplicate origin
	h.wait()
	admitCompanyRoomAppMention(t, h, ev)
	h.wait()
	if got := outsiderPostCount(gc); got != 1 {
		t.Fatalf("after restart + redelivery + twin: outsider posts = %d, want still 1", got)
	}
}

// codex r7 P2: the durable marker is written under the identifier the
// binding had at delivery time. A binding re-made under another identifier
// (name -> gc id, the old one kept as an alias) must still find it.
func TestCompanyRoomMentionOnlyDurableMarkerSurvivesRebindUnderAnotherIdentifier(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	reg := withMentionOnlyLane(t, h, outsiderBinding())

	ev := humanMessage("1700000000.001450", "<@"+companyMOBotUserID+"> once only")
	admitCompanyRoomMessage(t, h, ev)
	h.wait()
	if got := len(gc.sessionCalls()); got == 0 {
		t.Fatalf("first admission delivered nothing")
	}
	before := len(gc.sessionCalls())

	if err := reg.Set(testChannelID, mentionOnlyBinding{SessionID: "jg-outsider-9", Handle: "outsider", Aliases: []string{"outsider-session"}}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	h.gw.cfg.channelClaims = newEventDedupCache(time.Minute) // restart / claim TTL
	admitCompanyRoomMessage(t, h, ev)                        // Slack redelivery
	h.wait()
	if after := len(gc.sessionCalls()); after != before {
		t.Fatalf("session calls after rebind + redelivery = %d, want %d (the receipt already proves delivery)", after, before)
	}
}

// (10) MINOR 3: the ⚠️ posted on the first failed attempt is removed when
// the in-place retry reaches the session — it used to stay forever as a
// false "session not reached" marker.
func TestCompanyRoomMentionOnlyWarningClearedWhenRetrySucceeds(t *testing.T) {
	prev := companyMentionOnlyRetryBackoff
	companyMentionOnlyRetryBackoff = []time.Duration{10 * time.Millisecond}
	t.Cleanup(func() { companyMentionOnlyRetryBackoff = prev })

	var mu sync.Mutex
	var reactions []string
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/reactions.") {
			var req slackReactionsAddReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			reactions = append(reactions, strings.TrimPrefix(r.URL.Path, "/")+":"+req.Name+":"+req.Timestamp)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(slack.Close)
	prevBase := slackAPIBase
	slackAPIBase = slack.URL
	t.Cleanup(func() { slackAPIBase = prevBase })

	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())
	h.gw.cfg.slackBotToken = "xoxb-test"
	rejected := 0
	gc.respStatus = func(reqNum int) int {
		gc.mu.Lock()
		defer gc.mu.Unlock()
		if strings.Contains(gc.calls[reqNum].path, "/session/outsider-session/") && rejected == 0 {
			rejected++
			return http.StatusNotFound
		}
		return 0
	}

	const ts = "1700000000.001500"
	admitCompanyRoomMessage(t, h, humanMessage(ts, "<@"+companyMOBotUserID+"> flaky"))
	h.wait()

	mu.Lock()
	defer mu.Unlock()
	var warn []string
	for _, r := range reactions {
		if strings.Contains(r, ":warning:") {
			warn = append(warn, r)
		}
	}
	want := []string{"reactions.add:warning:" + ts, "reactions.remove:warning:" + ts}
	if len(warn) != 2 || warn[0] != want[0] || warn[1] != want[1] {
		t.Fatalf("warning reactions = %v, want %v (marker cleared once the retry delivered)", warn, want)
	}
}

// (11) MINOR 4: with SLACK_APP_ID configured, a PERSONA app's copy of the
// event contributes neither its app_mention type nor its own bot user id —
// only a mention of the switchboard's bot wakes a mention-only session.
func TestCompanyRoomMentionOnlyBotMentionPinnedOnSwitchboardApp(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())
	h.gw.cfg.slackAppID = "A0SWITCH"

	// A persona app that also subscribes app_mention delivers its twin for
	// a message mentioning the PERSONA, not the switchboard.
	ev := humanMessage("1700000000.001600", "<@U0PERSONABOT> not for the outsider")
	ev.Type = "app_mention"
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	env := slackEventEnvelope{
		Type: "event_callback", TeamID: testTeamID, APIAppID: "A0PERSONA", EventID: "EvP-" + ev.TS, Event: raw,
		Authorizations: []slackEventAuthorization{{UserID: "U0PERSONABOT", IsBot: true}},
	}
	w := httptest.NewRecorder()
	if !h.gw.tryHandleEvent(w, httptest.NewRequest(http.MethodPost, "/slack/events", nil), env, h.gw.agentAppsSnapshot()) {
		t.Fatalf("company gateway must own the twin")
	}
	h.wait()
	if got := outsiderPostCount(gc); got != 0 {
		t.Fatalf("persona app's app_mention woke the mention-only session: posts = %d, want 0", got)
	}

	// The switchboard's own copy of a real switchboard mention still does.
	admitCompanyRoomMessage(t, h, humanMessage("1700000000.001700", "<@"+companyMOBotUserID+"> for the outsider"))
	h.wait()
	if got := outsiderPostCount(gc); got != 1 {
		t.Fatalf("switchboard mention: outsider posts = %d, want 1", got)
	}
}

// (12) MINOR 1: the gateway's cfg copy carries the drain flag, so the retry
// ladder stops at shutdown and mentionOnlyWG — what shutdown joins — drains
// without waiting out a backoff step.
func TestCompanyRoomMentionOnlyRetryStopsWhenDraining(t *testing.T) {
	prev := companyMentionOnlyRetryBackoff
	companyMentionOnlyRetryBackoff = []time.Duration{30 * time.Second}
	t.Cleanup(func() { companyMentionOnlyRetryBackoff = prev })

	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())
	h.gw.cfg.draining = &atomic.Bool{}
	gc.respStatus = func(reqNum int) int {
		gc.mu.Lock()
		defer gc.mu.Unlock()
		if strings.Contains(gc.calls[reqNum].path, "/session/outsider-session/") {
			return http.StatusNotFound
		}
		return 0
	}

	admitCompanyRoomMessage(t, h, humanMessage("1700000000.001800", "<@"+companyMOBotUserID+"> shutting down"))
	time.Sleep(50 * time.Millisecond) // the first attempt fails; the lane is in its backoff
	h.gw.cfg.draining.Store(true)
	if !awaitWaitGroup(&h.gw.mentionOnlyWG, 3*time.Second) {
		t.Fatalf("mention-only lane still running 3s after draining was set (backoff step is 30s)")
	}
	if got := outsiderPostCount(gc); got != 1 {
		t.Errorf("outsider posts = %d, want 1 (no retry once draining)", got)
	}

	// codex r6 P1: the event was acked at admission and nothing upstream
	// retries it, so the cut-off injection is left on the receipt and the
	// next startup replays it.
	origin := ReceiptOrigin{TeamID: testTeamID, ChannelID: testChannelID, TS: "1700000000.001800"}
	r, err := h.gw.store().Get(origin)
	if err != nil || r == nil {
		t.Fatalf("receipt: %v %v", r, err)
	}
	if len(r.MentionOnlyPending) != 1 || r.MentionOnlyPending[0].SessionID != "outsider-session" ||
		r.MentionOnlyPending[0].Reason != mentionOnlyReasonBotMention || len(r.MentionOnlyDelivered) != 0 {
		t.Fatalf("receipt pending=%+v delivered=%v, want the outsider's bot_mention pending and nothing delivered", r.MentionOnlyPending, r.MentionOnlyDelivered)
	}

	// "Restart": fresh in-memory state, gc healthy again, not draining.
	h.gw.cfg.draining = &atomic.Bool{}
	h.gw.cfg.channelClaims = newEventDedupCache(time.Minute)
	h.gw.cfg.mentionOnlyDeliveries = newMentionOnlyDeliveryLog()
	gc.respStatus = nil
	h.gw.replayMentionOnlyPending()
	h.wait()
	if got := outsiderPostCount(gc); got != 2 {
		t.Fatalf("after replay: outsider posts = %d, want 2 (the failed one + the replayed one)", got)
	}
	r, _ = h.gw.store().Get(origin)
	if len(r.MentionOnlyPending) != 0 || len(r.MentionOnlyDelivered) != 1 {
		t.Fatalf("after replay: pending=%+v delivered=%v, want none pending and the outsider delivered", r.MentionOnlyPending, r.MentionOnlyDelivered)
	}
	// A second replay (or a Slack redelivery) finds nothing to do.
	h.gw.replayMentionOnlyPending()
	admitCompanyRoomMessage(t, h, humanMessage("1700000000.001800", "<@"+companyMOBotUserID+"> shutting down"))
	h.wait()
	if got := outsiderPostCount(gc); got != 2 {
		t.Fatalf("replay must be once only: outsider posts = %d, want 2", got)
	}
}

// (13) codex r6 re-gate P1: gc's 202 is an ASYNCHRONOUS acceptance, not a
// delivery. The lane awaits the terminal result on the event stream; a
// request.failed reads as a failed injection (retried, never recorded as
// delivered), a success as delivered.
func TestCompanyRoomMentionOnlyAsyncAcceptanceAwaitsTerminalResult(t *testing.T) {
	prev := companyMentionOnlyRetryBackoff
	companyMentionOnlyRetryBackoff = []time.Duration{10 * time.Millisecond}
	t.Cleanup(func() { companyMentionOnlyRetryBackoff = prev })

	var mu sync.Mutex
	outsiderPosts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/session/outsider-session/"):
			mu.Lock()
			outsiderPosts++
			n := outsiderPosts
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprintf(w, `{"status":"accepted","request_id":"req-%d","event_cursor":"%d"}`, n, 40+n)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if r.URL.Query().Get("after_seq") == "41" {
				_, _ = fmt.Fprint(w, "event: event\nid: 42\ndata: {\"type\":\"request.failed\",\"payload\":{\"request_id\":\"req-1\",\"operation\":\"session.message\",\"error_code\":\"message_failed\",\"error_message\":\"queued=false\"}}\n\n")
				return
			}
			_, _ = fmt.Fprint(w, "event: event\nid: 43\ndata: {\"type\":\"request.result.session.message\",\"payload\":{\"request_id\":\"req-2\"}}\n\n")
		case r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, srv.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())
	h.gw.cfg.gcAPIBase = srv.URL

	const ts = "1700000000.001900"
	admitCompanyRoomMessage(t, h, humanMessage(ts, "<@"+companyMOBotUserID+"> async please"))
	h.wait()

	mu.Lock()
	posts := outsiderPosts
	mu.Unlock()
	if posts != 2 {
		t.Fatalf("outsider POSTs = %d, want 2 (the 202 whose request failed, then the retry)", posts)
	}
	r, err := h.gw.store().Get(ReceiptOrigin{TeamID: testTeamID, ChannelID: testChannelID, TS: ts})
	if err != nil || r == nil {
		t.Fatalf("receipt: %v %v", r, err)
	}
	if len(r.MentionOnlyDelivered) != 1 || len(r.MentionOnlyPending) != 0 {
		t.Fatalf("receipt delivered=%v pending=%+v, want the outsider delivered once the async result succeeded", r.MentionOnlyDelivered, r.MentionOnlyPending)
	}
}

// (14) codex r6 final-gate P2: the app_mention twin can deliver before the
// message copy creates the receipt, so its outcome has nowhere to go. The
// message copy then skips on the twin's COMMITTED claim — that is a settled
// delivery and is recorded as such, not left pending for a startup replay.
func TestCompanyRoomMentionOnlyTwinBeforeReceiptSettlesOnTheMessageCopy(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())

	ev := humanMessage("1700000000.002000", "<@"+companyMOBotUserID+"> twin first")
	admitCompanyRoomAppMention(t, h, ev)
	h.wait()
	admitCompanyRoomMessage(t, h, ev)
	h.wait()

	if got := outsiderPostCount(gc); got != 1 {
		t.Fatalf("outsider posts = %d, want 1", got)
	}
	r, err := h.gw.store().Get(ReceiptOrigin{TeamID: testTeamID, ChannelID: testChannelID, TS: ev.TS})
	if err != nil || r == nil {
		t.Fatalf("receipt: %v %v", r, err)
	}
	if len(r.MentionOnlyDelivered) != 1 || len(r.MentionOnlyPending) != 0 {
		t.Fatalf("receipt delivered=%v pending=%+v, want the twin's delivery settled and nothing pending", r.MentionOnlyDelivered, r.MentionOnlyPending)
	}
	h.gw.replayMentionOnlyPending()
	h.wait()
	if got := outsiderPostCount(gc); got != 1 {
		t.Fatalf("startup replay re-delivered a settled injection: posts = %d, want 1", got)
	}
}

// (15) codex r6 final-gate P2: an interrupted confirmation stream is not a
// failed request. The same request is re-confirmed; no second POST.
func TestCompanyRoomMentionOnlyAsyncConfirmationInterruptedIsNotReposted(t *testing.T) {
	prev := companyMentionOnlyRetryBackoff
	companyMentionOnlyRetryBackoff = []time.Duration{10 * time.Millisecond}
	t.Cleanup(func() { companyMentionOnlyRetryBackoff = prev })

	var mu sync.Mutex
	outsiderPosts, streams := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/session/outsider-session/"):
			mu.Lock()
			outsiderPosts++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprint(w, `{"status":"accepted","request_id":"req-1","event_cursor":"41"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/stream"):
			mu.Lock()
			streams++
			n := streams
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if n == 1 {
				return // stream closes before the request's result
			}
			_, _ = fmt.Fprint(w, "event: event\nid: 42\ndata: {\"type\":\"request.result.session.message\",\"payload\":{\"request_id\":\"req-1\"}}\n\n")
		case r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, srv.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())
	h.gw.cfg.gcAPIBase = srv.URL

	const ts = "1700000000.002100"
	admitCompanyRoomMessage(t, h, humanMessage(ts, "<@"+companyMOBotUserID+"> flaky stream"))
	h.wait()

	mu.Lock()
	posts, st := outsiderPosts, streams
	mu.Unlock()
	if posts != 1 || st != 2 {
		t.Fatalf("outsider POSTs = %d, stream opens = %d; want 1 POST re-confirmed over 2 streams", posts, st)
	}
	r, _ := h.gw.store().Get(ReceiptOrigin{TeamID: testTeamID, ChannelID: testChannelID, TS: ts})
	if r == nil || len(r.MentionOnlyDelivered) != 1 || len(r.MentionOnlyPending) != 0 {
		t.Fatalf("receipt = %+v, want delivered and nothing pending", r)
	}
}

// (16) codex r6 P2 (raised in three gates): a company member whose company
// binding names it by SESSION NAME while the mention-only binding holds
// (id, alias) must still be recognized as a member — the binding carries
// the session's further identifiers.
func TestCompanyRoomMentionOnlyCompanyMemberRecognizedByAlias(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, mentionOnlyBinding{
		SessionID: "jg-ollie-1", SessionName: "ollie", Handle: "ollie", Aliases: []string{"ollie-main"},
	})

	admitCompanyRoomMessage(t, h, humanMessage("1700000000.002200", "<@"+companyMOBotUserID+"> one copy only"))
	h.wait()

	lane, other := mentionOnlyInjections(gc.sessionCalls())
	if len(lane) != 0 {
		t.Fatalf("company member (bound as ollie-main) also got a mention-only injection: %v", lane)
	}
	if len(other) != 1 || !strings.Contains(other[0].path, "/session/ollie-main/") {
		t.Errorf("company deliveries = %+v, want exactly ollie-main's", other)
	}
}

func TestMentionOnlyRegistryUpsertMatchesAcrossAliases(t *testing.T) {
	reg, err := newMentionOnlyRegistry(filepath.Join(t.TempDir(), "mo.json"))
	if err != nil {
		t.Fatalf("newMentionOnlyRegistry: %v", err)
	}
	// First bound by its literal session name (gc did not list it yet)…
	if err := reg.Set("C1", mentionOnlyBinding{SessionID: "rig__mayor", SessionName: "rig__mayor", Handle: "mayor"}); err != nil {
		t.Fatal(err)
	}
	// …then resolved: (id, alias) + the session name as an alias.
	if err := reg.Set("C1", mentionOnlyBinding{SessionID: "jg-123", SessionName: "mayor", Handle: "mayor", Aliases: []string{"rig__mayor", " ", "jg-123"}}); err != nil {
		t.Fatal(err)
	}
	got := reg.ForChannel("C1")
	if len(got) != 1 || got[0].SessionID != "jg-123" || len(got[0].Aliases) != 1 || got[0].Aliases[0] != "rig__mayor" {
		t.Fatalf("bindings = %+v, want the one resolved record with alias rig__mayor", got)
	}
	if !got[0].matchesSession("rig__mayor") || got[0].matchesSession("other") {
		t.Fatalf("matchesSession must cover aliases only: %+v", got[0])
	}
	if existed, err := reg.Delete("C1", "rig__mayor"); err != nil || !existed {
		t.Fatalf("Delete by alias: existed=%v err=%v", existed, err)
	}
}

// (r7b) citadel Fable read r2 MINOR 1: the company delivery's finalize
// commit and its ack commit can BOTH land between the lane's receipt read
// and its write — two stale generations in a row. The lane's confirmed
// outcome must still be recorded (one retry was not enough: the pending
// marker stayed and the next restart re-injected a copy), and the merge
// must keep what the two competing commits wrote.
func TestCompanyRoomMentionOnlyOutcomeSurvivesTwoStaleGenerations(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)

	r := admitReceived(t, h, "1700000000.001900")
	origin := r.Origin
	target := mentionOnlyTarget{binding: outsiderBinding(), reason: mentionOnlyReasonBotMention}
	h.gw.recordMentionOnlyOutcome(origin, nil, []mentionOnlyTarget{target}, false)
	if got, _ := h.gw.store().Get(origin); got == nil || len(got.MentionOnlyPending) != 1 {
		t.Fatalf("seed: receipt=%+v, want the outsider pending before dispatch", got)
	}

	// The company delivery's two commits, each landing after the lane has
	// read the receipt and before its generation-checked write.
	competing := []func(cur *IngressReceipt){
		func(cur *IngressReceipt) { cur.Status = ingressStatusDelivered },
		func(cur *IngressReceipt) { cur.AckState = ackStateDone },
	}
	h.gw.beforeReceiptUpdate = func() {
		if len(competing) == 0 {
			return
		}
		apply := competing[0]
		competing = competing[1:]
		cur, err := h.receipts.Get(origin)
		if err != nil || cur == nil {
			t.Errorf("competing commit read: %v %v", cur, err)
			return
		}
		apply(cur)
		if err := h.receipts.Update(cur); err != nil {
			t.Errorf("competing commit: %v", err)
		}
	}
	t.Cleanup(func() { h.gw.beforeReceiptUpdate = nil })

	h.gw.recordMentionOnlyOutcome(origin, []mentionOnlyTarget{target}, nil, true)

	if len(competing) != 0 {
		t.Fatalf("competing commits left = %d, want both to have landed mid-write", len(competing))
	}
	got, err := h.gw.store().Get(origin)
	if err != nil || got == nil {
		t.Fatalf("receipt: %v %v", got, err)
	}
	if len(got.MentionOnlyPending) != 0 || len(got.MentionOnlyDelivered) != 1 || got.MentionOnlyDelivered[0] != "outsider-session" {
		t.Errorf("receipt pending=%+v delivered=%v, want none pending and the outsider delivered after two stale generations",
			got.MentionOnlyPending, got.MentionOnlyDelivered)
	}
	if got.Status != ingressStatusDelivered || got.AckState != ackStateDone {
		t.Errorf("receipt status=%q ack=%q, want the two competing commits kept (merge, not overwrite)", got.Status, got.AckState)
	}
}

// (r7b) the merge-on-stale loop is bounded: a receipt that is stale on
// every attempt gives up after commitReceiptStaleRetries retries with
// ErrStale instead of spinning.
func TestCommitReceiptStaleRetryIsBounded(t *testing.T) {
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)

	r := admitReceived(t, h, "1700000000.001910")
	writes := 0
	h.gw.beforeReceiptUpdate = func() {
		writes++
		cur, err := h.receipts.Get(r.Origin)
		if err != nil || cur == nil {
			t.Errorf("competing commit read: %v %v", cur, err)
			return
		}
		if err := h.receipts.Update(cur); err != nil {
			t.Errorf("competing commit: %v", err)
		}
	}
	t.Cleanup(func() { h.gw.beforeReceiptUpdate = nil })

	err := h.gw.commitReceipt(r, func(cur *IngressReceipt) { cur.Reason = "never lands" })
	if !errors.Is(err, ErrStale) {
		t.Fatalf("commitReceipt err = %v, want ErrStale once the retries are spent", err)
	}
	if want := 1 + commitReceiptStaleRetries; writes != want {
		t.Errorf("write attempts = %d, want %d (one try + %d retries)", writes, want, commitReceiptStaleRetries)
	}
}
