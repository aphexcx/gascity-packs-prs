// Package gcclient implements the gc extmsg HTTP API. Delivery retries and
// receipt polling belong to callers, which can persist ambiguous outcomes.
package gcclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gchat-adapter/internal/wire"
)

type Client struct {
	base, city string
	http       *http.Client
}

// New constructs a client for a gc API base URL and city. Each call respects
// its context and has a 35-second ceiling, allowing gc's 30-second publish
// callback deadline to expire before the client's own timeout.
func New(base, city string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"), city: city,
		http: &http.Client{
			Timeout: 35 * time.Second,
			// A redirect is a non-2xx response, not permission to replay a
			// mutation or send it to another endpoint.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// HTTPError retains gc's status and up to 64 KiB of its response body.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	summary := e.Body
	if len(summary) > 256 {
		summary = summary[:256] + "..."
	}
	return fmt.Sprintf("gc API: HTTP %d: %s", e.StatusCode, summary)
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
