package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegoogle"
	googlechat "google.golang.org/api/chat/v1"
)

// createBodies keeps the body of every message create the client sends, byte
// for byte, and its query, then passes the request on to the fake Google server.
type createBodies struct {
	mu     sync.Mutex
	bodies []createRequest
}

type createRequest struct {
	body  []byte
	query url.Values
}

var messageCreatePath = regexp.MustCompile(`^/v1/spaces/[^/]+/messages$`)

func (r *createBodies) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && messageCreatePath.MatchString(req.URL.Path) && req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, createRequest{body: b, query: req.URL.Query()})
		r.mu.Unlock()
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (r *createBodies) take() []createRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.bodies
	r.bodies = nil
	return b
}

// Google's page "Format messages" says a newly created Chat app defaults to
// Markdown, and that <users/{user}> is a mention in Chat markup syntax. The
// converter writes Chat markup, so every message the client creates states
// MARKUP_SYNTAX_CHAT and leaves the rest of the body as it was.
func TestEveryCreateStatesChatMarkupSyntax(t *testing.T) {
	g := fakegoogle.New(t, nil)
	g.Configure(func(s *fakegoogle.Server) {
		s.Spaces["spaces/room"] = &googlechat.Space{Name: "spaces/room", SpaceType: "SPACE"}
		s.Spaces["spaces/dm"] = &googlechat.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
	})
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := &createBodies{}
	c, err := New(context.Background(), &http.Client{Transport: rec}, config.Endpoints{ChatAPIBase: g.URL(), MediaDir: dir, UploadRoot: dir})
	if err != nil {
		t.Fatal(err)
	}
	clock := useTestClock(c)
	g.Now = clock.Now
	ctx := context.Background()

	_, thread, err := c.CreateMessage(ctx, "spaces/room", "seed", "", "seed", true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(path, []byte("hello media"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := c.UploadAttachment(ctx, "spaces/room", path)
	if err != nil {
		t.Fatal(err)
	}
	files := []*googlechat.Attachment{{AttachmentDataRef: ref}}
	rec.take()

	const text = "*bold* _italic_ ~strike~ `code` <https://example.com|a link> and a tag <users/1234567890>"
	const notice = "I can't read your message right now."
	const fallback = "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"
	// want is the body each create sent before the syntax was stated, spelled
	// out by hand from the fields the creates set then (text, thread,
	// attachments); requestID and reply are the query the creates set, which
	// stating the syntax does not touch.
	cases := []struct {
		name      string
		send      func() error
		want      googlechat.Message
		requestID string
		reply     string
	}{{
		name: "notice",
		send: func() error { _, _, err := c.CreateNotice(ctx, "spaces/room", notice, "", true); return err },
		want: googlechat.Message{Text: notice},
	}, {
		name:  "notice in a thread",
		send:  func() error { _, _, err := c.CreateNotice(ctx, "spaces/room", notice, thread, true); return err },
		want:  googlechat.Message{Text: notice, Thread: &googlechat.Thread{Name: thread}},
		reply: fallback,
	}, {
		name:      "message",
		send:      func() error { _, _, err := c.CreateMessage(ctx, "spaces/room", text, "", "top", true); return err },
		want:      googlechat.Message{Text: text},
		requestID: "top",
	}, {
		name: "message in a thread",
		send: func() error {
			_, _, err := c.CreateMessage(ctx, "spaces/room", text, thread, "reply", true)
			return err
		},
		want:      googlechat.Message{Text: text, Thread: &googlechat.Thread{Name: thread}},
		requestID: "reply",
		reply:     fallback,
	}, {
		name:      "message in a direct message",
		send:      func() error { _, _, err := c.CreateMessage(ctx, "spaces/dm", text, "", "dm", false); return err },
		want:      googlechat.Message{Text: text},
		requestID: "dm",
	}, {
		name: "message with a file",
		send: func() error {
			_, _, err := c.CreateMessageWithAttachments(ctx, "spaces/room", "file", "", "file", true, files)
			return err
		},
		want:      googlechat.Message{Text: "file", Attachment: files},
		requestID: "file",
	}, {
		name: "message with a file and its time",
		send: func() error {
			_, _, _, err := c.CreateMessageWithAttachmentsAndTime(ctx, "spaces/room", text, thread, "timed", true, files)
			return err
		},
		want:      googlechat.Message{Text: text, Attachment: files, Thread: &googlechat.Thread{Name: thread}},
		requestID: "timed",
		reply:     fallback,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.send(); err != nil {
				t.Fatal(err)
			}
			sent := rec.take()
			if len(sent) != 1 {
				t.Fatalf("creates sent = %d, want 1", len(sent))
			}
			got := bytes.TrimSpace(sent[0].body)
			var fields map[string]any
			if err := json.Unmarshal(got, &fields); err != nil {
				t.Fatalf("%s: %v", got, err)
			}
			if fields["markupSyntax"] != "MARKUP_SYNTAX_CHAT" {
				t.Fatalf("markupSyntax = %v, want MARKUP_SYNTAX_CHAT; body %s", fields["markupSyntax"], got)
			}
			// Nothing else changes: the body is the old one with the syntax added.
			want := tc.want
			want.MarkupSyntax = "MARKUP_SYNTAX_CHAT"
			wantBody, err := json.Marshal(&want)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, wantBody) {
				t.Fatalf("body\n got %s\nwant %s", got, wantBody)
			}
			q := sent[0].query
			if q.Get("requestId") != tc.requestID || q.Get("messageReplyOption") != tc.reply {
				t.Fatalf("query %s, want requestId %q and messageReplyOption %q", q.Encode(), tc.requestID, tc.reply)
			}
		})
	}
}
