package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegoogle"
	googlechat "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *testClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *testClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
	return nil
}

func useTestClock(c *Client) *testClock {
	f := &testClock{now: time.Now()}
	c.timing = timing{now: f.Now, sleep: f.Sleep}
	c.limits.timing = c.timing
	return f
}

func TestRetryBudgetUsesClock(t *testing.T) {
	c := &Client{}
	f := useTestClock(c)
	c.retryBound = 5 * time.Second
	start := f.Now()
	calls := 0
	err := c.retry(context.Background(), true, func(context.Context) error {
		calls++
		return &googleapi.Error{Code: 429, Header: http.Header{"Retry-After": []string{"2"}}}
	})
	kind, hint := Classify(err)
	if calls != 3 || f.Now().Sub(start) != 4*time.Second || kind != "rate_limited" || hint != 2*time.Second || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("calls=%d elapsed=%v kind=%s hint=%v err=%v", calls, f.Now().Sub(start), kind, hint, err)
	}
}

func TestInjectedRetryBudgetIgnoresWallClockElapsed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &Client{retryBound: 5 * time.Second}
		clock := useTestClock(c)
		start := clock.Now()
		calls := 0
		err := c.retry(context.Background(), true, func(context.Context) error {
			calls++
			if calls == 1 {
				// Real elapsed time must not shorten the injected backoff budget.
				time.Sleep(3 * time.Second)
				return &googleapi.Error{Code: 429, Header: http.Header{"Retry-After": []string{"3"}}}
			}
			return nil
		})
		if err != nil || calls != 2 || clock.Now().Sub(start) != 3*time.Second {
			t.Fatalf("calls=%d fake elapsed=%v err=%v", calls, clock.Now().Sub(start), err)
		}
	})
}

