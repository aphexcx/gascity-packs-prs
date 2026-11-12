package ack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fixture = `{"reactions":[{"emoji":"👀","slack_name":"eyes","meaning":"Received.","adapter_may_place":true},{"emoji":"❤️","slack_name":"heart","meaning":"Welcome.","adapter_may_place":true},{"emoji":"🧪","slack_name":"fixture_arrival","meaning":"A fixture arrival.","adapter_may_place":true},{"emoji":"✅","slack_name":"fixture_claim","meaning":"Done.","adapter_may_place":false}]}`

func TestChoiceTieBreakPairs(t *testing.T) {
	for _, pair := range []struct {
		text  string
		names []string
	}{
		{"Can you find out what the plumber charged us last year?", []string{"eyes", "mag"}},
		{"We closed! Please send the invoice.", []string{"tada", "eyes"}},
	} {
		for _, name := range pair.names {
			var choice *Choice
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var b struct {
					State     string
					Questions map[string]struct {
						Instructions string
						Criteria     map[string]string
					}
				}
				if json.NewDecoder(r.Body).Decode(&b) != nil {
					t.Error("request malformed")
				}
				if b.State != pair.text || b.Questions["ack"].Instructions != Instructions {
					t.Error("tie-break request changed")
				}
				key := ""
				for _, offered := range pair.names {
					found := false
					for k, row := range choice.rows {
						if row.Name == offered && b.Questions["ack"].Criteria[k] == row.Meaning {
							found = true
							if row.Name == name {
								key = k
							}
						}
					}
					if !found {
						t.Error("pair not offered")
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"ack": map[string]string{"choice": key}}})
			}))
			var err error
			choice, err = New(Options{Mode: "jev", Key: "test-credential", Base: s.URL, Catalog: Catalog})
			if err != nil {
				t.Fatal(err)
			}
			row, how := choice.Choose(context.Background(), pair.text, time.Now())
			s.Close()
			if row.Name != name || how != "jev" {
				t.Fatal("fake choice was overridden")
			}
		}
	}
}

func TestChoiceCatalogAndRequest(t *testing.T) {
	for _, row := range []struct{ key, name, emoji string }{{"option_001", "eyes", "👀"}, {"option_002", "heart", "❤️"}, {"option_003", "fixture_arrival", "🧪"}} {
		t.Run(row.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer test-credential" {
					t.Error("request envelope mismatch")
				}
				var b struct {
					Model, State string
					Questions    map[string]struct {
						Type, Instructions string
						Criteria           map[string]string
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
					t.Error(err)
				}
				q := b.Questions["ack"]
				if b.Model != "jev-latest" || b.State != "A synthetic message" || len(b.Questions) != 1 || q.Type != "choice" || q.Instructions != Instructions || len(q.Criteria) != 3 || q.Criteria["option_003"] != "A fixture arrival." {
					t.Error("request body mismatch")
				}
				io.WriteString(w, `{"answers":{"ack":{"type":"choice","choice":"`+row.key+`"}}}`)
			}))
			defer s.Close()
			c, err := New(Options{Mode: "jev", Key: "test-credential", Base: s.URL, Catalog: []byte(fixture)})
			if err != nil {
				t.Fatal(err)
			}
			got, how := c.Choose(context.Background(), "A synthetic message", time.Now())
			if got.Name != row.name || got.Emoji != row.emoji || how != "jev" {
				t.Fatalf("wrong choice: %v %s", got, how)
			}
		})
	}
}

