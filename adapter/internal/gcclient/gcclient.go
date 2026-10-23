// Package gcclient implements the gc extmsg HTTP API. Delivery retries and
// receipt polling belong to callers, which can persist ambiguous outcomes.
package gcclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"gchat-adapter/internal/wire"
)

type Client struct {
	base, city  string
	http        *http.Client
	onRefusedMu sync.RWMutex
	onRefused   func(error)
}

// SetConnectionRefusedHandler lets the registrar observe failures from every
// gc call, including inbound and receipt polling owned by other components.
// The handler must return promptly; it schedules service-lifetime recovery.
func (c *Client) SetConnectionRefusedHandler(fn func(error)) {
	c.onRefusedMu.Lock()
	defer c.onRefusedMu.Unlock()
	c.onRefused = fn
}

// New constructs a client for a gc API base URL and city. Each call respects
// its context and has a 35-second ceiling, allowing gc's 30-second publish
// callback deadline to expire before the client's own timeout.
// A redirect is a non-2xx error, not permission to replay a mutation or send it
// to another endpoint.
func New(base, city string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"), city: city,
		http: &http.Client{
			Timeout:       35 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Problem is gc's RFC 9457 error envelope, including its machine-readable code.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
	Errors []struct {
		Message  string `json:"message"`
		Location string `json:"location"`
	} `json:"errors,omitempty"`
}

// HTTPError retains gc's status and up to 64 KiB of its response body.
// Problem is populated best-effort, regardless of the response content type.
type HTTPError struct {
	StatusCode int
	Body       string
	Problem    *Problem
	RetryAfter string // Retry-After header; interpreted by the caller's retry policy.
}

func (e *HTTPError) Error() string {
	summary := e.Body

	if p := e.Problem; p != nil {
		var parts []string
		label := p.Code
		if label == "" {
			label = p.Title
		}
		if label != "" {
			parts = append(parts, label)
		}
		if p.Detail != "" {
			parts = append(parts, p.Detail)
		}
		var fields []string
		for i, field := range p.Errors {
			if i >= 3 {
				break
			}
			detail := field.Message
			if field.Location != "" {
				detail = field.Location + ": " + detail
			}
			if detail != "" {
				fields = append(fields, detail)
			}
		}
		if len(fields) > 0 {
			parts = append(parts, strings.Join(fields, "; "))
		}
		if len(parts) > 0 {
			summary = strings.Join(parts, ": ")
		}
	}
	summary = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(summary, "�"))
	if len(summary) > 256 {
		end := 256
		for !utf8.RuneStart(summary[end]) {
			end--
		}
		summary = summary[:end] + "..."
	}
	return fmt.Sprintf("gc API: HTTP %d: %s", e.StatusCode, summary)
}

// Code returns the problem code, or an empty string when no problem was parsed.
func (e *HTTPError) Code() string {
	if e == nil || e.Problem == nil {
		return ""
	}
	return e.Problem.Code
}

// IsNoBinding reports whether gc rejected an outbound request with no binding.
// Over HTTP, only the missing group-route branch reaches this detail. An
// empty caller session is rejected earlier by request validation (HTTP 422).
func IsNoBinding(err error) bool {
	return strings.HasPrefix(validationDetail(err), "no active binding for conversation ")
}

// IsNoAdapter identifies a missing in-memory adapter registration after a gc
// restart. gc internal/extmsg/outbound.go emits "no adapter for <provider>/<account>",
// returned by the outbound API as 422 validation-failed. Only a missing current
// account can recover by re-registering; an intent from a renamed account cannot.
func IsNoAdapter(err error, accountID string) bool {
	return validationDetail(err) == "no adapter for gchat/"+accountID
}

// IsCityNotRunning identifies the supervisor's startup window before its city
// is running. gc internal/api/city_scope.go emits this 404 city-not-found detail
// for city-scoped requests, including adapter registration and outbound sends.
func IsCityNotRunning(err error) bool {
	var responseErr *HTTPError
	return errors.As(err, &responseErr) && responseErr != nil &&
		responseErr.StatusCode == http.StatusNotFound && responseErr.Code() == "city-not-found" &&
		strings.HasPrefix(responseErr.Problem.Detail, "not_found: city not found or not running: ")
}