func TestLimiterUsesClock(t *testing.T) {
	c := &Client{}
	f := useTestClock(c)
	start := f.Now()
	for i := 0; i < 3; i++ {
		if err := c.limits.WaitWrite(context.Background(), "spaces/room"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 6; i++ {
		if err := c.limits.WaitReaction(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := f.Now().Sub(start); elapsed != 3*time.Second {
		t.Fatalf("elapsed = %v", elapsed)
	}
}

type contextBody struct {
	ctx context.Context
	io.ReadCloser
}

type slowBody struct {
	contextBody
	once sync.Once
}

func (b *slowBody) Read(p []byte) (int, error) {
	b.once.Do(func() { time.Sleep(31 * time.Second) })
	return b.contextBody.Read(p)
}

func TestMediaTransferUsesCallerDeadline(t *testing.T) {
	for _, upload := range []bool{true, false} {
		t.Run(fmt.Sprintf("upload=%v", upload), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "media")
				if upload {
					if err := os.WriteFile(path, []byte("media"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				h := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
					if upload {
						body := &slowBody{contextBody: contextBody{ctx: r.Context(), ReadCloser: r.Body}}
						defer body.Close()
						if _, err := io.Copy(io.Discard, body); err != nil {
							return nil, err
						}
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"attachmentDataRef":{"resourceName":"spaces/room/attachments/1"}}`)), Request: r}, nil
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: -1, Body: &slowBody{contextBody: contextBody{ctx: r.Context(), ReadCloser: io.NopCloser(strings.NewReader("media"))}}, Request: r}, nil
				})}
				c, err := New(context.Background(), h, config.Endpoints{UploadRoot: dir, MediaDir: dir})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if upload {
					if _, err := c.UploadAttachment(ctx, "spaces/room", path); err != nil {
						t.Fatal(err)
					}
				} else if n, err := c.DownloadMedia(ctx, "spaces/room/attachments/1", path, 10); err != nil || n != 5 {
					t.Fatalf("download: %d %v", n, err)
				}
			})
		})
	}
}

func (b contextBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.ReadCloser.Read(p)
}

func TestDownloadStreamingContextAndUnknownLength(t *testing.T) {
	_, s, dir := fixture(t)
	resource := "spaces/room/attachments/stream"
	s.Media[resource] = []byte("streaming data")
	httpClient := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(r)
		if err == nil {
			response.ContentLength = -1
			response.Body = contextBody{ctx: r.Context(), ReadCloser: response.Body}
		}
		return response, err
	})}
	c, err := New(context.Background(), httpClient, config.Endpoints{ChatAPIBase: s.URL(), MediaDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.DownloadMedia(context.Background(), resource, filepath.Join(dir, "complete"), 14); err != nil || n != 14 {
		t.Fatalf("streamed download: %d %v", n, err)
	}
	dest := filepath.Join(dir, "oversize")
	n, err := c.DownloadMedia(context.Background(), resource, dest, 5)
	if n != 5 || err == nil {
		t.Fatalf("unbounded download: %d %v", n, err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial download remains: %v", err)
	}
}

func fixture(t *testing.T) (*Client, *fakegoogle.Server, string) {
	t.Helper()
	s := fakegoogle.New(t, nil)
	s.Spaces["spaces/room"] = &googlechat.Space{Name: "spaces/room", SpaceType: "SPACE"}
	s.Spaces["spaces/dm"] = &googlechat.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(context.Background(), http.DefaultClient, config.Endpoints{ChatAPIBase: s.URL(), MediaDir: dir, UploadRoot: dir})
	if err != nil {
		t.Fatal(err)
	}
	clock := useTestClock(c)
	s.Now = clock.Now
	return c, s, dir
}

func TestCreateMessageIdempotencyAndThreading(t *testing.T) {
	c, s, _ := fixture(t)
	ctx := context.Background()
	name, thread, err := c.CreateMessage(ctx, "spaces/room", "hello", "spaces/room/threads/gone", "same-id", true)
	if err != nil || name == "" || thread == "" || thread == "spaces/room/threads/gone" {
		t.Fatalf("create: %q %q %v", name, thread, err)
	}
	name2, thread2, err := c.CreateMessage(ctx, "spaces/room", "hello", "", "same-id", true)
	if err != nil || name2 != name || thread2 != thread {
		t.Fatalf("retry: %q %q %v", name2, thread2, err)
	}
	s.Configure(func(s *fakegoogle.Server) {
		if len(s.Messages) != 1 {
			t.Errorf("messages = %d", len(s.Messages))
		}
	})
	_, replyThread, err := c.CreateMessage(ctx, "spaces/room", "reply", thread, "reply", true)
	if err != nil || replyThread != thread {
		t.Fatalf("named reply: %q %v", replyThread, err)
	}
	_, dmThread, err := c.CreateMessage(ctx, "spaces/dm", "reply", "spaces/dm/threads/existing", "dm", false)
	if err != nil || dmThread != "spaces/dm/threads/existing" {
		t.Fatalf("dm reply: %q %v", dmThread, err)
	}
	got, err := c.GetMessage(ctx, name)
	if err != nil || got.Text != "hello" {
		t.Fatalf("get: %+v %v", got, err)
	}
}

func TestPaginationAndFilters(t *testing.T) {
	c, s, _ := fixture(t)
	now := time.Now().UTC()
	const updated = "google.workspace.chat.message.v1.updated"
	const deleted = "google.workspace.chat.message.v1.deleted"
	s.PageSize = 1
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("spaces/room/messages/%d", i)
		thread := "spaces/room/threads/a"
		if i == 4 {
			thread = "spaces/room/threads/b"
		}
		s.Messages[name] = &googlechat.Message{Name: name, CreateTime: now.Add(time.Duration(i-5) * time.Minute).Format(time.RFC3339Nano), Thread: &googlechat.Thread{Name: thread}}
		typ := updated
		if i == 3 {
			typ = deleted
		}
		if i == 4 {
			typ = "google.workspace.chat.reaction.v1.created"
		}
		s.SpaceEvents["spaces/room"] = append(s.SpaceEvents["spaces/room"], &googlechat.SpaceEvent{Name: fmt.Sprint(i), EventType: typ, EventTime: now.Add(time.Duration(i-5) * time.Minute).Format(time.RFC3339Nano)})
	}
	spaces, err := c.ListSpaces(context.Background())
	if err != nil || len(spaces) != 2 {
		t.Fatalf("spaces: %v %v", spaces, err)
	}
	var messages []*googlechat.Message
	err = c.ListMessages(context.Background(), "spaces/room", now.Add(-5*time.Minute), "spaces/room/threads/a", func(page []*googlechat.Message) error { messages = append(messages, page...); return nil })
	if err != nil || len(messages) != 3 || messages[0].Name != "spaces/room/messages/1" {
		t.Fatalf("messages: %v %v", messages, err)
	}
	var events []*googlechat.SpaceEvent
	err = c.ListSpaceEvents(context.Background(), "spaces/room", now.Add(-5*time.Minute), []string{updated, deleted}, func(page []*googlechat.SpaceEvent) error { events = append(events, page...); return nil })
	if err != nil || len(events) != 3 || events[0].Name != "1" {
		t.Fatalf("events: %v %v", events, err)
	}
	stop := errors.New("stop paging")
	for _, run := range []func() error{
		func() error {
			return c.ListMessages(context.Background(), "spaces/room", time.Time{}, "", func([]*googlechat.Message) error { return stop })
		},
		func() error {
			return c.ListSpaceEvents(context.Background(), "spaces/room", time.Time{}, []string{updated}, func([]*googlechat.SpaceEvent) error { return stop })
		},
	} {
		if err := run(); !errors.Is(err, stop) {
			t.Fatalf("callback error = %v", err)
		}
	}
}

func TestListMessagesUsesDefaultAscendingOrder(t *testing.T) {
	_, s, _ := fixture(t)
	h := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if order := r.URL.Query().Get("orderBy"); order != "" {
			t.Errorf("expected documented default order, got %q", order)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	c, err := New(context.Background(), h, config.Endpoints{ChatAPIBase: s.URL()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ListMessages(context.Background(), "spaces/room", time.Time{}, "", func([]*googlechat.Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestReactionsDeleteOnlyOwnAcrossPages(t *testing.T) {
	c, s, _ := fixture(t)
	message := "spaces/room/messages/seed"
	s.Messages[message] = &googlechat.Message{Name: message}
	s.PageSize = 1
	other := message + "/reactions/0"
	s.Reactions[other] = &googlechat.Reaction{Name: other, Emoji: &googlechat.Emoji{Unicode: "👍"}, User: &googlechat.User{Name: "users/other"}}
	if err := c.DeleteReaction(context.Background(), message, "👍"); err == nil {
		t.Fatal("deleted without knowing caller identity")
	}
	mine, err := c.CreateReaction(context.Background(), message, "👍")
	if err != nil || mine == "" {
		t.Fatalf("create reaction: %q %v", mine, err)
	}
	keep, err := c.CreateReaction(context.Background(), message, "🙂")
	if err != nil {
		t.Fatal(err)
	}
	// A restarted client can remove reactions without first creating one.
	c, err = New(context.Background(), http.DefaultClient, config.Endpoints{ChatAPIBase: s.URL()})
	if err != nil {
		t.Fatal(err)
	}
	useTestClock(c)
	if err := c.SetIdentityUserID(fakegoogle.UserID); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteReaction(context.Background(), message, "👍"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteReaction(context.Background(), message, "👍"); err != nil {
		t.Fatalf("repeat removal: %v", err)
	}
	s.Configure(func(s *fakegoogle.Server) {
		if s.Reactions[mine] != nil || s.Reactions[other] == nil || s.Reactions[keep] == nil {
			t.Errorf("wrong reactions remaining: %v", s.Reactions)
		}
	})
}

func TestIdentityRejectsAliases(t *testing.T) {
	for _, id := range []string{"", "users/", "users/me", "users/app", "users/example", "users/example@example.com", "users/123/4", "users/１２３", "users/+123", "users/123\n"} {
		c := &Client{}
		if err := c.SetIdentityUserID(id); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
	c := &Client{}
	if err := c.SetIdentityUserID("users/123456789012345678901"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetIdentityUserID("users/456"); err == nil {
		t.Fatal("allowed identity change")
	}
}

// This test and its subtests must remain serial: slog.SetDefault changes the
// process-wide logger. Cleanup restores it before any parallel tests resume.
func TestCreateIdentityLearningIsBestEffort(t *testing.T) {
	for _, reaction := range []bool{false, true} {
		for _, mismatch := range []bool{false, true} {
			t.Run(fmt.Sprintf("reaction=%v/mismatch=%v", reaction, mismatch), func(t *testing.T) {
				_, s, _ := fixture(t)
				const message = "spaces/room/messages/seed"
				s.Messages[message] = &googlechat.Message{Name: message}
				var logs bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(previous) })
				observed := "users/example"
				initial := ""
				if mismatch {
					initial, observed = "users/111", "users/222"
				}
				requests := 0
				h := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					response, err := http.DefaultTransport.RoundTrip(r)
					if err != nil {
						return nil, err
					}
					defer response.Body.Close()
					var body map[string]any
					if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
						return nil, err
					}
					field := "sender"
					if reaction {
						field = "user"
					}
					body[field] = map[string]string{"name": observed}
					data, err := json.Marshal(body)
					if err != nil {
						return nil, err
					}
					response.Body = io.NopCloser(bytes.NewReader(data))
					response.ContentLength = int64(len(data))
					response.Header.Set("Content-Length", fmt.Sprint(len(data)))
					return response, nil
				})}
				c, err := New(context.Background(), h, config.Endpoints{ChatAPIBase: s.URL()})
				if err != nil {
					t.Fatal(err)
				}
				useTestClock(c)
				if initial != "" {
					if err := c.SetIdentityUserID(initial); err != nil {
						t.Fatal(err)
					}
				}
				for i := 0; i < 2; i++ {
					var name string
					if reaction {
						name, err = c.CreateReaction(context.Background(), message, "👍")
					} else {
						var thread string
						name, thread, err = c.CreateMessage(context.Background(), "spaces/room", "hello", "", fmt.Sprint(i), true)
						if thread == "" {
							t.Error("successful message lost its thread")
						}
					}
					if err != nil || name == "" {
						t.Errorf("stored create: name=%q err=%v", name, err)
					}
					s.Configure(func(s *fakegoogle.Server) {
						if reaction && s.Reactions[name] == nil || !reaction && s.Messages[name] == nil {
							t.Errorf("create %q was not stored", name)
						}
					})
				}
				if got := c.IdentityUserID(); got != initial {
					t.Errorf("identity=%q, want %q", got, initial)
				}
				if mismatch {
					// Re-supplying the first ID cannot clear a credential conflict.
					if err := c.SetIdentityUserID(initial); err != nil {
						t.Fatal(err)
					}
					err := c.DeleteReaction(context.Background(), message, "👍")
					kind, _ := Classify(err)
					if kind != "permanent" || !strings.Contains(err.Error(), "mismatch") {
						t.Errorf("delete after mismatch: kind=%s err=%v", kind, err)
					}
					var record struct{ Level, Msg string }
					if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil || record.Level != "WARN" || !strings.Contains(record.Msg, "mismatch") {
						t.Errorf("want exactly one WARN for mismatch, got %q (%v)", logs.String(), err)
					}
				} else if logs.Len() != 0 {
					t.Errorf("non-canonical sender logged: %s", logs.String())
				}
				if requests != 2 {
					t.Errorf("requests=%d, want only two creates", requests)
				}
			})
		}
	}
}

func TestCreateMessageLearnsIdentityForReactionRemoval(t *testing.T) {
	c, s, _ := fixture(t)
	name, _, err := c.CreateMessage(context.Background(), "spaces/room", "hello", "", "identity", true)
	if err != nil {
		t.Fatal(err)
	}
	// No reaction has been created by this client. A pre-existing own reaction
	// must already be removable after a successful message response.
	const own = fakegoogle.UserID
	reaction := name + "/reactions/own"
	s.Configure(func(s *fakegoogle.Server) {
		s.Reactions[reaction] = &googlechat.Reaction{Name: reaction, User: &googlechat.User{Name: own}, Emoji: &googlechat.Emoji{Unicode: "👍"}}
	})
	if err := c.DeleteReaction(context.Background(), name, "👍"); err != nil {
		t.Fatal(err)
	}
	s.Configure(func(s *fakegoogle.Server) {
		if s.Reactions[reaction] != nil {
			t.Error("own reaction remains")
		}
		// Idempotency replays can omit the sender; preserve the learned identity.
		s.Messages[name].Sender = nil
	})
	if _, _, err := c.CreateMessage(context.Background(), "spaces/room", "hello", "", "identity", true); err != nil {
		t.Fatal(err)
	}
	if c.IdentityUserID() != own {
		t.Fatalf("identity = %q", c.IdentityUserID())
	}
}

func TestUploadMessageAndBoundedDownload(t *testing.T) {
	c, _, dir := fixture(t)
	path := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(path, []byte("hello media"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := c.UploadAttachment(context.Background(), "spaces/room", path)
	if err != nil || ref == nil {
		t.Fatalf("upload: %v %v", ref, err)
	}
	name, _, err := c.CreateMessageWithAttachments(context.Background(), "spaces/room", "file", "", "upload-id", true, []*googlechat.Attachment{{AttachmentDataRef: ref}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.GetMessage(context.Background(), name)
	if err != nil || len(m.Attachment) != 1 || m.Attachment[0].AttachmentDataRef.ResourceName != ref.ResourceName {
		t.Fatalf("attachment: %v %v", m, err)
	}
	for _, limit := range []int64{11, 12, 5, 0} {
		dest := filepath.Join(dir, fmt.Sprintf("download-%d", limit))
		n, err := c.DownloadMedia(context.Background(), ref.ResourceName, dest, limit)
		if limit < 11 {
			if err == nil || n > limit {
				t.Fatalf("limit %d: n=%d err=%v", limit, n, err)
			}
			if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed download left file: %v", statErr)
			}
		} else {
			data, readErr := os.ReadFile(dest)
			if err != nil || n != 11 || readErr != nil || string(data) != "hello media" {
				t.Fatalf("download: %d %q %v %v", n, data, err, readErr)
			}
		}
	}
	if _, err := c.DownloadMedia(context.Background(), ref.ResourceName, path, 20); err == nil {
		t.Fatal("overwrote existing destination")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "hello media" {
		t.Fatal("source changed")
	}
}

func TestMediaRejectsSymlinksAndEscapes(t *testing.T) {
	c, s, dir := fixture(t)
	s.Media["spaces/room/attachments/media"] = []byte("data")
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "out"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(outside, "out"), filepath.Join(dir, "link", "out")} {
		if _, err := c.DownloadMedia(context.Background(), "spaces/room/attachments/media", path, 20); err == nil {
			t.Fatalf("unsafe download %s", path)
		}
		if _, err := c.UploadAttachment(context.Background(), "spaces/room", path); err == nil {
			t.Fatalf("unsafe upload %s", path)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "file"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadAttachment(context.Background(), "spaces/room", filepath.Join(dir, "alias")); err == nil {
		t.Fatal("uploaded symlink")
	}
}

func TestMediaRootAllowsSymlinkedParent(t *testing.T) {
	c, _, dir := fixture(t)
	parent := t.TempDir()
	alias := filepath.Join(parent, "parent-link")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(alias, "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	c.ep.UploadRoot, c.ep.MediaDir = root, root
	path := filepath.Join(root, "source")
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := c.UploadAttachment(context.Background(), "spaces/room", path)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.DownloadMedia(context.Background(), ref.ResourceName, filepath.Join(root, "dest"), 10); err != nil || n != 4 {
		t.Fatalf("download: %d %v", n, err)
	}
	// A symlink below the root is refused even when its target stays inside it.
	if err := os.Symlink(root, filepath.Join(root, "inside-link")); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"inside-link/source", "./../root/source"} {
		if _, err := c.UploadAttachment(context.Background(), "spaces/room", root+"/"+suffix); err == nil {
			t.Fatalf("accepted %s", suffix)
		}
	}
	for _, suffix := range []string{"inside-link/new", "./../root/new"} {
		if _, err := c.DownloadMedia(context.Background(), ref.ResourceName, root+"/"+suffix, 10); err == nil {
			t.Fatalf("accepted %s", suffix)
		}
	}
	// Resolving only symlinked ancestors is insufficient: the root can be a link.
	rootLink := filepath.Join(parent, "root-link")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Fatal(err)
	}
	c.ep.UploadRoot, c.ep.MediaDir = rootLink, rootLink
	ref, err = c.UploadAttachment(context.Background(), "spaces/room", filepath.Join(rootLink, "source"))
	if err != nil {
		t.Fatalf("upload through root symlink: %v", err)
	}
	if n, err := c.DownloadMedia(context.Background(), ref.ResourceName, filepath.Join(rootLink, "root-link-dest"), 10); err != nil || n != 4 {
		t.Fatalf("download through root symlink: %d %v", n, err)
	}
}

func TestMediaRejectsDotDotWithinRoot(t *testing.T) {
	c, _, dir := fixture(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadAttachment(context.Background(), "spaces/room", dir+"/sub/../source"); err == nil {
		t.Fatal("accepted dot-dot path")
	}
}

func TestUploadLimitStillAppliesAfterQuotaRetry(t *testing.T) {
	_, s, dir := fixture(t)
	path := filepath.Join(dir, "growing.txt")
	if err := os.WriteFile(path, []byte("small"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	httpClient := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			if err := os.WriteFile(path, []byte("now too big"), 0600); err != nil {
				t.Error(err)
			}
			return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"quota"}}`)), Request: r}, nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	c, err := New(context.Background(), httpClient, config.Endpoints{ChatAPIBase: s.URL(), UploadRoot: dir, MediaMaxBytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	useTestClock(c)
	_, err = c.UploadAttachment(context.Background(), "spaces/room", path)
	if err == nil || !strings.Contains(err.Error(), "growing.txt") {
		t.Fatalf("oversize upload: %v", err)
	}
	if calls != 1 {
		t.Errorf("requests=%d, want only the attempt before growth", calls)
	}
	s.Configure(func(s *fakegoogle.Server) {
		if len(s.Media) != 0 {
			t.Errorf("oversized media accepted: %v", s.Media)
		}
	})
}

func TestLargeUploadQuotaPolicy(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		t.Run(fmt.Sprintf("exhausted=%v", exhausted), func(t *testing.T) {
			_, s, dir := fixture(t)
			data := bytes.Repeat([]byte("x"), 17<<20)
			path := filepath.Join(dir, "large.bin")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			var clock *testClock
			var first time.Time
			attempts := 0
			h := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				// Inject at the transfer request, not at a resumable session's
				// metadata request. This catches the SDK's hidden chunk retries.
				if r.URL.Query().Get("uploadType") == "resumable" {
					return http.DefaultTransport.RoundTrip(r)
				}
				attempts++
				if attempts == 1 {
					first = clock.Now()
				} else if clock.Now().Sub(first) < 2*time.Second {
					t.Error("transfer retried before Retry-After")
				}
				if exhausted || attempts == 1 {
					// RoundTrippers own and close each request body, even on errors.
					r.Body.Close()
					return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"2"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"quota"}}`)), Request: r}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})}
			c, err := New(context.Background(), h, config.Endpoints{ChatAPIBase: s.URL(), UploadRoot: dir})
			if err != nil {
				t.Fatal(err)
			}
			clock = useTestClock(c)
			c.retryBound = 3 * time.Second
			if exhausted {
				c.retryBound = 500 * time.Millisecond
			}
			// The injected wait bound drives the assertion; a generous real deadline
			// is only a guard if SDK chunk retries are accidentally reintroduced.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ref, err := c.UploadAttachment(ctx, "spaces/room", path)
			if exhausted {
				kind, hint := Classify(err)
				if kind != "rate_limited" || hint != 2*time.Second || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("quota deadline: kind=%s hint=%v err=%v", kind, hint, err)
				}
				if attempts != 1 {
					t.Fatalf("unfit hint retried %d times", attempts)
				}
				return
			}
			if err != nil || ref == nil {
				t.Fatalf("upload: %v %v", ref, err)
			}
			if attempts != 2 {
				t.Fatalf("transfer attempts=%d, want 2 single requests", attempts)
			}
			s.Configure(func(s *fakegoogle.Server) {
				if !bytes.Equal(s.Media[ref.ResourceName], data) {
					t.Error("uploaded bytes differ")
				}
			})
		})
	}
}

func TestLargeUploadClientByteLimit(t *testing.T) {
	for _, growDuringTransfer := range []bool{false, true} {
		t.Run(fmt.Sprintf("grow=%v", growDuringTransfer), func(t *testing.T) {
			_, s, dir := fixture(t)
			const size = 17 << 20
			path := filepath.Join(dir, "large-limit.bin")
			initialSize := size
			if growDuringTransfer {
				initialSize = size - 1
			}
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), initialSize), 0600); err != nil {
				t.Fatal(err)
			}
			requests := 0
			h := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if growDuringTransfer {
					file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
					if err != nil {
						t.Fatal(err)
					}
					_, err = file.Write([]byte("x"))
					file.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				return http.DefaultTransport.RoundTrip(r)
			})}
			c, err := New(context.Background(), h, config.Endpoints{ChatAPIBase: s.URL(), UploadRoot: dir, MediaMaxBytes: size - 1})
			if err != nil {
				t.Fatal(err)
			}
			useTestClock(c)
			ref, err := c.UploadAttachment(context.Background(), "spaces/room", path)
			kind, _ := Classify(err)
			if ref != nil || kind != "permanent" || !strings.Contains(err.Error(), "large-limit.bin") {
				t.Fatalf("size limit: ref=%v kind=%s err=%v", ref, kind, err)
			}
			if !growDuringTransfer && requests != 0 {
				t.Fatalf("client sent oversize file (%d requests)", requests)
			}
		})
	}
}

func TestHTTP2UploadGrowthNeverCompletesPart(t *testing.T) {
	const size = 8 << 20
	dir := t.TempDir()
	path := filepath.Join(dir, "growing.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		protocol int
		bytes    int64
		err      error
	}
	done := make(chan result, 1)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := result{protocol: r.ProtoMajor}
		defer func() { done <- got }()
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Error(err)
			return
		}
		_, err = file.Write([]byte("x"))
		file.Close()
		if err != nil {
			t.Error(err)
			return
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Error(err)
			return
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		metadata, err := reader.NextPart()
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := io.Copy(io.Discard, metadata); err != nil {
			t.Error(err)
			return
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Error(err)
			return
		}
		got.bytes, got.err = io.Copy(io.Discard, part)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"attachmentDataRef":{"resourceName":"spaces/room/attachments/1"}}`)
	}))
	s.EnableHTTP2 = true
	s.StartTLS()
	defer s.Close()
	c, err := New(context.Background(), s.Client(), config.Endpoints{ChatAPIBase: s.URL, UploadRoot: dir, MediaMaxBytes: size})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ref, err := c.UploadAttachment(ctx, "spaces/room", path)
	if kind, _ := Classify(err); kind != "permanent" || ref != nil {
		t.Fatalf("growing upload: ref=%v err=%v", ref, err)
	}
	select {
	case got := <-done:
		if got.protocol != 2 {
			t.Fatalf("want HTTP/2, got HTTP/%d", got.protocol)
		}
		if got.err == nil {
			t.Fatalf("server parsed a complete truncated part: %d bytes", got.bytes)
		}
	case <-ctx.Done():
		t.Fatal("server did not finish reading the canceled upload")
	}
}

func TestUploadAttemptsHaveIndependentOffsets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := newUploadReader(file, 6, cancel)
	var prefix [2]byte
	if _, err := io.ReadFull(first, prefix[:]); err != nil || string(prefix[:]) != "ab" {
		t.Fatalf("first prefix=%q err=%v", prefix, err)
	}
	second := newUploadReader(file, 6, cancel)
	// An earlier SDK pipe can still be reading when the next attempt starts.
	if got, err := io.ReadAll(second); err != nil || string(got) != "abcdef" {
		t.Errorf("second attempt=%q err=%v", got, err)
	}
	if got, err := io.ReadAll(first); err != nil || string(got) != "cdef" {
		t.Errorf("first remainder=%q err=%v", got, err)
	}
	if err := ctx.Err(); err != nil {
		t.Errorf("in-limit attempts canceled: %v", err)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		code          int
		message, kind string
	}{
		{429, "quota", "rate_limited"}, {401, "unauthorized", "auth"}, {403, "scope missing", "auth"},
		{403, "The caller is not a member of the space.", "not_found"}, {404, "gone", "not_found"},
		{403, "The caller is not a member.", "not_found"},
		{400, "bad", "permanent"}, {408, "timeout", "transient"}, {409, "conflict", "transient"}, {500, "server", "transient"}, {503, "down", "transient"},
	} {
		err := fmt.Errorf("wrapped: %w", &googleapi.Error{Code: tc.code, Message: tc.message, Header: http.Header{"Retry-After": []string{"3"}}})
		kind, hint := Classify(err)
		if kind != tc.kind || (tc.code == 429 && hint != 3*time.Second) {
			t.Errorf("%d: %s %v", tc.code, kind, hint)
		}
	}
	if kind, _ := Classify(io.ErrUnexpectedEOF); kind != "transient" {
		t.Fatalf("network: %s", kind)
	}
	if kind, _ := Classify(nil); kind != "" {
		t.Fatalf("success: %s", kind)
	}
	for _, hint := range []string{"-1", "garbage", "99999999999999999999999999"} {
		_, delay := Classify(&googleapi.Error{Code: 429, Header: http.Header{"Retry-After": []string{hint}}})
		if delay != 0 {
			t.Errorf("invalid hint %q = %v", hint, delay)
		}
	}
	date := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
	_, delay := Classify(&googleapi.Error{Code: 429, Header: http.Header{"Retry-After": []string{date}}})
	if delay < 3*time.Second || delay > 5*time.Second {
		t.Fatalf("date hint: %v", delay)
	}
}

func TestLimiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var l Limiter
		start := time.Now()
		if err := l.WaitWrite(context.Background(), "spaces/a"); err != nil {
			t.Fatal(err)
		}
		if err := l.WaitWrite(context.Background(), "spaces/b"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := l.WaitWrite(ctx, "spaces/a"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queue cancellation: %v", err)
		}
		if err := l.WaitWrite(context.Background(), "spaces/a"); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("write interval: %v", elapsed)
		}
		start = time.Now()
		for i := 0; i < 6; i++ {
			if err := l.WaitReaction(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("reaction interval: %v", elapsed)
		}
		start = time.Now()
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Go(func() {
				if err := l.WaitWrite(context.Background(), "spaces/concurrent"); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if elapsed := time.Since(start); elapsed != 3*time.Second {
			t.Fatalf("concurrent writes: %v", elapsed)
		}
	})
}

func TestClientRateLimitAndDeadlineHint(t *testing.T) {
	c, s, _ := fixture(t)
	s.EnforceRate = true
	for i := 0; i < 2; i++ {
		if _, _, err := c.CreateMessage(context.Background(), "spaces/room", "paced", "", fmt.Sprint(i), true); err != nil {
			t.Fatal(err)
		}
	}
	// Freeze server time: the next write still sees a quota failure. Its hint must
	// survive the caller's deadline, rather than becoming an unclassified timeout.
	s.Configure(func(s *fakegoogle.Server) { frozen := c.timing.Now(); s.Now = func() time.Time { return frozen } })
	c.retryBound = 1200 * time.Millisecond
	_, _, err := c.CreateMessage(context.Background(), "spaces/room", "limited", "", "limited", true)
	kind, hint := Classify(err)
	if kind != "rate_limited" || hint != time.Second || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("quota deadline: %v %s %v", err, kind, hint)
	}
}

func TestRetryHintAndLostResponse(t *testing.T) {
	c, s, _ := fixture(t)
	s.DropNextResponse = true
	name, _, err := c.CreateMessage(context.Background(), "spaces/room", "once", "", "stable", true)
	if err != nil || name == "" {
		t.Fatalf("lost response: %s %v", name, err)
	}
	s.Configure(func(s *fakegoogle.Server) {
		if len(s.Messages) != 1 {
			t.Errorf("duplicate messages: %d", len(s.Messages))
		}
	})
	// The transport scripts a server quota response before forwarding to the fake.
	var first time.Time
	var clock *testClock
	calls := 0
	httpClient := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			first = clock.Now()
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"2"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"quota"}}`)), Request: r}, nil
		}
		if clock.Now().Sub(first) < 2*time.Second {
			t.Error("retried before Retry-After")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	c, err = New(context.Background(), httpClient, config.Endpoints{ChatAPIBase: s.URL()})
	if err != nil {
		t.Fatal(err)
	}
	clock = useTestClock(c)
	if _, _, err = c.CreateMessage(context.Background(), "spaces/dm", "retry", "", "hint", false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls: %d", calls)
	}
}

func TestRetryReturnsImmediatelyWhenHintExceedsCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		start := time.Now()
		c := &Client{}
		calls := 0
		err := c.retry(ctx, true, func(context.Context) error {
			calls++
			return &googleapi.Error{Code: 429, Header: http.Header{"Retry-After": []string{"1"}}}
		})
		if calls != 1 || !time.Now().Equal(start) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
		}
	})
}

func TestDeleteReactionRechecksIdentityAfterPacing(t *testing.T) {
	deletes := 0
	h := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete {
			deletes++
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"reactions":[{"name":"spaces/room/messages/one/reactions/mine","user":{"name":"users/111"},"emoji":{"unicode":"👍"}}]}`))}, nil
	})}
	c, err := New(context.Background(), h, config.Endpoints{ChatAPIBase: "https://chat.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	clock := useTestClock(c)
	if err := c.SetIdentityUserID("users/111"); err != nil {
		t.Fatal(err)
	}
	if err := c.limits.WaitReaction(context.Background()); err != nil {
		t.Fatal(err)
	}
	conflicted := false
	c.limits.timing.sleep = func(ctx context.Context, d time.Duration) error {
		conflicted = c.SetIdentityUserID("users/222") != nil
		return clock.Sleep(ctx, d)
	}
	err = c.DeleteReaction(context.Background(), "spaces/room/messages/one", "👍")
	if !conflicted || err == nil || deletes != 0 {
		t.Fatalf("conflicted=%v deletes=%d err=%v", conflicted, deletes, err)
	}
}
