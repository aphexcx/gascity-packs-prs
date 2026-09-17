package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
