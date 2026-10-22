package gcclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/wire"
)

func conversation() wire.Conversation {
	return wire.Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "example", ConversationID: "spaces/room", Kind: "room"}
}

func registration(callback string) wire.Registration {
	return wire.Registration{
		Provider: "gchat", AccountID: "example", CallbackURL: callback,
		Capabilities:      wire.Capabilities{SupportsAttachments: true, MaxMessageLength: 32000},
		ReplyInstructions: "gc gchat reply-current --conversation-id {conversation_id} --body-file <path>",
	}
}

func inbound(id string) wire.InboundMessage {
	return wire.InboundMessage{
		ProviderMessageID: id, Conversation: conversation(),
		Actor: wire.Actor{ID: "users/example", DisplayName: "Example"}, Text: "hello",
		ReplyToMessageID: "spaces/room/threads/one", ExplicitTarget: "example",
		Attachments: []wire.Attachment{{ProviderID: "attachment", URL: "file:///tmp/example.txt", MIMEType: "text/plain"}},
		DedupKey:    "stable", ReceivedAt: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Observe the real client requests while still exercising fakegc's HTTP contract.
func checkedClient(t *testing.T, base string) *Client {
	t.Helper()
	c := New(base, "examplecity")
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get(wire.RequestHeader); got != "gc-gchat-adapter" {
			t.Errorf("%s %s: X-GC-Request = %q", r.Method, r.URL.Path, got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("%s %s: Content-Type = %q", r.Method, r.URL.Path, got)
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	return c
}

func TestRegister(t *testing.T) {
	s := fakegc.New(t)
	c := checkedClient(t, s.URL()+"/")
	if err := c.Register(t.Context(), registration(s.URL()+"/adapter")); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterWithoutHeaderRejected(t *testing.T) {
	s := fakegc.New(t)
	body, err := json.Marshal(registration(s.URL() + "/adapter"))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+"/v0/city/examplecity/extmsg/adapters", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(data), "X-GC-Request") {
		t.Fatalf("missing header: status %d, body %s", resp.StatusCode, data)
	}
}

func TestInbound(t *testing.T) {
	s := fakegc.New(t)
	c := checkedClient(t, s.URL())
	for _, status := range []string{"delivered", "pending", "partial", "failed", "no_route"} {
		t.Run(status, func(t *testing.T) {
			s.Configure(func(s *fakegc.Server) { s.NextDelivery = status })
			message := inbound("spaces/room/messages/" + status)
			got, err := c.Inbound(t.Context(), message)
			if err != nil {
				t.Fatal(err)
			}
			if got.Delivery.Status != status || got.Delivery.ReceiptID == "" {
				t.Fatalf("delivery = %+v", got.Delivery)
			}
			receipt, err := c.Receipt(t.Context(), got.Delivery.ReceiptID)
			if err != nil {
				t.Fatal(err)
			}
			if status == "pending" {
				if receipt.State != "pending" {
					t.Fatalf("receipt = %+v", receipt)
				}
			} else if receipt.State != "concluded" || receipt.Delivery.Status != status {
				t.Fatalf("receipt = %+v", receipt)
			}
			messages := s.Inbound()
			if len(messages) == 0 || !reflect.DeepEqual(messages[len(messages)-1], message) {
				t.Fatalf("inbound fields changed: %+v", messages)
			}
		})
	}
	if len(s.Inbound()) != 5 {
		t.Fatalf("inbound requests = %d, want 5", len(s.Inbound()))
	}
}

func TestReceiptStates(t *testing.T) {
	s := fakegc.New(t)
	c := checkedClient(t, s.URL())
	s.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
	response, err := c.Inbound(t.Context(), inbound("spaces/room/messages/pending"))
	if err != nil {
		t.Fatal(err)
	}
	id := response.Delivery.ReceiptID
	assertReceipt := func(want wire.ReceiptResponse) {
		t.Helper()
		got, err := c.Receipt(t.Context(), id)
		if err != nil || got != want {
			t.Fatalf("receipt = %+v, %v; want %+v", got, err, want)
		}
	}
	assertReceipt(wire.ReceiptResponse{State: "pending"})
	for _, status := range []string{"delivered", "no_route", "partial", "failed", "pending"} {
		want := wire.ReceiptResponse{State: "concluded"}
		want.Delivery.Status = status
		s.SetReceipt(id, want)
		assertReceipt(want)
	}
	s.Advance(time.Hour + time.Nanosecond)
	assertReceipt(wire.ReceiptResponse{State: "unknown"})
	id = "missing"
	assertReceipt(wire.ReceiptResponse{State: "unknown"})
}

func TestOutboundTranscript(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		t.Run(fmt.Sprintf("recorded=%t", recorded), func(t *testing.T) {
			s := fakegc.New(t)
			s.Bind(conversation(), "session")
			s.Configure(func(s *fakegc.Server) { s.FailTranscript = !recorded })
			callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/adapter/publish" {
					t.Errorf("callback = %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(wire.PublishReceipt{
					MessageID: "spaces/room/messages/sent", Conversation: conversation(), Delivered: true,
				})
			}))
			t.Cleanup(callback.Close)
			c := checkedClient(t, s.URL())
			if err := c.Register(t.Context(), registration(callback.URL+"/adapter")); err != nil {
				t.Fatal(err)
			}
			req := wire.OutboundRequest{SessionID: "session", Conversation: conversation(), Text: "reply", ReplyToMessageID: "spaces/room/threads/one", IdempotencyKey: "stable"}
			got, err := c.Outbound(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Receipt.Delivered || got.Receipt.MessageID != "spaces/room/messages/sent" || (got.TranscriptEntry != nil) != recorded {
				t.Fatalf("outbound = %+v", got)
			}
			if recorded && (*got.TranscriptEntry)["Text"] != req.Text {
				t.Fatalf("transcript = %+v", *got.TranscriptEntry)
			}
			want := []wire.PublishRequest{{SessionID: req.SessionID, Conversation: req.Conversation, Text: req.Text, ReplyToMessageID: req.ReplyToMessageID, IdempotencyKey: req.IdempotencyKey}}
			if published := s.Published(); !reflect.DeepEqual(published, want) {
				t.Fatalf("callbacks = %+v; want %+v", published, want)
			}
		})
	}
}