// IsWrongOwner reports whether the outbound caller does not own the binding.
// gc internal/extmsg/outbound.go:109-110,129-130 uses the same detail prefix for
// agent and session bindings, with the caller session formatted using %q.
func IsWrongOwner(err error) bool {
	rest, ok := strings.CutPrefix(validationDetail(err), "session ")
	if !ok {
		return false
	}
	session, quoteErr := strconv.QuotedPrefix(rest)
	return quoteErr == nil && strings.HasPrefix(rest[len(session):], " does not own binding for conversation ")
}

func validationDetail(err error) string {
	var responseErr *HTTPError
	if errors.As(err, &responseErr) && responseErr != nil &&
		responseErr.StatusCode == http.StatusUnprocessableEntity && responseErr.Code() == "validation-failed" {
		return responseErr.Problem.Detail
	}
	return ""
}

func (c *Client) Register(ctx context.Context, reg wire.Registration) error {
	return c.call(ctx, http.MethodPost, "adapters", reg, nil)
}

func (c *Client) Inbound(ctx context.Context, m wire.InboundMessage) (wire.InboundResponse, error) {
	var result wire.InboundResponse
	err := c.call(ctx, http.MethodPost, "inbound", wire.InboundEnvelope{Message: m}, &result)
	return result, err
}

func (c *Client) Receipt(ctx context.Context, id string) (wire.ReceiptResponse, error) {
	var result wire.ReceiptResponse
	err := c.call(ctx, http.MethodGet, "inbound/receipts/"+url.PathEscape(id), nil, &result)
	return result, err
}

func (c *Client) Outbound(ctx context.Context, req wire.OutboundRequest) (wire.OutboundResult, error) {
	var result wire.OutboundResult
	err := c.call(ctx, http.MethodPost, "outbound", req, &result)
	return result, err
}

func (c *Client) call(ctx context.Context, method, path string, body, result any) error {
	var reader io.Reader
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("gc %s %s: encode request: %w", method, path, err)
		}
		reader = bytes.NewReader(data)
	}
	endpoint := c.base + "/v0/city/" + url.PathEscape(c.city) + "/extmsg/" + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("gc %s %s: create request: %w", method, path, err)
	}
	req.Header.Set(wire.RequestHeader, "gc-gchat-adapter")
	req.Header.Set("Content-Type", "application/json")
	if method == http.MethodPost && path == "adapters" {
		// The same registration can be retried after a lost response or a
		// coalesced reconnect. A changed registration must get a new key.
		sum := sha256.Sum256(data)
		req.Header.Set("Idempotency-Key", fmt.Sprintf("gchat-register-%x", sum))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			c.onRefusedMu.RLock()
			fn := c.onRefused
			c.onRefusedMu.RUnlock()
			if fn != nil {
				fn(err)
			}
		}
		return fmt.Errorf("gc %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	var responseBody io.Reader = resp.Body
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		responseBody = io.LimitReader(resp.Body, 64<<10)
	}
	data, readErr := io.ReadAll(responseBody)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		err := &HTTPError{StatusCode: resp.StatusCode, Body: string(data), RetryAfter: resp.Header.Get("Retry-After")}
		var problem Problem
		if json.Unmarshal(data, &problem) == nil && (problem.Type != "" || problem.Title != "" || problem.Status != 0 || problem.Detail != "" || problem.Code != "" || len(problem.Errors) > 0) {
			err.Problem = &problem
		}
		if readErr != nil {
			return fmt.Errorf("%w (read response: %w)", err, readErr)
		}
		return err
	}
	if readErr != nil {
		return fmt.Errorf("gc %s %s: read response: %w", method, path, readErr)
	}
	if result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			return fmt.Errorf("gc %s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}
