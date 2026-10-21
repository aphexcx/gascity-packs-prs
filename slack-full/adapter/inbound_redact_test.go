package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
