package main

// THE OFF-PARITY TEST (PR 104 round 2, item 11). With ACK_REACTION unset the
// adapter must behave as it does at the PR's base, c9969436. This file runs
// byte for byte at the base, at the PR head and at the tip. It names only
// symbols that exist at the base; what the start wires after the base (the
// arrival service, the stopwatch writer) is reached through ONE hook,
// offparity_hook_test.go, which does nothing at the base. The expected values
// below are what the base produces; the base run proves them.
//
// The only differences allowed, each named with its contract item:
//   - the stopwatch's own files, stopwatch-YYYY-MM.jsonl (item 15);
//   - the healthz line jev_api_key=... (item 10);
//   - the stopwatch's own warn-once lines (item 19), which this healthy
//     script never prints.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type offParitySlack struct {
	mu      sync.Mutex
	calls   map[string]int
	posted  int
	lastTS  string
	lastTxt string
	root    string
}

func (s *offParitySlack) serve(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/")
	_ = r.ParseForm()
	body, _ := io.ReadAll(r.Body)
	var fields map[string]any
	_ = json.Unmarshal(body, &fields)
	key := method
	// The readback's own parameters show which thread it read (case 1e).
	for _, p := range []string{"ts", "thread_ts"} {
		if v := r.Form.Get(p); v != "" {
			key += " " + p + "=" + v
		} else if v, ok := fields[p].(string); ok && v != "" {
			key += " " + p + "=" + v
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[key]++
	w.Header().Set("Content-Type", "application/json")
	switch method {
	case "chat.postMessage":
		s.posted++
		s.lastTS = fmt.Sprintf("%s.%06d", strings.Split(s.root, ".")[0], 500+s.posted)
		s.lastTxt, _ = fields["text"].(string)
		answer := map[string]any{"ok": true, "ts": s.lastTS, "channel": fields["channel"]}
		if thread, _ := fields["thread_ts"].(string); thread != "" {
			// Slack answers a reply with the thread's canonical root.
			answer["message"] = map[string]string{"thread_ts": s.root}
		}
		_ = json.NewEncoder(w).Encode(answer)
	case "conversations.history", "conversations.replies":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]string{{"ts": s.lastTS, "text": s.lastTxt}}})
	case "files.getUploadURLExternal":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upload_url": "http://" + r.Host + "/upload", "file_id": "F0EXAMPLE"})
	case "files.completeUploadExternal":
		// Case 1h: an answer that carries no shares.
		_, _ = io.WriteString(w, `{"ok":true,"files":[{"id":"F0EXAMPLE"}]}`)
	case "files.info":
		_, _ = io.WriteString(w, `{"ok":true,"file":{"id":"F0EXAMPLE"}}`)
	default:
		_, _ = io.WriteString(w, `{"ok":true}`)
	}
}

func (s *offParitySlack) list(now string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, v := range s.calls {
		out = append(out, strings.ReplaceAll(k, now, "<now>")+" x"+strconv.Itoa(v))
	}
	sort.Strings(out)
	return out
}

type offParityGC struct {
	mu    sync.Mutex
	turns []string
}

func (g *offParityGC) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var env struct {
		Message externalInboundMessage `json:"message"`
	}
	line := r.Method + " " + r.URL.Path
	if json.Unmarshal(body, &env) == nil && env.Message.ProviderMessageID != "" {
		m := env.Message
		line += " | " + m.Conversation.ConversationID + " | " + m.Conversation.Kind + " | " + m.ProviderMessageID + " | " + m.ReplyToMessageID + " | " + m.Text
	}
	g.mu.Lock()
	g.turns = append(g.turns, line)
	g.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (g *offParityGC) list(now string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, t := range g.turns {
		out = append(out, strings.ReplaceAll(t, now, "<now>"))
	}
	sort.Strings(out) // events dispatch on their own goroutines
	return out
}

