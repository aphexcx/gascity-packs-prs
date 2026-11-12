package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
)

func observeGoogle(t *testing.T, f *fixture, intercept func(*http.Request) (*http.Response, error)) *atomic.Int32 {
	t.Helper()
	calls := new(atomic.Int32)
	c, err := chat.New(t.Context(), &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if intercept != nil {
			return intercept(r)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}, f.cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetClients(c, f.options.GC)
	return calls
}

func TestReferenceSpellingsAndMalformedValues(t *testing.T) {
	for _, ref := range []string{questionInA, "gchat:" + questionInA, questionInA + "#deleted",
		questionInA + "#updated#2026-11-12T01:02:03.123Z", questionInA + "/reactions/REACTION#created",
		questionInA + "/reactions/REACTION#deleted"} {
		t.Run(ref, func(t *testing.T) {
			f := wrongGroupFixture(t)
			code, raw, _ := f.call(t, "/commands/publish", map[string]any{"to": ref, "text": "answer"})
			got := decode[CommandResult](t, raw)
			if code != 200 || !got.Delivered || got.InReplyTo != questionInA || got.ConversationID != groupA || got.Thread != questionThread || got.SpaceName != "Group A" {
				t.Fatalf("reference outcome: %d %s", code, raw)
			}
		})
	}
	for _, ref := range []string{"spaces/AAAA", "https://chat.google.com/spaces/AAAA/messages/QUESTION", questionInA + "#bogus", questionInA + "#updated#bad", "spaces/AAAA/messages/.."} {
		t.Run("invalid/"+ref, func(t *testing.T) {
			f := setup(t)
			calls := observeGoogle(t, f, nil)
			code, raw, _ := f.call(t, "/commands/publish", map[string]any{"to": ref, "text": "answer"})
			if code != 400 || calls.Load() != 0 || f.count() != 0 {
				t.Fatalf("malformed reference: %d %s calls=%d", code, raw, calls.Load())
			}
		})
	}
}

func TestReferenceLedgerGoogleAndTopLevel(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cached, top bool
	}{
		{"ledger", true, false}, {"Google", false, false}, {"top-level", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := wrongGroupFixture(t)
			if !tc.cached {
				delete(f.l.Messages, questionInA)
			}
			var gets atomic.Int32
			observeGoogle(t, f, func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" && r.URL.Path == "/v1/"+questionInA {
					gets.Add(1)
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			code, raw, _ := f.call(t, "/commands/publish", map[string]any{"to": questionInA, "top_level": tc.top, "text": "answer"})
			got := decode[CommandResult](t, raw)
			if code != 200 || !got.Delivered || got.InReplyTo != questionInA || (got.Thread == questionThread) == tc.top {
				t.Fatalf("resolved thread: %d %s", code, raw)
			}
			if (gets.Load() == 0) != tc.cached {
				t.Fatalf("message GETs=%d", gets.Load())
			}
			if tc.top && f.l.Sends[got.Key].Request.ReplyToMessageID != "" {
				t.Fatal("top-level request retained thread")
			}
		})
	}
}

func TestReferenceAgreementsAndEveryRoute(t *testing.T) {
	for _, route := range []string{"/commands/publish", "/commands/upload", "/publish"} {
		t.Run(route, func(t *testing.T) {
			f := wrongGroupFixture(t)
			calls := observeGoogle(t, f, nil)
			var cmd any = map[string]any{"conversation_id": groupB, "thread": questionThread, "text": "answer", "path": "unused"}
			if route == "/publish" {
				cmd = wire.PublishRequest{Conversation: conversation(groupB), ReplyToMessageID: questionThread, Text: "answer"}
			}
			code, raw, _ := f.call(t, route, cmd)
			if route == "/publish" {
				got := decode[wire.PublishReceipt](t, raw)
				if got.Delivered || got.FailureKind != "permanent" {
					t.Fatal(string(raw))
				}
			} else if code != 400 {
				t.Fatalf("mixed route: %d %s", code, raw)
			}
			if calls.Load() != 0 || f.count() != 1 {
				t.Fatal("mixed route called Google")
			}
		})
	}
	for _, tc := range []struct {
		space, thread string
		refuse        bool
	}{
		{groupA, questionThread, false}, {groupB, questionThread, true}, {groupA, groupA + "/threads/OTHER", true},
	} {
		f := wrongGroupFixture(t)
		code, raw, _ := f.call(t, "/commands/publish", map[string]any{"to": questionInA, "conversation_id": tc.space, "thread": tc.thread, "text": "answer"})
		if tc.refuse {
			if code != 400 || f.count() != 1 {
				t.Fatalf("conflict: %d %s", code, raw)
			}
		} else if code != 200 || !decode[CommandResult](t, raw).Delivered {
			t.Fatalf("equal pair: %d %s", code, raw)
		}
	}
}