func TestChoiceFallbacksAndUnusableCatalog(t *testing.T) {
	for _, body := range []string{"", "{", `{"reactions":[]}`, `{"reactions":[{"emoji":"x","slack_name":"x","meaning":"x"}]}`, `{"reactions":[{"emoji":"x","slack_name":"x","meaning":"x","adapter_may_place":"true"}]}`} {
		for _, mode := range []string{"jev", "eyes"} {
			c, err := New(Options{Mode: mode, Catalog: []byte(body)})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := c.Choose(context.Background(), "synthetic", time.Now())
			if got.Name != "eyes" {
				t.Fatal("missing default")
			}
		}
		if _, err := New(Options{Mode: "mag", Catalog: []byte(body)}); err == nil || !strings.Contains(err.Error(), "ACK_REACTION") || !strings.Contains(err.Error(), "reactions.json") {
			t.Fatal("unusable file accepted")
		}
	}
	for _, mode := range []string{"auto", "model", "fixture_claim", "unknown"} {
		if _, err := New(Options{Mode: mode, Catalog: []byte(fixture)}); err == nil {
			t.Fatalf("accepted %s", mode)
		}
	}
	for _, code := range []int{200, 401, 422, 429, 529, 500} {
		for _, body := range []string{`{`, `{}`, `{"answers":{"ack":{"choice":"fixture_claim"}}}`, `{"answers":{"ack":{"choice":"✅"}}}`, `{"answers":{"ack":{"choice":"not_offered"}}}`} {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(code)
				io.WriteString(w, body)
			}))
			c, err := New(Options{Mode: "jev", Key: "test-credential", Base: s.URL, Catalog: []byte(fixture)})
			if err != nil {
				t.Fatal(err)
			}
			got, how := c.Choose(context.Background(), "synthetic", time.Now())
			s.Close()
			if got.Name != "eyes" || how != "default" || calls.Load() != 1 {
				t.Fatal("fallback was not one attempt")
			}
		}
	}
}

func TestChoiceNoTextNoKeyAndCharacterLimit(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if len([]rune(b["state"].(string))) != 1000 {
			t.Error("not 1000 characters")
		}
		io.WriteString(w, `{"answers":{"ack":{"choice":"option_001"}}}`)
	}))
	defer s.Close()
	c, _ := New(Options{Mode: "jev", Key: "test-credential", Base: s.URL, Catalog: []byte(fixture)})
	c.Choose(context.Background(), "", time.Now())
	c.Choose(context.Background(), strings.Repeat("界", 1200), time.Now())
	c, _ = New(Options{Mode: "jev", Base: s.URL, Catalog: []byte(fixture)})
	c.Choose(context.Background(), "synthetic", time.Now())
	if calls.Load() != 1 {
		t.Fatal("empty text or absent key called vendor")
	}
}

func TestChoiceDeadlineAndConcurrency(t *testing.T) {
	now := time.Now()
	tick := make(chan time.Time)
	var active, maximum atomic.Int32
	stop := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer s.Close()
	defer close(stop)
	c, _ := New(Options{Mode: "jev", Key: "test-credential", Base: s.URL, Catalog: []byte(fixture), Now: func() time.Time { return now }, After: func(time.Duration) <-chan time.Time { return tick }})
	done := make(chan string, 9)
	for i := 0; i < 9; i++ {
		go func() { r, _ := c.Choose(context.Background(), "synthetic", now); done <- r.Name }()
	}
	deadline := time.After(time.Second)
	for maximum.Load() < 4 {
		select {
		case <-deadline:
			t.Fatal("requests never started")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(tick)
	for i := 0; i < 9; i++ {
		select {
		case name := <-done:
			if name != "eyes" {
				t.Fatal(name)
			}
		case <-time.After(time.Second):
			t.Fatal("deadline ignored")
		}
	}
	if maximum.Load() > 4 {
		t.Fatal("more than four requests")
	}
}

func TestChoiceEndpointAndCanary(t *testing.T) {
	for _, base := range []string{"http://example.invalid", "https://user:secret@example.invalid", "https://example.invalid?key=secret", "invalid"} {
		_, err := New(Options{Mode: "jev", Key: "canary-must-stay-private", Base: base, Catalog: []byte(fixture)})
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "canary-must-stay-private") {
			t.Fatal("unsafe endpoint error")
		}
	}
}
