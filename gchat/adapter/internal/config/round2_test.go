package config

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/ack"
)

// With ACK_REACTION unset the adapter starts as at the PR's base, which reads
// ACK_SCOPE nowhere: any value starts, silently. Set, the scope is checked.
func TestScopeUncheckedWhileOff(t *testing.T) {
	env, _, dir := configFixture(t)
	t.Setenv("ACK_REACTION", "")
	t.Setenv("ACK_SCOPE", "")
	body, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, env, string(body)+"ACK_SCOPE=everyone\n")
	c, err := Load(env, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	options := c.Acknowledgement
	options.Log = func(s string) { lines = append(lines, s) }
	choice, err := ack.New(options)
	if err != nil {
		t.Fatalf("switch off, ACK_SCOPE=everyone: start refused: %v", err)
	}
	if len(lines) != 0 || choice.Enabled() || choice.All() {
		t.Fatalf("switch off: lines=%q enabled=%t all=%t", lines, choice.Enabled(), choice.All())
	}
	writeFixture(t, env, string(body)+"ACK_SCOPE=everyone\nACK_REACTION=eyes\n")
	if c, err = Load(env, "", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ack.New(c.Acknowledgement); err == nil || err.Error() != "ACK_SCOPE must be addressed or all" {
		t.Fatalf("switch on, ACK_SCOPE=everyone: %v", err)
	}
}

// With the switch on, the text the Jev API receives passes Slack's secret
// redaction first, then the 1,000-character cut, for every shared case.
func TestJevStateIsRedactedLikeSlack(t *testing.T) {
	data, err := os.ReadFile("../redact/testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Text, Want string
		TextParts        []string `json:"text_parts"`
	}
	if err := json.Unmarshal(data, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("fixtures: %d, %v", len(cases), err)
	}
	var mu sync.Mutex
	var states []string
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ State string }
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		states = append(states, request.State)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"answers":{"ack":{"choice":"option_001"}}}`)
	}))
	defer jev.Close()
	env, _, dir := configFixture(t)
	for _, key := range []string{"ACK_REACTION", "ACK_SCOPE", "JEV_API_KEY", "JEV_API_BASE_URL", "JEV_MODEL"} {
		t.Setenv(key, "")
	}
	body, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, env, string(body)+"ACK_REACTION=jev\nJEV_API_KEY=test-credential\nJEV_API_BASE_URL="+jev.URL+"\n")
	c, err := Load(env, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	choice, err := ack.New(c.Acknowledgement)
	if err != nil {
		t.Fatal(err)
	}
	matched := 0
	for _, tc := range cases {
		text := tc.Text
		if tc.TextParts != nil {
			text = strings.Join(tc.TextParts, "")
		}
		want := []rune(tc.Want)
		if len(want) > 1000 {
			want = want[:1000]
		}
		mu.Lock()
		states = nil
		mu.Unlock()
		_, how := choice.Choose(context.Background(), text, time.Now())
		mu.Lock()
		got := append([]string(nil), states...)
		mu.Unlock()
		if strings.TrimSpace(string(want)) == "" {
			if len(got) != 0 {
				t.Errorf("%s: blank text reached Jev", tc.Name)
			}
			continue
		}
		// Report by name and size only: the inputs are credential-shaped.
		if how != "jev" || len(got) != 1 || got[0] != string(want) {
			size := -1
			if len(got) == 1 {
				size = len([]rune(got[0]))
			}
			t.Errorf("%s: how=%s requests=%d state is not the redacted want (state %d chars, want %d)", tc.Name, how, len(got), size, len(want))
			continue
		}
		matched++
	}
	t.Logf("redacted like Slack: %d of %d shared cases", matched, len(cases))
}
