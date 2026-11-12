package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/neversilent"
	"gchat-adapter/internal/wire"
)

func TestNeverSilentReplayedCreateRetainsOriginalActivityTime(t *testing.T) {
	for _, media := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "caption"}[media], func(t *testing.T) {
			f := setup(t)
			original := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
			f.g.Configure(func(g *fakegoogle.Server) { g.Now = func() time.Time { return original } })
			now := original
			dir := t.TempDir()
			health := filepath.Join(dir, "health.json")
			posts := 0
			noticeOpts := neversilent.Options{Enabled: true, Path: filepath.Join(dir, "notices.json"), HealthFile: health, Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }}
			e, err := neversilent.New(noticeOpts)
			if err != nil {
				t.Fatal(err)
			}
			f.s.SetNeverSilent(e)
			client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				resp, err := http.DefaultTransport.RoundTrip(r)
				if err == nil && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
					f.s.opts.LedgerPath = filepath.Join(dir, "absent", "state.json")
				}
				return resp, err
			})}
			c, err := chat.New(context.Background(), client, f.cfg.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			f.s.SetClients(c, f.options.GC)
			var upload *ledger.SendUpload
			if media {
				file := filepath.Join(f.cfg.Endpoints.UploadRoot, "example.txt")
				if err = os.WriteFile(file, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				upload = &ledger.SendUpload{Path: file}
			}
			p := wire.PublishRequest{SessionID: "example-session", Conversation: conversation("spaces/room"), Text: "old reply words", IdempotencyKey: "original-reply"}
			if _, err = f.s.publish(context.Background(), p, upload); err == nil || f.count() != 1 {
				t.Fatalf("expected accepted message before ledger failure: %v count=%d", err, f.count())
			}
			l, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			opts := f.options
			opts.Ledger = l
			s, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.SetHealth(Health{State: "ready"})
			e, err = neversilent.New(noticeOpts)
			if err != nil {
				t.Fatal(err)
			}
			s.SetNeverSilent(e)
			now = time.Now()
			if err = e.Track(neversilent.Message{ID: "new-request", Conversation: "spaces/room", Created: now}); err != nil {
				t.Fatal(err)
			}
			if err = e.Result("new-request", "delivered", []string{"example-session"}); err != nil {
				t.Fatal(err)
			}
			if r, err := s.publish(context.Background(), p, upload); err != nil || !r.Delivered {
				t.Fatalf("replay failed: %+v %v", r, err)
			}
			if f.count() != 1 {
				t.Fatal("provider replay posted a duplicate")
			}
			now = now.Add(3 * time.Minute)
			stamp := now.Format(time.RFC3339)
			if err = os.WriteFile(health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err = e.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if posts != 1 {
				t.Fatal("old provider create suppressed a newer request")
			}
			if !l.Sends[p.IdempotencyKey].Parts[0].SentAt.Equal(original) {
				t.Fatal("primary receipt lost original provider creation time")
			}
		})
	}
}

