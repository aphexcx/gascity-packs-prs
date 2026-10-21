package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func redactionDM(t *testing.T) ([]byte, string) {
	t.Helper()
	body, err := os.ReadFile("../tests/fixtures/redaction/dm.json")
	if err != nil {
		t.Fatal(err)
	}
	var env slackEventEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	var msg slackMessageEvent
	if err := json.Unmarshal(env.Event, &msg); err != nil {
		t.Fatal(err)
	}
	return body, strings.Fields(msg.Text)[3]
}

func assertInboundRedacted(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatal("synthetic secret escaped redaction")
	}
	if !strings.Contains(text, "[REDACTED:openai:...7z9Q]") {
		t.Fatalf("missing marker: %q", text)
	}
}

// Fake gc persists the exact inbound HTTP bytes as a journal line and renders
// its mail body. This tests the adapter boundary before either gc sink; the
// Python suite runs the same DM fixture through the tail helper independently.
func TestInboundRedactionDMJournalMailAndDeadLetter(t *testing.T) {
	body, secret := redactionDM(t)
	dir := t.TempDir()
	journal := filepath.Join(dir, "events.jsonl")
	mail := filepath.Join(dir, "mail.txt")
	gcStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		var payload struct {
			Message externalInboundMessage `json:"message"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		if err := os.WriteFile(journal, append(raw, '\n'), 0o600); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(mail, []byte(renderAliasDispatch(config{}, payload.Message, "mayor")), 0o600); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer gcStub.Close()
	wg := &sync.WaitGroup{}
	cfg := config{gcAPIBase: gcStub.URL, cityName: "test-city", provider: "slack", accountID: "T1",
		slackSigningKey: "test-signing-secret", dispatchSem: defaultTestDispatchSem, eventWG: wg}
	w := httptest.NewRecorder()
	handleSlackEvents(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil)(w, signedSlackEventRequest(t, cfg.slackSigningKey, body))
	if w.Code != 200 {
		t.Fatalf("HTTP %d", w.Code)
	}
	wg.Wait()
	for _, path := range []string{journal, mail} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		assertInboundRedacted(t, string(data), secret)
	}
	// The dead-letter and shutdown-spool backstops also protect older/replayed
	// payloads that did not traverse today's ingest hook.
	inbound := externalInboundMessage{ProviderMessageID: "1.0", Text: secret}
	path, err := writeAliasDeadLetter(filepath.Join(dir, "dead"), "D0DM", inbound, "session", "mayor", secret, errors.New(secret))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertInboundRedacted(t, string(data), secret)
	spoolPath := filepath.Join(dir, "spool.jsonl")
	spool := newInboundSpool(spoolPath)
	if !spool.spillBatch("D0DM", []pendingChannelInbound{{inbound: inbound, preamble: secret, body: secret}}) {
		t.Fatal("spill failed")
	}
	data, err = os.ReadFile(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	assertInboundRedacted(t, string(data), secret)
}

func TestInboundRedactionNestedAndEscapedText(t *testing.T) {
	body, secret := redactionDM(t)
	var env slackEventEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	// Slack JSON may Unicode-escape characters in a token.
	raw := strings.ReplaceAll(string(env.Event), "sk-test-", `\u0073k-test-`)
	got, err := redactEventContent([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	assertInboundRedacted(t, string(got), secret)
	if strings.Contains(string(got), `\u0073k-test-`) {
		t.Fatal("escaped secret survived")
	}
	if strings.Count(string(got), "[REDACTED:openai:...7z9Q]") != 2 {
		t.Fatal("nested block text not redacted")
	}
}

func TestInboundRedactionCompanyDurableAdmission(t *testing.T) {
	_, secret := redactionDM(t)
	gc := newFakeGC(t)
	df, bf := baseDirectoryFile(), baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	env := companyEnvelope(t, humanMessage(baseOrigin().TS, secret))
	// Company receipts retain the whole event, including content outside text.
	var event map[string]any
	if err := json.Unmarshal(env.Event, &event); err != nil {
		t.Fatal(err)
	}
	event["blocks"] = []any{map[string]any{"type": "rich_text", "elements": []any{
		map[string]any{"type": "link", "url": "https://example.test/" + secret},
	}}}
	event["attachments"] = []any{map[string]any{"pretext": secret, "footer": secret,
		"fields": []any{map[string]any{"title": "credential", "value": secret}}}}
	event["metadata"] = map[string]any{"event_type": "gc_delegation_result", "event_payload": map[string]any{
		"nonce": "gcs-0123456789abcdef0123", "comment": secret,
	}}
	env.Event, _ = json.Marshal(event)
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{slackSigningKey: "test-signing-secret", companyGateway: h.gw}
	w := httptest.NewRecorder()
	handleSlackEvents(cfg, nil, nil, nil, nil, nil)(w, signedSlackEventRequest(t, cfg.slackSigningKey, body))
	if w.Code != 200 {
		t.Fatalf("HTTP %d", w.Code)
	}
	h.wait()
	calls := gc.sessionCalls()
	if len(calls) != 1 {
		t.Fatalf("deliveries: %d", len(calls))
	}
	assertInboundRedacted(t, calls[0].body, secret)
	markers := 0
	err = filepath.WalkDir(h.ingressDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), secret) {
			t.Error("company receipt persisted synthetic secret")
		}
		markers += strings.Count(string(data), "[REDACTED:")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if markers < 6 {
		t.Fatal("nested content missing from redacted receipt")
	}
}

func TestInboundRedactionBeforeContextAndHoldClipping(t *testing.T) {
	_, secret := redactionDM(t)
	text := strings.Repeat(". ", livenessHeldTextMax/2-5) + secret
	held, _ := clipHeldMessage(slackHistoryMessage{Text: text})
	if strings.Contains(held.Text, "sk-test-") {
		t.Fatal("hold clipped before redaction")
	}
	preamble := formatThreadContextPreamble([]slackThreadMessage{{User: "U1", TS: "1.0", Text: secret}}, "1.0", "2.0", "", nil, nil, nil)
	assertInboundRedacted(t, preamble, secret)
}

func TestInboundRedactionPreservesProtocolFields(t *testing.T) {
	_, secret := redactionDM(t)
	fileURL := "https://files.slack.com/files-pri/T123-F123/download-file-original-name.txt"
	event := map[string]any{
		"channel": "D123", "ts": "1792436306.401379",
		"metadata": map[string]any{"event_type": "gc_delegation_result", "event_payload": map[string]any{"nonce": "gcs-0123456789abcdef0123", "comment": secret}},
		"files":    []any{map[string]any{"url_private": fileURL, "url_private_download": fileURL, "title": secret}},
	}
	raw, _ := json.Marshal(event)
	got, err := redactEventContent(raw)
	if err != nil {
		t.Fatal(err)
	}
	assertInboundRedacted(t, string(got), secret)
	for _, protocol := range []string{fileURL, "gcs-0123456789abcdef0123", "D123", "1792436306.401379"} {
		if !strings.Contains(string(got), protocol) {
			t.Fatalf("protocol field changed: %s", protocol)
		}
	}
}

func TestInboundRedactionDistinctFilesWithSameSuffix(t *testing.T) {
	testAllowAnyURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	defer srv.Close()
	files := []slackFile{
		{ID: "F1", Name: "sk-one-AAAAAAAA7z9Q.txt", URLPrivate: srv.URL + "/one"},
		{ID: "F2", Name: "sk-two-BBBBBBBB7z9Q.txt", URLPrivate: srv.URL + "/two"},
	}
	raw, _ := json.Marshal(slackMessageEvent{Files: files})
	clean, err := redactEventContent(raw)
	if err != nil {
		t.Fatal(err)
	}
	var msg slackMessageEvent
	if err := json.Unmarshal(clean, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Files[0].Name != msg.Files[1].Name {
		t.Fatal("fixture must collide after redaction")
	}
	atts := downloadSlackFiles(config{inboundFileStore: t.TempDir(), slackBotToken: "fixture"}, "D1", "1.0", msg.Files)
	if len(atts) != 2 || atts[0].URL == atts[1].URL {
		t.Fatal("distinct files share a destination")
	}
	for i, want := range []string{"/one", "/two"} {
		data, err := os.ReadFile(strings.TrimPrefix(atts[i].URL, "file://"))
		if err != nil || string(data) != want {
			t.Fatalf("attachment %d overwritten: %q, %v", i, data, err)
		}
	}
}

func TestInboundRedactionReplaysOldCompanyReceipt(t *testing.T) {
	_, secret := redactionDM(t)
	gc := newFakeGC(t)
	df, bf := baseDirectoryFile(), baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	origin := baseOrigin()
	raw, _ := json.Marshal(humanMessage(origin.TS, secret))
	hydration := companyHydration{
		Root:    &companyHydrationRoot{TS: origin.TS, User: "U1", Text: secret},
		Excerpt: []companyExcerptLine{{TS: origin.TS, User: "U1", Text: secret}},
		Files:   []companyHydrationFile{{Name: secret, Content: secret, Status: companyFileStatusIncluded}},
	}
	frozen, _ := json.Marshal(hydration)
	// Seed the pre-upgrade on-disk form, bypassing the new ingress hook.
	_, _, err := h.receipts.Admit(&IngressReceipt{Origin: origin, Event: raw, Hydration: frozen, Status: ingressStatusReceived})
	if err != nil {
		t.Fatal(err)
	}
	h.reopen(t, gc.server.URL, 4)
	if err := h.gw.recoverPending(); err != nil {
		t.Fatal(err)
	}
	h.wait()
	calls := gc.sessionCalls()
	if len(calls) != 1 {
		t.Fatalf("replay deliveries: %d", len(calls))
	}
	assertInboundRedacted(t, calls[0].body, secret)
	receipt, err := h.receipts.Get(origin)
	if err != nil || receipt == nil {
		t.Fatalf("receipt: %v", err)
	}
	assertInboundRedacted(t, string(receipt.Hydration), secret)
	// The DM and group-DM renderers also consume frozen pre-upgrade bundles.
	for name, body := range map[string]string{
		"dm":   renderCompanyDMReminder("agent", secret, origin.TS, origin.TS, hydration, nil),
		"mpim": renderCompanyMpimReminder("agent", secret, origin.TS, origin.TS, hydration, nil),
	} {
		t.Run(name, func(t *testing.T) { assertInboundRedacted(t, body, secret) })
	}
}

func TestInboundRedactionReplaysOldMentionOnlyReceipt(t *testing.T) {
	_, secret := redactionDM(t)
	gc := newFakeGC(t)
	df, bf := baseDirectoryFile(), baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	h.openBarrier()
	withMentionOnlyLane(t, h, outsiderBinding())
	h.gw.mentionOnlyLaneStart = func() bool { return false }
	ev := humanMessage("1700000000.002600", "<@"+companyMOBotUserID+"> "+secret)
	ev.Files = []slackFile{{ID: "F123", Name: secret + ".txt"}}
	// Admission bypasses the HTTP scrub to represent a pre-upgrade receipt.
	admitCompanyRoomMessage(t, h, ev)
	h.wait()
	if outsiderPostCount(gc) != 0 {
		t.Fatal("mention-only lane ran before restart")
	}
	h.gw.mentionOnlyLaneStart = nil
	h.gw.cfg.channelClaims = newEventDedupCache(time.Minute)
	h.gw.cfg.mentionOnlyDeliveries = newMentionOnlyDeliveryLog()
	h.gw.replayMentionOnlyPending()
	h.wait()
	if outsiderPostCount(gc) != 1 {
		t.Fatal("mention-only receipt was not replayed exactly once")
	}
	for _, call := range gc.sessionCalls() {
		assertInboundRedacted(t, call.body, secret)
	}
}

func TestInboundRedactionWithheldPathsSurviveDeliveryAndSpool(t *testing.T) {
	_, secret := redactionDM(t)
	const path = "/tmp/gc-slack-adapter/inbound/C1234567890/1792436306.401379-image.png"
	msg := externalInboundMessage{ProviderMessageID: "1792436306.401379", Text: secret,
		Attachments: []externalAttachment{{URL: "file://" + path}}}
	p := withholdAttachments(pendingChannelInbound{inbound: msg}, permanent422())
	capture := &inboundCapture{}
	server := httptest.NewServer(capture.handler())
	defer server.Close()
	cfg := config{gcAPIBase: server.URL, cityName: "test-city"}
	if err := postInbound(cfg, p.inbound); err != nil {
		t.Fatal(err)
	}
	spool := newInboundSpool(filepath.Join(t.TempDir(), "spool.jsonl"))
	if !spool.spillBatch("C1234567890", []pendingChannelInbound{p}) {
		t.Fatal("spill failed")
	}
	entries, done, err := spool.consume()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if len(entries) != 1 {
		t.Fatalf("spool replay returned %d entries", len(entries))
	}
	if err := postInbound(cfg, entries[0].Inbound); err != nil {
		t.Fatal(err)
	}
	got := capture.snapshot()
	if len(got) != 2 {
		t.Fatalf("gc received %d messages", len(got))
	}
	for _, delivered := range got {
		assertInboundRedacted(t, delivered.Text, secret)
		if !strings.Contains(delivered.Text, path) || len(delivered.Attachments) != 0 {
			t.Fatalf("withheld attachment path was lost or attachment was reintroduced: %+v", delivered)
		}
	}
	// A matching path supplied only as untrusted text gets no exemption.
	if clean := redactInboundMessage(externalInboundMessage{Text: path}); clean.Text == path {
		t.Fatal("untrusted text gained the generated-path exemption")
	}
}

func TestInboundRedactionOldSpoolBeforeBudgeting(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	// The protected 200-rune head used to expose 31 of these 32 hex
	// characters, leaving a fragment that the delivery scrub cannot match.
	text := strings.Repeat(".", 169) + secret + strings.Repeat(". ", 1000)
	entry := spooledInbound{Channel: "C1", Inbound: externalInboundMessage{ProviderMessageID: "1.0", Text: text}}
	raw, _ := json.Marshal(entry)
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, done, err := newInboundSpool(path).consume()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if len(entries) != 1 {
		t.Fatalf("old spool entries: %d", len(entries))
	}
	capture := &inboundCapture{}
	server := httptest.NewServer(capture.handler())
	defer server.Close()
	cfg := config{gcAPIBase: server.URL, cityName: "test-city", reminderTextBudget: 200}
	if err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{{inbound: entries[0].Inbound}}); err != nil {
		t.Fatal(err)
	}
	got := capture.snapshot()
	if len(got) != 1 || strings.Contains(got[0].Text, secret[:20]) || !strings.Contains(got[0].Text, "[REDACTED:hex:...cdef]") {
		t.Fatalf("old spool leaked a clipped secret or lost its marker: %+v", got)
	}
}

func TestInboundRedactionInteractionIngress(t *testing.T) {
	_, secret := redactionDM(t)
	for _, kind := range []string{"slash", "block_actions", "view_submission"} {
		t.Run(kind, func(t *testing.T) {
			bodies := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				bodies <- string(raw)
				w.WriteHeader(http.StatusAccepted)
			}))
			defer server.Close()
			cfg := config{slackSigningKey: "test-signing-secret", accountID: "T1", cityName: "test-city",
				gcAPIBase: server.URL, dispatchSem: defaultTestDispatchSem}
			registry := newTestChannelMappingRegistry(t)
			if err := registry.Set(channelMappingDiskRecord{WorkspaceID: "T1", ChannelID: "C1", TargetKind: "session", TargetID: "test-session"}); err != nil {
				t.Fatal(err)
			}
			form := url.Values{"team_id": {"T1"}, "channel_id": {"C1"}, "command": {"/gc"}, "text": {secret}, "user_id": {"U1"}}
			if kind != "slash" {
				payload := map[string]any{"type": kind, "team": map[string]string{"id": "T1"},
					"channel": map[string]string{"id": "C1"}, "user": map[string]string{"id": "U1"}, "trigger_id": "123.456.trigger"}
				if kind == "block_actions" {
					payload["actions"] = []any{map[string]any{"action_id": "test-action", "type": "button", "value": secret,
						"selected_option": map[string]string{"value": secret}}}
				} else {
					payload["view"] = map[string]any{"callback_id": "test-callback", "private_metadata": `{"session_id":"test-session"}`,
						"state": map[string]any{"values": map[string]any{"block": map[string]any{"input": map[string]string{"type": "plain_text_input", "value": secret}}}}}
				}
				raw, _ := json.Marshal(payload)
				// Exercise decoded JSON redaction, not a raw-text replacement.
				form = url.Values{"payload": {strings.ReplaceAll(string(raw), "sk-test-", `\u0073k-test-`)}}
			}
			w := httptest.NewRecorder()
			handleSlackInteractions(cfg, registry, nil)(w, signedSlackInteractionRequest(t, cfg.slackSigningKey, []byte(form.Encode())))
			if w.Code != http.StatusOK {
				t.Fatalf("interaction returned %d: %s", w.Code, w.Body.String())
			}
			select {
			case body := <-bodies:
				assertInboundRedacted(t, body, secret)
			case <-time.After(2 * time.Second):
				t.Fatal("interaction was not delivered")
			}
			dispatchInflightWG.Wait()
		})
	}
}

func TestInboundRedactionOldRigModalMetadata(t *testing.T) {
	_, secret := redactionDM(t)
	raw, _ := json.Marshal(slackRigDispatchMetadata{Kind: metadataKindRigFix, WorkspaceID: "T1", RigName: "test-rig", OriginalCommandText: secret})
	meta, ok := decodeRigDispatchMetadata(string(raw))
	if !ok || meta.RigName != "test-rig" {
		t.Fatal("redaction changed modal routing")
	}
	assertInboundRedacted(t, meta.OriginalCommandText, secret)
}