func clientCalls() map[string]func(context.Context, *Client) error {
	return map[string]func(context.Context, *Client) error{
		"register": func(ctx context.Context, c *Client) error { return c.Register(ctx, wire.Registration{}) },
		"inbound": func(ctx context.Context, c *Client) error {
			_, err := c.Inbound(ctx, wire.InboundMessage{})
			return err
		},
		"receipt": func(ctx context.Context, c *Client) error {
			_, err := c.Receipt(ctx, "receipt")
			return err
		},
		"outbound": func(ctx context.Context, c *Client) error {
			_, err := c.Outbound(ctx, wire.OutboundRequest{})
			return err
		},
	}
}

func TestHTTPErrorIncludesStatusAndBody(t *testing.T) {
	for name, call := range clientCalls() {
		for _, status := range []int{302, 400, 422, 503} {
			t.Run(fmt.Sprintf("%s/%d", name, status), func(t *testing.T) {
				const body = "gc request failed\n"
				requests := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					if requests > 1 {
						t.Error("client retried or followed a redirect")
						w.WriteHeader(http.StatusOK)
						return
					}
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, body)
				}))
				t.Cleanup(s.Close)
				err := call(t.Context(), checkedClient(t, s.URL))
				var responseErr *HTTPError
				if !errors.As(err, &responseErr) || responseErr.StatusCode != status || responseErr.Body != body {
					t.Fatalf("error = %v; want status %d and body %q", err, status, body)
				}
				if !strings.Contains(err.Error(), fmt.Sprint(status)) || !strings.Contains(err.Error(), body) {
					t.Fatalf("error message omits response: %v", err)
				}
			})
		}
	}
}

func TestMalformedResponse(t *testing.T) {
	for name, call := range clientCalls() {
		if name == "register" { // Registration only needs a successful status.
			continue
		}
		for _, body := range []string{"", "not JSON", `{} {}`} {
			t.Run(name+"/"+body, func(t *testing.T) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, body)
				}))
				t.Cleanup(s.Close)
				if err := call(t.Context(), checkedClient(t, s.URL)); err == nil {
					t.Fatal("malformed response accepted")
				}
			})
		}
	}
}

func TestContextCancellation(t *testing.T) {
	for name, call := range clientCalls() {
		t.Run(name, func(t *testing.T) {
			s := fakegc.New(t)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := call(ctx, checkedClient(t, s.URL())); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v; want context.Canceled", err)
			}
		})
	}
}

func TestReceiptEscapesPathSegments(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/proxy/v0/city/example%20city%2F%23%3F/extmsg/inbound/receipts/receipt%2F%3F%23%25"; r.URL.EscapedPath() != want || r.URL.RawQuery != "" {
			t.Errorf("URL = %s; want %s", r.URL, want)
		}
		_, _ = io.WriteString(w, `{"state":"unknown"}`)
	}))
	t.Cleanup(s.Close)
	c := New(s.URL+"/proxy/", "example city/#?")
	if _, err := c.Receipt(t.Context(), "receipt/?#%"); err != nil {
		t.Fatal(err)
	}
}