func offParityTree(t *testing.T, root, now string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." || regexp.MustCompile(`(^|/)stopwatch-\d{4}-\d{2}\.jsonl$`).MatchString(rel) { // item 15
			return nil
		}
		if info.IsDir() {
			out = append(out, rel+"/")
			return nil
		}
		if strings.Contains(rel, "inbound-journal") {
			// The journal's rows by phase: case 1f adds none for a root post.
			data, _ := os.ReadFile(path)
			var phases []string
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var row struct {
					Phase     string `json:"phase"`
					Channel   string `json:"channel"`
					ThreadTS  string `json:"thread_ts"`
					MessageTS string `json:"message_ts"`
				}
				if json.Unmarshal([]byte(line), &row) == nil && row.Phase != "" {
					phases = append(phases, row.Phase+"@"+row.Channel+"/"+row.MessageTS+"/thread="+row.ThreadTS)
				}
			}
			sort.Strings(phases)
			rel = regexp.MustCompile(`\d{8}\.jsonl$`).ReplaceAllString(rel, "<date>.jsonl")
			rel += " rows=" + strings.Join(phases, ",")
		}
		if strings.Contains(rel, "publish-receipts") {
			// The stored receipt's thread (case 1e).
			data, _ := os.ReadFile(path)
			var receipt struct {
				MessageID string `json:"message_id"`
				ThreadTS  string `json:"thread_ts"`
				ReplyTo   string `json:"reply_to_message_id"`
				Delivered bool   `json:"delivered"`
				Readback  string `json:"readback"`
			}
			_ = json.Unmarshal(data, &receipt)
			rel += fmt.Sprintf(" message=%s thread=%s reply_to=%s delivered=%t readback=%s", receipt.MessageID, receipt.ThreadTS, receipt.ReplyTo, receipt.Delivered, receipt.Readback)
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range out {
		out[i] = strings.ReplaceAll(out[i], now, "<now>")
	}
	sort.Strings(out)
	return out
}

func offParityMatch(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("OFF-PARITY %s differs from the base:\n got: %q\nwant: %q", what, got, want)
	}
}

func offParityLogLines(text, now, city string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" || strings.HasPrefix(line, "stopwatch:") { // item 19
			continue
		}
		line = strings.ReplaceAll(strings.ReplaceAll(line, city, "<city>"), now, "<now>")
		line = regexp.MustCompile(`127\.0\.0\.1:\d+`).ReplaceAllString(line, "127.0.0.1:<port>")
		line = regexp.MustCompile(`\d+(\.\d+)?(ms|µs|s)\b`).ReplaceAllString(line, "<d>")
		out = append(out, line)
	}
	sort.Strings(out)
	return out
}

func offParityHealthz() map[string]string {
	rec := httptest.NewRecorder()
	handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		if strings.HasPrefix(line, "jev_api_key=") { // item 10
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		out[k] = v
	}
	return out
}

// healthz counters are process-wide; the script's own effect is the change.
func offParityHealthzDelta(before, after map[string]string) []string {
	var out []string
	for k, v := range after {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			m, _ := strconv.ParseInt(before[k], 10, 64)
			out = append(out, k+"=+"+strconv.FormatInt(n-m, 10))
		} else {
			out = append(out, k+"="+v)
		}
	}
	sort.Strings(out)
	return out
}

