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
	"unicode/utf8"

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

func TestRegistrationKeyTracksCallbackAndSurvivesClientRestart(t *testing.T) {
	keys := make(chan string, 4)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()
	callbacks := []string{
		s.URL + "/svc/gchat",
		s.URL + "/svc/gchat",     // fresh client, identical configuration
		other.URL + "/svc/gchat", // changed callback base
		s.URL + "/svc/other",     // changed service prefix
	}
	for _, callback := range callbacks {
		if err := New(s.URL, "examplecity").Register(t.Context(), registration(callback)); err != nil {
			t.Fatal(err)
		}
	}
	first, fresh, base, prefix := <-keys, <-keys, <-keys, <-keys
	if first == "" || first != fresh || first == base || first == prefix || base == prefix {
		t.Fatalf("registration keys: initial=%q fresh=%q changed-base=%q changed-prefix=%q", first, fresh, base, prefix)
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
				if !strings.Contains(err.Error(), fmt.Sprint(status)) || !strings.Contains(err.Error(), strings.TrimSpace(body)) {
					t.Fatalf("error message omits response: %v", err)
				}
			})
		}
	}
}

func TestHTTPErrorBoundsForeignBody(t *testing.T) {
	body := strings.Repeat("x", 1<<20)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	err := New(s.URL, "examplecity").Register(t.Context(), wire.Registration{})
	var responseErr *HTTPError
	if !errors.As(err, &responseErr) {
		t.Fatalf("error type = %T, want *HTTPError", err)
	}
	if len(responseErr.Body) > 64<<10 || responseErr.Body != body[:64<<10] {
		t.Errorf("retained body length = %d, want first 64 KiB", len(responseErr.Body))
	}
	message := err.Error()
	if len(message) > 300 || !strings.HasPrefix(message, "gc API: HTTP 502: x") || !strings.HasSuffix(message, "...") {
		t.Errorf("error message length = %d, want short status/body summary with ellipsis", len(message))
	}
}

func TestSuccessfulResponseIsNotTruncated(t *testing.T) {
	want := wire.InboundResponse{}
	want.Delivery.Status = strings.Repeat("x", 65<<10)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(want)
	}))
	t.Cleanup(s.Close)
	got, err := New(s.URL, "examplecity").Inbound(t.Context(), inbound("message"))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("response truncated: status length = %d, error = %v", len(got.Delivery.Status), err)
	}
}

func TestHTTPErrorProblemSummary(t *testing.T) {
	for _, contentType := range []string{"application/problem+json", "application/json", "text/plain", ""} {
		t.Run(contentType, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header()["Content-Type"] = []string{contentType}
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"type":"urn:gascity:error:validation-failed","title":"Validation Failed","status":422,"detail":"invalid conversation","code":"validation-failed","extra":"not in summary"}`)
			}))
			t.Cleanup(s.Close)
			err := New(s.URL, "examplecity").Register(t.Context(), wire.Registration{})
			if err == nil || err.Error() != "gc API: HTTP 422: validation-failed: invalid conversation" {
				t.Fatalf("error = %v; want status, problem code and detail", err)
			}
		})
	}
}

func TestOutboundBindingProblems(t *testing.T) {
	for _, owner := range []string{"", "other-session"} {
		t.Run("owner="+owner, func(t *testing.T) {
			s := fakegc.New(t)
			if owner != "" {
				s.Bind(conversation(), owner)
			}
			_, err := checkedClient(t, s.URL()).Outbound(t.Context(), wire.OutboundRequest{
				SessionID: "session", Conversation: conversation(), Text: "reply",
			})
			var responseErr *HTTPError
			if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("error = %v; want HTTP 422", err)
			}
			detail := "no active binding for conversation gchat/spaces/room"
			if owner != "" {
				detail = `session "session" does not own binding for conversation gchat/spaces/room (bound to other-session)`
			}
			want := &Problem{Type: "urn:gascity:error:validation-failed", Title: "Validation Failed", Status: 422, Detail: detail, Code: "validation-failed"}
			if !reflect.DeepEqual(responseErr.Problem, want) || responseErr.Code() != want.Code {
				t.Fatalf("problem = %+v, code = %q; want %+v", responseErr.Problem, responseErr.Code(), want)
			}
			var retained Problem
			if err := json.Unmarshal([]byte(responseErr.Body), &retained); err != nil || !reflect.DeepEqual(retained, *want) {
				t.Fatalf("raw body not retained: %q, %v", responseErr.Body, err)
			}
			for _, candidate := range []error{err, fmt.Errorf("outbound failed: %w", err)} {
				if IsNoBinding(candidate) != (owner == "") || IsWrongOwner(candidate) != (owner != "") {
					t.Errorf("binding helpers misclassified %v", candidate)
				}
			}
		})
	}
}

func TestHTTPErrorNonProblem(t *testing.T) {
	for _, body := range []string{"not JSON", `{"code":`, `null`, `{}`, `{"error":"proxy failure"}`, `[]`, `{"status":"bad"}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(s.Close)
			err := New(s.URL, "examplecity").Register(t.Context(), wire.Registration{})
			var responseErr *HTTPError
			if !errors.As(err, &responseErr) || responseErr.Problem != nil || responseErr.Code() != "" || responseErr.Body != body {
				t.Fatalf("error = %+v; want unparsed body %q", responseErr, body)
			}
			if IsNoBinding(err) || IsWrongOwner(err) {
				t.Fatalf("non-problem classified as binding error: %v", err)
			}
		})
	}
}

