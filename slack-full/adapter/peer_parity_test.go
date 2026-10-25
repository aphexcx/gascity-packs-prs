package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPeerArrivalUsesHumanReminderShape(t *testing.T) {
	for _, mode := range []string{"full", "minimal"} {
		for _, thread := range []string{"", "100.000001"} {
			t.Run(mode+"/"+thread, func(t *testing.T) {
				capture := &inboundCapture{}
				server := httptest.NewServer(capture.handler())
				defer server.Close()
				cfg := peerTestConfig(server.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), citadelResolver())
				cfg.injectionMode, cfg.reminderTextBudget = mode, 700
				cfg.injectionParents, cfg.deliveredIDs = &injectionParents{}, newDeliveredIDs()
				cfg.injectionParents.remember("C1", "100.000001", "Parent", "parent first line")
				body := strings.Repeat("报告 text\n", 700)
				processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
					peerBotEnvelope(t, "message", "E1", "C1", "100.000002", thread, body, peerTestPeerBotID, ""), func() {})
				got := capture.snapshot()
				if len(got) != 1 || got[0].Actor.DisplayName != "citadel" || !strings.Contains(got[0].Text, body) || got[0].ReplyToMessageID != thread {
					t.Fatal("peer did not arrive whole with ordinary author/coordinates")
				}
				if mode == "minimal" && (!strings.Contains(got[0].Text, "Context: gc slack read") || !strings.Contains(got[0].Text, got[0].Actor.ID)) {
					t.Fatal("minimal sender/pointer missing")
				}
				if thread != "" && !strings.Contains(got[0].Text, "[reply in thread "+thread) {
					t.Fatal("thread anchor missing")
				}
				assertNoPeerInstructions(t, got[0].Text)
				// A distinct event id for the same Slack post must collapse too.
				processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
					peerBotEnvelope(t, "message", "E2", "C1", "100.000002", thread, body, peerTestPeerBotID, ""), func() {})
				if len(capture.snapshot()) != 1 {
					t.Fatal("same-ts peer delivered twice")
				}
			})
		}
	}
}

func TestThreadContextQuotesAndShedsAllAuthorsEqually(t *testing.T) {
	body := strings.Repeat("报告 é ", 240)
	replies := []slackThreadMessage{
		{TS: "100.1", User: "U_HUMAN", Text: body},
		citadelShape("100.2", body),
		ownShape("100.3", body),
		{TS: "100.4", BotID: "B_OTHER", Text: body},
	}
	preamble := formatThreadContextPreamble(replies, "100.1", "100.5", "", nil, nil, tcSelfClassifier().classify)
	if strings.Count(preamble, strings.TrimSpace(body)) != 4 || !utf8.ValidString(preamble) {
		t.Fatal("authors did not receive identical whole quotes")
	}
	if !strings.Contains(preamble, "(own bot)") || !strings.Contains(preamble, "B_OTHER (bot)") {
		t.Fatal("author provenance missing")
	}
	parts := channelReminderParts{preamble: preamble, body: "new message", anchor: "[reply in thread 100.1]"}
	text, _, _, used, clipped := composeChannelReminderText(parts, "", "", 700)
	if used || clipped || strings.Contains(text, body) || !strings.Contains(text, "[4 omitted — thread context over budget]") || !strings.Contains(text, parts.body) {
		t.Fatal("context was shed with author preference or no count")
	}
	// A nil classifier affects names only; no history author disappears.
	withoutClassifier := formatThreadContextPreamble(replies, "100.1", "100.5", "", nil, nil, nil)
	if strings.Count(withoutClassifier, strings.TrimSpace(body)) != 4 {
		t.Fatal("nil classifier filtered history authors")
	}
	window := injectionWindow{Known: true}
	collapsed := formatThreadContextPreamble(replies, "100.1", "100.5", "", nil, func(ts string) bool { return ts == "100.1" || ts == "100.2" }, tcSelfClassifier().classify, &window)
	if window.Collapsed != 2 || strings.Count(collapsed, strings.TrimSpace(body)) != 2 {
		t.Fatal("human/peer delivered collapse differs")
	}
}

func TestWholeBodySurvivesOptionalContextBudget(t *testing.T) {
	for _, budget := range []int{0, 1, 700, 3500} {
		body := strings.Repeat("界é body ", 1000)
		parts := channelReminderParts{body: body, files: "[file: synthetic.txt]", anchor: "[reply in thread 1]"}
		got, _, _, _, trimmed := composeChannelReminderText(parts, "", "help", budget)
		if trimmed || !strings.Contains(got, body) || !strings.Contains(got, parts.files) || !utf8.ValidString(got) {
			t.Fatal("new message was clipped by the context budget")
		}
	}
}
