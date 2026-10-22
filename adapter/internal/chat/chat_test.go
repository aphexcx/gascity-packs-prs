package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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

type contextBody struct {
	ctx context.Context
	io.ReadCloser
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
	if err := c.SetIdentityUserID("users/example"); err != nil {
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

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		code          int
		message, kind string
	}{
		{429, "quota", "rate_limited"}, {401, "unauthorized", "auth"}, {403, "scope missing", "auth"},
		{403, "The caller is not a member of the space.", "not_found"}, {404, "gone", "not_found"},
		{400, "bad", "permanent"}, {409, "conflict", "permanent"}, {500, "server", "transient"}, {503, "down", "transient"},
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
	s.Configure(func(s *fakegoogle.Server) { frozen := time.Now(); s.Now = func() time.Time { return frozen } })
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	_, _, err := c.CreateMessage(ctx, "spaces/room", "limited", "", "limited", true)
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
	calls := 0
	httpClient := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			first = time.Now()
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"2"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"quota"}}`)), Request: r}, nil
		}
		if time.Since(first) < 2*time.Second {
			t.Error("retried before Retry-After")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	c, err = New(context.Background(), httpClient, config.Endpoints{ChatAPIBase: s.URL()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.CreateMessage(context.Background(), "spaces/dm", "retry", "", "hint", false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls: %d", calls)
	}
}