func TestBindingHelpersRejectOtherErrors(t *testing.T) {
	for _, detail := range []string{
		"no active binding for conversation gchat/spaces/room",
		`session "session" does not own binding for conversation gchat/spaces/room (bound to other-session)`,
	} {
		for _, err := range []error{
			nil,
			errors.New(detail),
			&HTTPError{StatusCode: 422, Body: detail},
			&HTTPError{StatusCode: 503, Problem: &Problem{Code: "validation-failed", Detail: detail}},
			&HTTPError{StatusCode: 422, Problem: &Problem{Code: "other", Detail: detail}},
			&HTTPError{StatusCode: 422, Problem: &Problem{Code: "validation-failed", Detail: "context: " + detail}},
		} {
			if IsNoBinding(err) || IsWrongOwner(err) {
				t.Errorf("unrelated error classified as binding error: %v", err)
			}
		}
	}
	for _, detail := range []string{"session is required", `session "session" could not resolve a binding`, `session setup: does not own binding for conversation gchat/spaces/room`} {
		err := &HTTPError{StatusCode: 422, Problem: &Problem{Code: "validation-failed", Detail: detail}}
		if IsNoBinding(err) || IsWrongOwner(err) {
			t.Errorf("unrelated validation detail classified as binding error: %v", err)
		}
	}
}

func TestWrongOwnerAgentBinding(t *testing.T) {
	err := &HTTPError{StatusCode: 422, Problem: &Problem{
		Code:   "validation-failed",
		Detail: fmt.Sprintf("session %q does not own binding for conversation gchat/spaces/room (bound to agent example, currently session other)", `session"quoted`),
	}}
	if !IsWrongOwner(err) || IsNoBinding(err) {
		t.Fatalf("agent binding misclassified: %v", err)
	}
}

func TestHTTPErrorBoundsProblemSummary(t *testing.T) {
	for _, field := range []string{"code", "detail"} {
		t.Run(field, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_ = json.NewEncoder(w).Encode(map[string]string{field: strings.Repeat("x", 1024)})
			}))
			t.Cleanup(s.Close)
			err := New(s.URL, "examplecity").Register(t.Context(), wire.Registration{})
			var responseErr *HTTPError
			if !errors.As(err, &responseErr) || responseErr.Problem == nil {
				t.Fatalf("error = %v; want parsed problem", err)
			}
			if len(err.Error()) > 300 || !strings.HasSuffix(err.Error(), "...") {
				t.Errorf("problem summary not bounded: %d bytes", len(err.Error()))
			}
		})
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

