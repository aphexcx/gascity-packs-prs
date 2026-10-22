// Package gcclient implements the gc extmsg HTTP API. Delivery retries and
// receipt polling belong to callers, which can persist ambiguous outcomes.
package gcclient

import (
	"bytes"
	"context"
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
}

// HTTPError retains gc's status and up to 64 KiB of its response body.
// Problem is populated best-effort, regardless of the response content type.
type HTTPError struct {
	StatusCode int
	Body       string
	Problem    *Problem
}

func (e *HTTPError) Error() string {
	summary := e.Body
	if e.Problem != nil {
		summary = e.Problem.Code + ": " + e.Problem.Detail
	}
	if len(summary) > 256 {
		summary = summary[:256] + "..."
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
// gc internal/extmsg/outbound.go:137-146 emits this detail for both an absent
// caller session and a caller that has no matching group route.
func IsNoBinding(err error) bool {
	return strings.HasPrefix(bindingDetail(err), "no active binding for conversation ")
}

// IsWrongOwner reports whether the outbound caller does not own the binding.
// gc internal/extmsg/outbound.go:109-110,129-130 uses the same detail prefix for
// agent and session bindings, with the caller session formatted using %q.
func IsWrongOwner(err error) bool {
	rest, ok := strings.CutPrefix(bindingDetail(err), "session ")
	if !ok {
		return false
	}
	session, quoteErr := strconv.QuotedPrefix(rest)
	return quoteErr == nil && strings.HasPrefix(rest[len(session):], " does not own binding for conversation ")
}

func bindingDetail(err error) string {
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
	if body != nil {
		data, err := json.Marshal(body)
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
		err := &HTTPError{StatusCode: resp.StatusCode, Body: string(data)}
		var problem Problem
		if json.Unmarshal(data, &problem) == nil && problem != (Problem{}) {
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
