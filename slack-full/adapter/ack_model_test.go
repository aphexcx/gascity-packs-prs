package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseAckEmojiReply(t *testing.T) {
	cases := []struct {
		reply string
		want  string
		ok    bool
	}{
		{"tada", "tada", true},
		{":tada:", "tada", true},
		{"  Pray\n", "pray", true},
		{":chart_with_upwards_trend:", "chart_with_upwards_trend", true},
		{"tada.", "tada", true},
		{"I would pick :fire: here", "fire", true},
		{":fire: :fire:", "fire", true},
		{":fire: or :heart:", "", false},
		{"not_an_emoji", "", false},
		{":not_an_emoji:", "", false},
		{"I cannot choose an emoji for this", "", false},
		{"🎉", "", false},
		{"", "", false},
		{"   \n", "", false},
		{"::", "", false},
	}
	for _, c := range cases {
		got, ok := parseAckEmojiReply(c.reply)
		if got != c.want || ok != c.ok {
			t.Errorf("parseAckEmojiReply(%q) = (%q, %v), want (%q, %v)", c.reply, got, ok, c.want, c.ok)
		}
	}
}

func TestAckAllowedEmojiAreStandardSlackNames(t *testing.T) {
	for _, e := range ackAllowedEmoji {
		if !slackIsStandardEmojiName(e) {
			t.Errorf("%q is not a standard Slack emoji name", e)
		}
	}
	// every emoji the keyword picker can return must be pickable by the model too
	for _, r := range ackRules {
		if !ackAllowedSet[r.emoji] {
			t.Errorf("keyword emoji %q missing from the model's allowed list", r.emoji)
		}
	}
}

func TestAckModelUserContent(t *testing.T) {
	got := ackModelUserContent("see attached", []slackFile{{Name: "lease.pdf", MIMEType: "application/pdf"}}, "ops")
	for _, want := range []string{"#ops", "lease.pdf (application/pdf)", "Message: see attached"} {
		if !strings.Contains(got, want) {
			t.Errorf("content %q missing %q", got, want)
		}
	}
	long := ackModelUserContent(strings.Repeat("a", 5000), nil, "")
	if len(long) > ackModelMaxText+len("Message: ")+4 {
		t.Errorf("text not clipped: %d bytes", len(long))
	}
}

func ackTestConfig(t *testing.T, url string, timeout time.Duration) config {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("sk-test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config{
		ackModelBaseURL: url,
		ackModelName:    "teale/auto",
		ackModelKeyFile: keyFile,
		ackModelTimeout: timeout,
	}
}

func chatReply(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": content}}},
	})
	return string(b)
}

func TestPickAckEmojiModelUsesModelReply(t *testing.T) {
	var gotAuth, gotModel string
	var gotTemp float64
	var gotMax int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var req ackChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel, gotTemp, gotMax = req.Model, req.Temperature, req.MaxTokens
		w.Write([]byte(chatReply(":rocket:")))
	}))
	defer srv.Close()
	cfg := ackTestConfig(t, srv.URL, 2*time.Second)
	// keyword pick would say "eyes"; the model says rocket
	if got := pickAckEmojiModel(cfg, "launching tomorrow-ish", nil, "general"); got != "rocket" {
		t.Errorf("got %q, want rocket", got)
	}
	if gotAuth != "Bearer sk-test-secret" || gotModel != "teale/auto" || gotTemp != 0 || gotMax != ackModelMaxTokens {
		t.Errorf("request: auth=%q model=%q temp=%v max=%d", gotAuth, gotModel, gotTemp, gotMax)
	}
}

func TestPickAckEmojiModelFallsBack(t *testing.T) {
	// "the site is broken again" -> wrench via the keyword table
	const text = "the site is broken again"
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		w.Write([]byte(chatReply("tada")))
	}))
	defer slow.Close()
	status500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer status500.Close()
	emptyContent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatReply("")))
	}))
	defer emptyContent.Close()
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatReply("no idea, sorry")))
	}))
	defer junk.Close()
	badJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>gateway</html>"))
	}))
	defer badJSON.Close()

	cases := map[string]config{
		"timeout":       ackTestConfig(t, slow.URL, 100*time.Millisecond),
		"http 500":      ackTestConfig(t, status500.URL, time.Second),
		"empty content": ackTestConfig(t, emptyContent.URL, time.Second),
		"junk reply":    ackTestConfig(t, junk.URL, time.Second),
		"bad json":      ackTestConfig(t, badJSON.URL, time.Second),
		"unreachable":   ackTestConfig(t, "http://127.0.0.1:1", time.Second),
	}
	noKey := ackTestConfig(t, junk.URL, time.Second)
	noKey.ackModelKeyFile = ""
	cases["no key file"] = noKey
	missingKey := ackTestConfig(t, junk.URL, time.Second)
	missingKey.ackModelKeyFile = filepath.Join(t.TempDir(), "absent")
	cases["missing key file"] = missingKey

	for name, cfg := range cases {
		start := time.Now()
		if got := pickAckEmojiModel(cfg, text, nil, "ops"); got != "wrench" {
			t.Errorf("%s: got %q, want fallback wrench", name, got)
		}
		if name == "timeout" && time.Since(start) > time.Second {
			t.Errorf("timeout case took %s, expected ~100ms", time.Since(start))
		}
	}
}

func TestPickAckEmojiModelSendsOneRequest(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer srv.Close()
	pickAckEmojiModel(ackTestConfig(t, srv.URL, time.Second), "hi", nil, "")
	if n != 1 {
		t.Errorf("requests = %d, want 1 (no retries)", n)
	}
}

func TestAckModelConfig(t *testing.T) {
	base := map[string]string{
		"SLACK_WORKSPACE_ID": "T1", "SLACK_BOT_TOKEN": "xoxb-1", "SLACK_SIGNING_SECRET": "s", "GC_CITY_NAME": "c",
	}
	load := func(extra map[string]string) (config, error) {
		return loadConfigFromLookup(func(k string) (string, bool) {
			if v, ok := extra[k]; ok {
				return v, true
			}
			v, ok := base[k]
			return v, ok
		})
	}
	cfg, err := load(map[string]string{"ACK_REACTION": "model"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ackReaction != "model" || cfg.ackModelBaseURL != "https://gateway.teale.com/v1" ||
		cfg.ackModelName != "teale/auto" || cfg.ackModelTimeout != 6*time.Second || cfg.ackModelKeyFile != "" {
		t.Errorf("defaults wrong: %+v", cfg)
	}
	cfg, err = load(map[string]string{
		"ACK_MODEL_BASE_URL": "http://x/v1", "ACK_MODEL_NAME": "m", "ACK_MODEL_API_KEY_FILE": "/k", "ACK_MODEL_TIMEOUT_MS": "2500",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ackModelBaseURL != "http://x/v1" || cfg.ackModelName != "m" || cfg.ackModelKeyFile != "/k" || cfg.ackModelTimeout != 2500*time.Millisecond {
		t.Errorf("overrides wrong: %+v", cfg)
	}
	for _, bad := range []string{"abc", "0", "-5"} {
		if _, err := load(map[string]string{"ACK_MODEL_TIMEOUT_MS": bad}); err == nil {
			t.Errorf("ACK_MODEL_TIMEOUT_MS=%q accepted", bad)
		}
	}
}