func TestReferenceLookupFailures(t *testing.T) {
	for _, status := range []int{403, 404, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := setup(t)
			observeGoogle(t, f, func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" || r.URL.Path != "/v1/"+questionInA {
					t.Errorf("unexpected Google call: %s %s", r.Method, r.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"60"}},
					Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"code":%d,"message":"private upstream text"}}`, status))), Request: r}, nil
			})
			code, raw, _ := f.call(t, "/commands/publish", map[string]any{"to": questionInA, "text": "answer"})
			got := decode[CommandResult](t, raw)
			if code != 200 || got.Delivered || f.count() != 0 || strings.Contains(got.Reason, "\n") || strings.Contains(got.Reason, "private") {
				t.Fatalf("lookup failure: %d %s", code, raw)
			}
			if status == 503 && (got.FailureKind != "transient" || got.RetryAfter == 0) {
				t.Fatalf("lost retry hint: %s", raw)
			}
			if status != 503 && !strings.Contains(got.Reason, "cannot be read") {
				t.Fatalf("missing refusal: %s", raw)
			}
		})
	}
}

func TestReferenceRetryKeepsOutcomeAndRejectsAnotherSpace(t *testing.T) {
	f := wrongGroupFixture(t)
	cmd := map[string]any{"to": questionInA, "text": "answer", "idempotency_key": "retry", "session_id": "session"}
	_, first, _ := f.call(t, "/commands/publish", cmd)
	delete(f.l.Messages, questionInA)
	f.g.Configure(func(g *fakegoogle.Server) { delete(g.Messages, questionInA) })
	calls := observeGoogle(t, f, nil)
	_, second, _ := f.call(t, "/commands/publish", cmd)
	if string(first) != string(second) || calls.Load() != 0 || !decode[CommandResult](t, second).Delivered {
		t.Fatalf("retry changed: %s / %s", first, second)
	}
	other := groupB + "/messages/OTHER"
	f.l.Messages[other] = &ledger.MessageState{Thread: groupB + "/threads/OTHER"}
	cmd["to"] = other
	_, raw, _ := f.call(t, "/commands/publish", cmd)
	if decode[CommandResult](t, raw).Delivered || len(landedInB(f)) != 0 {
		t.Fatalf("key moved spaces: %s", raw)
	}
}

func TestSendDestinationAndIdentifierOnlyLog(t *testing.T) {
	for _, tc := range []struct {
		name, space, ref string
		upload           bool
	}{
		{"reply", groupA, questionInA, false}, {"new topic", groupA, "", false}, {"direct message", "spaces/dm", "", false}, {"upload reply", groupA, questionInA, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := wrongGroupFixture(t)
			var logs bytes.Buffer
			f.s.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			cmd := map[string]any{"to": tc.ref, "conversation_id": tc.space, "text": "private body text", "idempotency_key": "logged"}
			route := "/commands/publish"
			if tc.upload {
				route = "/commands/upload"
				path := filepath.Join(f.cfg.Endpoints.UploadRoot, "file.txt")
				if err := os.WriteFile(path, []byte("private file"), 0600); err != nil {
					t.Fatal(err)
				}
				cmd["path"] = path
			}
			_, raw, _ := f.call(t, route, cmd)
			got := decode[CommandResult](t, raw)
			if !got.Delivered || got.ConversationID != tc.space || got.InReplyTo != tc.ref || got.Thread == "" {
				t.Fatalf("destination: %s", raw)
			}
			for _, field := range []string{"conversation_id", "space_name", "thread", "in_reply_to"} {
				var fields map[string]any
				_ = json.Unmarshal(raw, &fields)
				if _, ok := fields[field]; !ok {
					t.Fatalf("missing %s", field)
				}
			}
			_, _, _ = f.call(t, route, cmd)
			if strings.Count(logs.String(), `"level":"INFO"`) != 1 || strings.Contains(logs.String(), "private") {
				t.Fatalf("send logs: %s", logs.String())
			}
			line := decode[map[string]any](t, bytes.TrimSpace(logs.Bytes()))
			if line["space"] != got.ConversationID || line["thread"] != got.Thread || line["in_reply_to"] != tc.ref || line["message_id"] != got.MessageID || line["session"] != "" {
				t.Fatalf("log address: %s", logs.String())
			}
		})
	}
}

func TestReferenceRejectsLegacyMixedThreadOnEverySendPath(t *testing.T) {
	for _, route := range []string{"/commands/publish", "/commands/upload", "/publish", "recovery"} {
		t.Run(route, func(t *testing.T) {
			f := wrongGroupFixture(t)
			legacy := wire.PublishRequest{Conversation: conversation(groupA), ReplyToMessageID: groupB + "/threads/OTHER", Text: "pending answer", IdempotencyKey: "legacy"}
			if err := f.s.prepare(t.Context(), legacy, nil, true); err != nil {
				t.Fatal(err)
			}
			calls := observeGoogle(t, f, nil)
			switch route {
			case "recovery":
				_ = f.s.RetryPending(t.Context())
			case "/publish":
				legacy.ReplyToMessageID = questionThread
				_, _, _ = f.call(t, route, legacy)
			default:
				_, _, _ = f.call(t, route, map[string]any{"to": questionInA, "idempotency_key": "legacy", "path": "unused", "text": "answer"})
			}
			if calls.Load() != 0 || f.count() != 1 {
				t.Fatalf("legacy mixed thread reached Google: calls=%d messages=%d", calls.Load(), f.count())
			}
		})
	}
}

func TestReferenceDeliveredAddressSurvivesCommandDeadline(t *testing.T) {
	f := wrongGroupFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := wire.PublishRequest{SessionID: "session", Conversation: conversation(groupA), Text: "answer", IdempotencyKey: "deadline"}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if receipt, err := f.s.publish(t.Context(), p, nil); err != nil || !receipt.Delivered {
			t.Errorf("callback: %+v %v", receipt, err)
		}
		cancel() // The command budget ends after delivery, before gc answers.
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer h.Close()
	f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
	got, _ := f.s.command(ctx, p, nil, questionInA)
	if !got.Delivered || got.Thread == "" || got.InReplyTo != questionInA || got.ConversationID != groupA {
		t.Fatalf("delivered result lost its address at deadline: %+v", got)
	}
}