func TestTransportFailureIsNotHTTPError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("closed server received a request")
	}))
	s.Close()
	for name, call := range clientCalls() {
		t.Run(name, func(t *testing.T) {
			err := call(t.Context(), checkedClient(t, s.URL))
			if err == nil {
				t.Fatal("connection to closed server succeeded")
			}
			var responseErr *HTTPError
			if errors.As(err, &responseErr) {
				t.Fatalf("transport failure classified as HTTP response: %v", err)
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

func TestNoAdapterProblem(t *testing.T) {
	s := fakegc.New(t)
	s.Bind(conversation(), "session")
	_, err := checkedClient(t, s.URL()).Outbound(t.Context(), wire.OutboundRequest{
		SessionID: "session", Conversation: conversation(), Text: "reply",
	})
	for _, candidate := range []error{err, fmt.Errorf("outbound failed: %w", err)} {
		if !IsNoAdapter(candidate, "example") || IsNoBinding(candidate) || IsWrongOwner(candidate) {
			t.Errorf("missing registration misclassified: %v", candidate)
		}
	}
	const detail = "no adapter for gchat/example"
	for _, candidate := range []error{
		nil,
		(*HTTPError)(nil),
		errors.New(detail),
		&HTTPError{StatusCode: 422, Body: detail},
		&HTTPError{StatusCode: 503, Problem: &Problem{Code: "validation-failed", Detail: detail}},
		&HTTPError{StatusCode: 422, Problem: &Problem{Code: "other", Detail: detail}},
		&HTTPError{StatusCode: 422, Problem: &Problem{Code: "validation-failed", Detail: "context: " + detail}},
		&HTTPError{StatusCode: 422, Problem: &Problem{Code: "validation-failed", Detail: "no active binding for conversation gchat/spaces/room"}},
		&HTTPError{StatusCode: 422, Problem: &Problem{Code: "validation-failed", Detail: "no adapter format supplied"}},
		&HTTPError{StatusCode: 422, Problem: &Problem{Code: "validation-failed", Detail: "no adapter for gchat/previous"}},
		&HTTPError{StatusCode: 422, Problem: &Problem{Code: "validation-failed", Detail: "no adapter for other/example"}},
		&HTTPError{StatusCode: 422, Problem: &Problem{Code: "validation-failed", Detail: detail + "-previous"}},
		&HTTPError{StatusCode: 422, Problem: &Problem{Code: "validation-failed", Detail: detail + " extra"}},
	} {
		if IsNoAdapter(candidate, "example") {
			t.Errorf("unrelated error classified as missing registration: %v", candidate)
		}
	}
}

func TestCityNotRunningProblem(t *testing.T) {
	s := fakegc.New(t)
	s.Configure(func(g *fakegc.Server) { g.CityNotRunning = true })
	c := checkedClient(t, s.URL())
	_, outboundErr := c.Outbound(t.Context(), wire.OutboundRequest{SessionID: "session", Conversation: conversation(), Text: "reply"})
	registerErr := c.Register(t.Context(), wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: "http://localhost/callback"})
	for _, candidate := range []error{outboundErr, registerErr, fmt.Errorf("wrapped: %w", outboundErr)} {
		if !IsCityNotRunning(candidate) {
			t.Errorf("city startup error misclassified: %v", candidate)
		}
	}
	const detail = "not_found: city not found or not running: examplecity"
	for _, candidate := range []error{
		nil,
		(*HTTPError)(nil),
		errors.New(detail),
		&HTTPError{StatusCode: 404, Body: detail},
		&HTTPError{StatusCode: 422, Problem: &Problem{Code: "city-not-found", Detail: detail}},
		&HTTPError{StatusCode: 404, Problem: &Problem{Code: "not-found", Detail: detail}},
		&HTTPError{StatusCode: 404, Problem: &Problem{Code: "city-not-found", Detail: "context: " + detail}},
		&HTTPError{StatusCode: 404, Problem: &Problem{Code: "city-not-found", Detail: "route not found"}},
	} {
		if IsCityNotRunning(candidate) {
			t.Errorf("unrelated error classified as city startup: %v", candidate)
		}
	}
}

func TestHTTPErrorSafeUsefulSummary(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"title", `{"title":"Bad gateway","status":502}`, "Bad gateway"},
		{"body", `{"status":502}`, `{"status":502}`},
		{"detail", `{"detail":"try later","status":502}`, "try later"},
		{"validation", `{"title":"Unprocessable Entity","detail":"validation failed","errors":[{"location":"body.callback_url","message":"expected uri"},{"location":"body.account_id","message":"required"}]}`, "Unprocessable Entity: validation failed: body.callback_url: expected uri; body.account_id: required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var problem Problem
			if err := json.Unmarshal([]byte(tc.body), &problem); err != nil {
				t.Fatal(err)
			}
			got := (&HTTPError{StatusCode: 502, Body: tc.body, Problem: &problem}).Error()
			if got != "gc API: HTTP 502: "+tc.want {
				t.Fatalf("summary: %q", got)
			}
		})
	}
	for _, body := range []string{strings.Repeat("a", 255) + "界", "bad\nforged\rline\x00\x1b", string([]byte{0xff, 0xfe})} {
		got := (&HTTPError{StatusCode: 502, Body: body}).Error()
		if !utf8.ValidString(got) || strings.ContainsAny(got, "\n\r\x00\x1b") || len(got) > 300 {
			t.Fatalf("unsafe summary %q", got)
		}
	}
}