func TestNeverSilentSessionPublishAndReactionSuppressHealthLine(t *testing.T) {
	for _, action := range []string{"publish", "top-level publish", "partial publish", "reaction", "unattributed", "notice"} {
		t.Run(action, func(t *testing.T) {
			f := setup(t)
			now := time.Now()
			dir := t.TempDir()
			health := filepath.Join(dir, "health.json")
			posts := 0
			e, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(dir, "notices.json"), HealthFile: health, Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }})
			if err != nil {
				t.Fatal(err)
			}
			f.s.SetNeverSilent(e)
			// Seed a real fake-Google thread without session attribution.
			seed := f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "example setup", IdempotencyKey: "example-seed"})
			thread := seed.Metadata["thread"]
			if err = e.Track(neversilent.Message{ID: seed.MessageID, Conversation: "spaces/room", Thread: thread, Created: now.Add(-time.Second)}); err != nil {
				t.Fatal(err)
			}
			if err = e.Result(seed.MessageID, "delivered", []string{"example-session"}); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "top-level publish":
				f.send(t, wire.PublishRequest{SessionID: "example-session", Conversation: conversation("spaces/room"), Text: "example top-level reply", IdempotencyKey: "example-top-level"})
			case "partial publish":
				httpClient := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Query().Get("requestId"), "-2") {
						return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"quota"}}`)), Request: r}, nil
					}
					return http.DefaultTransport.RoundTrip(r)
				})}
				c, err := chat.New(context.Background(), httpClient, f.cfg.Endpoints)
				if err != nil {
					t.Fatal(err)
				}
				f.s.SetClients(c, f.options.GC)
				answer := f.send(t, wire.PublishRequest{SessionID: "example-session", Conversation: conversation("spaces/room"), Text: strings.Repeat("x", 33000), ReplyToMessageID: thread, IdempotencyKey: "example-partial"})
				if answer.Delivered {
					t.Fatal("expected partial send failure")
				}
			case "publish":
				f.send(t, wire.PublishRequest{SessionID: "example-session", Conversation: conversation("spaces/room"), Text: "example reply", ReplyToMessageID: thread, IdempotencyKey: "example-reply"})
			case "reaction", "unattributed":
				session := ""
				if action == "reaction" {
					session = "example-session"
				}
				code, _, _ := f.call(t, "/react", map[string]any{"message": seed.MessageID, "emoji": "eyes", "session_id": session})
				if code != 200 {
					t.Fatalf("react status=%d", code)
				}
			case "notice":
				c, _ := f.s.clients()
				if _, _, err = c.CreateNotice(context.Background(), "spaces/room", neversilent.DownLine, thread, true); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(3 * time.Minute)
			stamp := now.Format(time.RFC3339)
			if err = os.WriteFile(health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err = e.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 1
			if action == "publish" || action == "top-level publish" || action == "partial publish" || action == "reaction" {
				want = 0
			}
			if posts != want {
				t.Fatalf("posts=%d want=%d", posts, want)
			}
			_, raw, _ := f.call(t, "/status", nil)
			var status map[string]json.RawMessage
			if err = json.Unmarshal(raw, &status); err != nil {
				t.Fatal(err)
			}
			view := string(status["never_silent"])
			if !strings.Contains(view, `"enabled":true`) || strings.Contains(view, "spaces/room") || strings.Contains(view, "example-session") {
				t.Fatal(view)
			}
		})
	}
}

func TestNeverSilentFirstNoticeResolvesUncachedSpaceType(t *testing.T) {
	f := setup(t)
	seed := f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "example seed", IdempotencyKey: "example-seed"})
	thread := seed.Metadata["thread"]
	if err := f.s.update(context.Background(), func(l *ledger.Ledger) error { l.Spaces["spaces/room"].ChatType = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PostNotice(context.Background(), neversilent.Message{Conversation: "spaces/room", Thread: thread}, neversilent.DownLine); err != nil {
		t.Fatal(err)
	}
	found := false
	f.g.Configure(func(g *fakegoogle.Server) {
		for _, m := range g.Messages {
			if m.Text == neversilent.DownLine {
				found = true
				if m.Thread == nil || m.Thread.Name != thread {
					t.Fatal("notice left the owner thread")
				}
			}
		}
	})
	if !found {
		t.Fatal("notice missing")
	}
}

func TestNeverSilentSessionActDuringSpaceLookupCancelsNotice(t *testing.T) {
	f := setup(t)
	entered, release := make(chan struct{}), make(chan struct{})
	c, err := chat.New(context.Background(), &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/spaces") {
			close(entered)
			<-release
		}
		return http.DefaultTransport.RoundTrip(r)
	})}, f.cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetClients(c, f.options.GC)
	now := time.Now()
	e, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(t.TempDir(), "state.json"), Now: func() time.Time { return now }, Post: f.s.PostNotice})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Track(neversilent.Message{ID: "example", Conversation: "spaces/room", Created: now.Add(-4 * time.Minute), Targets: []string{"mayor"}}); err != nil {
		t.Fatal(err)
	}
	if err = e.StopAt("example", false, now.Add(-4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Poll(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("notice did not reach space lookup")
	}
	if err = e.Act("spaces/room", "", "mayor", now); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	f.g.Configure(func(g *fakegoogle.Server) {
		for _, m := range g.Messages {
			if m.Text == neversilent.DownLine {
				t.Fatal("notice posted after session act during lookup")
			}
		}
	})
}