func TestOffParityWithTheBase(t *testing.T) {
	now := time.Now()
	sec := strconv.FormatInt(now.Unix(), 10)
	ts := func(n int) string { return fmt.Sprintf("%s.%06d", sec, n) }
	slack := &offParitySlack{calls: map[string]int{}, root: ts(200)}
	slackStub := httptest.NewServer(http.HandlerFunc(slack.serve))
	defer slackStub.Close()
	withSlackAPIStub(t, slackStub)
	fakeGC := &offParityGC{}
	gcStub := httptest.NewServer(http.HandlerFunc(fakeGC.serve))
	defer gcStub.Close()
	city, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	upload := filepath.Join(city, "uploads")
	if err := os.MkdirAll(upload, 0o700); err != nil {
		t.Fatal(err)
	}
	env := baseSlackEnv()
	env["GC_API_BASE_URL"] = gcStub.URL
	env["GC_CITY_PATH"] = city
	env["FILE_UPLOAD_ROOT"] = upload
	for _, key := range []string{"ACK_REACTION", "ACK_SCOPE", "JEV_API_KEY", "GC_SERVICE_STATE_ROOT"} {
		t.Setenv(key, "")
	}
	healthzBefore := offParityHealthz()
	read, restore := captureLog(t)
	defer restore()
	cfg, err := loadConfigFromLookup(func(key string) (string, bool) { v, ok := env[key]; return v, ok })
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	// The hook runs the start's arrival and stopwatch half (main.go at the
	// head, startArrival); at the base there is none.
	if err := offParityStart(t, &cfg, ""); err != nil {
		t.Fatalf("start: %v", err)
	}
	cfg.dispatchSem = make(chan struct{}, 16)
	cfg.busyMarks = newBusyReactionRegistry()
	cfg.eventDedup = newEventDedupCache(eventDedupTTL)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	cfg.fileUploadRoot = upload
	if cfg.publishReceipts, err = newPublishReceiptStore(companyStateDirDefault(cfg.cityPath, "publish-receipts")); err != nil {
		t.Fatal(err)
	}

	event := func(id string, ev slackMessageEvent) {
		t.Helper()
		raw, _ := json.Marshal(ev)
		body, _ := json.Marshal(slackEventEnvelope{Type: "event_callback", TeamID: "T01234567", EventID: id, Event: raw,
			Authorizations: []slackEventAuthorization{{UserID: "U0BOTSELF", IsBot: true}}})
		if w := postSignedEvent(t, cfg, body); w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", id, w.Code)
		}
		time.Sleep(150 * time.Millisecond) // one event at a time
	}
	event("Ev01", slackMessageEvent{Type: "message", ChannelType: "im", Channel: "DEXAMPLE", User: "U0AAAAAA1", TS: ts(101), Text: "a direct message"})
	event("Ev02", slackMessageEvent{Type: "message", ChannelType: "mpim", Channel: "GEXAMPLE", User: "U0AAAAAA1", TS: ts(102), Text: "<@U0BOTSELF> in a group"})
	event("Ev03", slackMessageEvent{Type: "app_mention", ChannelType: "channel", Channel: "CEXAMPLE", User: "U0AAAAAA1", TS: ts(103), Text: "<@U0BOTSELF> please look"})
	event("Ev04", slackMessageEvent{Type: "message", ChannelType: "channel", Channel: "CEXAMPLE", User: "U0AAAAAA1", TS: ts(104), Text: "no mention here"})
	event("Ev05", slackMessageEvent{Type: "message", ChannelType: "im", Channel: "DEXAMPLE", BotID: "B0EXAMPLE", User: "U0AAAAAA9", TS: ts(105), Text: "a bot speaks"})
	event("Ev06", slackMessageEvent{Type: "message", ChannelType: "im", Channel: "DEXAMPLE", Subtype: "message_changed", TS: ts(106), Text: ""})
	time.Sleep(500 * time.Millisecond)

	publish := func(replyTo, key string) {
		t.Helper()
		body, _ := json.Marshal(publishRequest{SessionID: "gc-offparity", Conversation: conversationRef{ConversationID: "CEXAMPLE"}, Text: "the mayor answers " + key, ReplyToMessageID: replyTo, IdempotencyKey: key})
		rec := httptest.NewRecorder()
		handlePublish(cfg, nil, nil, newPublishDedupCache(time.Hour))(rec, httptest.NewRequest(http.MethodPost, "/publish", bytes.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("publish %s: %d %s", key, rec.Code, rec.Body.String())
		}
	}
	publish("", "offparity-root")       // case 1f
	publish(ts(300), "offparity-child") // case 1e: a child of the thread at ts(200)
	file := filepath.Join(upload, "answer.txt")
	if err := os.WriteFile(file, []byte("an answer"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handlePublishFile(cfg, nil)(rec, httptest.NewRequest(http.MethodPost, "/publish-file", strings.NewReader(fmt.Sprintf(`{"conversation":{"conversation_id":"CEXAMPLE"},"file_path":%q}`, file))))
	if rec.Code != http.StatusOK {
		t.Fatalf("publish-file: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handleReact(cfg)(rec, httptest.NewRequest(http.MethodPost, "/react", strings.NewReader(fmt.Sprintf(`{"conversation":{"conversation_id":"CEXAMPLE"},"message_id":%q,"emoji":"thumbsup"}`, ts(103)))))
	if rec.Code != http.StatusOK {
		t.Fatalf("react: %d %s", rec.Code, rec.Body.String())
	}

	// A company-room mention under a stale roster (case 1b), on the company
	// gateway with the started arrival wiring copied in as main copies cfg.
	gc := newFakeGC(t)
	df := baseDirectoryFile()
	df.Rooms[0].MentionWake = []string{"ollie"}
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
	setFixedClock(h)
	var probes int32
	h.gw.channelMembersProbe = countingMembersProbe(map[string]bool{botRiley: true}, true, &probes)
	offParityCompany(t, h.gw, cfg)
	h.openBarrier()
	if _, handled := h.admitViaHandler(t, humanMessage(ts(110), driftMention(botRiley)), 0); !handled {
		t.Fatal("company mention not handled")
	}
	h.wait()
	offParityWait(t, cfg)

	company := []string{
		"membership_probes=" + strconv.Itoa(int(atomic.LoadInt32(&probes))),
		"session_calls=" + strconv.Itoa(len(gc.sessionCalls())),
	}
	// The company gateway's own healthz lines (company_roster_drift_wakes).
	for _, line := range strings.Split(strings.TrimSpace(h.gw.healthzDetail()), "\n") {
		company = append(company, strings.ReplaceAll(line, h.ingressDir, "<ingress>"))
	}
	healthz := offParityHealthzDelta(healthzBefore, offParityHealthz())
	logs := offParityLogLines(read(), sec, city)
	t.Logf("turns=%#v", fakeGC.list(sec))
	t.Logf("calls=%#v", slack.list(sec))
	t.Logf("company=%#v", company)
	t.Logf("healthz=%#v", healthz)
	t.Logf("logs=%#v", logs)
	t.Logf("state=%#v", offParityTree(t, city, sec))
	offParityMatch(t, "turns raised", fakeGC.list(sec), offParityWantTurns)
	offParityMatch(t, "vendor calls", slack.list(sec), offParityWantCalls)
	offParityMatch(t, "company counters", company, offParityWantCompany)
	offParityMatch(t, "healthz", healthz, offParityWantHealthz)
	offParityMatch(t, "log lines", logs, offParityWantLogs)
	offParityMatch(t, "state directory", offParityTree(t, city, sec), offParityWantState)
}

// What a start accepts with ACK_REACTION unset: an ACK_SCOPE the base never
// reads (case 1c), and an executable that cannot be hashed (case 1d).
func TestOffParityStartsAsTheBase(t *testing.T) {
	for _, tc := range []struct{ name, scope, executable string }{
		{"ACK_SCOPE=everyone (1c)", "everyone", ""},
		{"an executable that cannot be hashed (1d)", "", "no-such-executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			env := baseSlackEnv()
			env["GC_API_BASE_URL"] = "http://127.0.0.1:9"
			env["GC_CITY_PATH"] = city
			if tc.scope != "" {
				env["ACK_SCOPE"] = tc.scope
			}
			cfg, err := loadConfigFromLookup(func(key string) (string, bool) { v, ok := env[key]; return v, ok })
			if err != nil {
				t.Fatalf("OFF-PARITY config refused where the base starts: %v", err)
			}
			executable := ""
			if tc.executable != "" {
				executable = filepath.Join(city, tc.executable)
			}
			if err := offParityStart(t, &cfg, executable); err != nil {
				t.Errorf("OFF-PARITY start refused where the base starts: %v", err)
			}
		})
	}
}

// The base's observations of the script above (c9969436, base hooks),
// generated from that run's own %#v output.
var (
	offParityWantTurns   = []string{"POST /v0/city/test-city/extmsg/inbound | CEXAMPLE | room | <now>.000103 |  | <@U0BOTSELF> please look", "POST /v0/city/test-city/extmsg/inbound | CEXAMPLE | room | <now>.000104 |  | no mention here", "POST /v0/city/test-city/extmsg/inbound | DEXAMPLE | dm | <now>.000101 |  | a direct message", "POST /v0/city/test-city/extmsg/inbound | GEXAMPLE | room | <now>.000102 |  | <@U0BOTSELF> in a group"}
	offParityWantCalls   = []string{"chat.postMessage thread_ts=<now>.000300 x1", "chat.postMessage x1", "conversations.history x1", "conversations.replies ts=<now>.000300 x1", "files.completeUploadExternal x1", "files.getUploadURLExternal x1", "reactions.add x3", "reactions.remove x1", "upload x1"}
	offParityWantCompany = []string{"membership_probes=1", "session_calls=1", "company_barrier_ready=true", "company_store_ready=true", "company_store_error=\"\"", "company_write_failures=0", "company_delivery_failures=0", "company_target_reresolved=0", "company_materialize_requests=0", "company_boot_requests=0", "company_delivered_asleep=0", "company_roster_drift_wakes=1", "company_stale_posting_intents=0", "company_correlation_parked=0", "company_directory_loaded=true", "company_bindings_loaded=true", "company_dm_bindings_loaded=false", "registered_agent_apps=0", "registered_agent_apps_join_warnings=0", "company_dm_sig_reject=0", "company_dm_token_missing=0", "company_dm_receipts_received=0", "company_dm_receipts_routing=0", "company_dm_receipts_delivered=0", "company_dm_receipts_no_delivery=0", "company_dm_receipts_failed=0", "company_mpim_receipts_received=0", "company_mpim_receipts_routing=0", "company_mpim_receipts_delivered=0", "company_mpim_receipts_no_delivery=0", "company_mpim_receipts_failed=0", "company_body_missing=0", "company_bodies_redacted=0", "company_body_digest_mismatch=0"}
	offParityWantHealthz = []string{"dispatch_dropped_total=+0", "ok="}
	offParityWantLogs    = []string{"company: roster-drift eligibility overlay admitted agent=riley bot_user_id=U0AAAAAA2 room=orchestrator-team channel=C0AAAAAAA (live channel member, directory roster stale — recapture declaratively)", "inbound: chan=CEXAMPLE user=U0AAAAAA1 ts=<now>.000103 thread= target=\"\" bot_mention=true files=0 spooled=0 text=24ch posted=24ch receipt=unsupported", "inbound: chan=CEXAMPLE user=U0AAAAAA1 ts=<now>.000104 thread= target=\"\" bot_mention=false files=0 spooled=0 text=15ch posted=15ch receipt=unsupported", "inbound: chan=DEXAMPLE user=U0AAAAAA1 ts=<now>.000101 thread= target=\"\" bot_mention=false files=0 spooled=0 text=16ch posted=16ch receipt=unsupported", "inbound: chan=GEXAMPLE user=U0AAAAAA1 ts=<now>.000102 thread= target=\"\" bot_mention=true files=0 spooled=0 text=23ch posted=23ch receipt=unsupported", "publish-file: conv=CEXAMPLE file=answer.txt size=9 reply_to= session= as=\"\"", "publish: conv=CEXAMPLE text=32ch posted=32ch reply_to= idem=offparity-root session=gc-offparity as=\"\" mentions_rewritten=false", "publish: conv=CEXAMPLE text=33ch posted=33ch reply_to=<now>.000300 idem=offparity-child session=gc-offparity as=\"\" mentions_rewritten=false", "react: op=reactions.add conv=CEXAMPLE ts=<now>.000103 emoji=thumbsup"}
	offParityWantState   = []string{".gc/", ".gc/slack/", ".gc/slack/inbound-journal/", ".gc/slack/inbound-journal/<date>.jsonl rows=accepted@CEXAMPLE/<now>.000103/thread=,accepted@CEXAMPLE/<now>.000104/thread=,accepted@DEXAMPLE/<now>.000101/thread=,accepted@GEXAMPLE/<now>.000102/thread=,reaction@CEXAMPLE/<now>.000103/thread=,reply@CEXAMPLE/<now>.000502/thread=<now>.000300", ".gc/slack/publish-receipts/", ".gc/slack/publish-receipts/11f3a8939e248e75a8027adc52c8b4f2f94cab4b721b1f5a176f82cd0e923876.json message=<now>.000502 thread=<now>.000300 reply_to=<now>.000300 delivered=true readback=confirmed", ".gc/slack/publish-receipts/ed059699115662aeca1fcd2785a50a6348f8f6877ae71e9b6bb765319f9f2a51.json message=<now>.000501 thread= reply_to= delivered=true readback=confirmed", "uploads/", "uploads/answer.txt"}
)
