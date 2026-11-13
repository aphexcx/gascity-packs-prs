// Package ack chooses only arrival reactions. The catalog is data, never a
// prompt supplied by a sender. Errors deliberately exclude request bodies,
// response bodies, URLs and credentials.
package ack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	MaxAge         = 60 * time.Second
	ChoiceBudget   = 1500 * time.Millisecond
	ReactionBudget = 2 * time.Second
	Concurrency    = 4
	Instructions   = "Pick the reaction that best acknowledges this chat message to an assistant. If two reactions fit, a request outranks a feeling; otherwise the narrower one wins."
)

type Row struct{ Name, Emoji, Meaning string }

var Default = Row{Name: "eyes", Emoji: "👀"}

type Options struct {
	Mode, Scope, Base, Model, Key string
	Catalog                       []byte
	Now                           func() time.Time
	After                         func(time.Duration) <-chan time.Time
	Log                           func(string)
	Redact                        func(string) string
}

type Choice struct {
	mode, scope, base, model, key string
	rows                          map[string]Row
	fixed                         Row
	now                           func() time.Time
	after                         func(time.Duration) <-chan time.Time
	redact                        func(string) string
	client                        *http.Client
	slots                         chan struct{}
}

// New reads a snapshot at startup. Invalid individual rows are omitted. An
// absent or unusable file leaves exactly the built-in default available.
func New(o Options) (*Choice, error) {
	c := &Choice{mode: o.Mode, scope: o.Scope, base: o.Base, model: o.Model, key: o.Key, rows: map[string]Row{}, now: o.Now, after: o.After, redact: o.Redact, slots: make(chan struct{}, Concurrency)}
	if c.now == nil {
		c.now = time.Now
	}
	if c.after == nil {
		c.after = time.After
	}
	if c.scope == "" {
		c.scope = "addressed"
	}
	if c.scope != "addressed" && c.scope != "all" {
		return nil, errors.New("ACK_SCOPE must be addressed or all")
	}
	if c.base == "" {
		c.base = "https://api.typesafe.ai"
	}
	if c.model == "" {
		c.model = "jev-latest"
	}
	if c.mode == "" {
		return c, nil
	}
	u, err := url.Parse(c.base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !(u.Scheme == "https" || u.Scheme == "http" && loopback(u.Hostname())) {
		return nil, errors.New("JEV_API_BASE_URL must use HTTPS or local HTTP, without credentials, query or fragment")
	}
	var doc struct {
		Reactions []json.RawMessage `json:"reactions"`
	}
	if json.Unmarshal(o.Catalog, &doc) == nil {
		names := map[string]bool{}
		for i, raw := range doc.Reactions {
			var r struct {
				Emoji   *string `json:"emoji"`
				Name    *string `json:"slack_name"`
				Meaning *string `json:"meaning"`
				May     *bool   `json:"adapter_may_place"`
			}
			if json.Unmarshal(raw, &r) != nil || r.Emoji == nil || r.Name == nil || r.Meaning == nil || r.May == nil || !*r.May || *r.Emoji == "" || *r.Name == "" || *r.Meaning == "" {
				continue
			}
			if names[*r.Name] {
				return nil, errors.New("ACK_REACTION: duplicate arrival name in reactions.json")
			}
			names[*r.Name] = true
			c.rows[fmt.Sprintf("option_%03d", i+1)] = Row{Name: *r.Name, Emoji: *r.Emoji, Meaning: *r.Meaning}
		}
	}
	if len(c.rows) > 255 {
		return nil, errors.New("ACK_REACTION: reactions.json exceeds 255 arrival choices")
	}
	if len(c.rows) == 0 && o.Log != nil {
		o.Log("ACK_REACTION: reactions.json unusable; using fixed eyes default")
	}
	if c.mode == "jev" {
		if c.key == "" && o.Log != nil {
			o.Log("JEV_API_KEY not set; using fixed eyes default")
		}
	} else {
		for _, r := range c.rows {
			if r.Name == c.mode {
				c.fixed = r
			}
		}
		if len(c.rows) == 0 && c.mode == Default.Name {
			c.fixed = Default
		}
		if c.fixed.Name == "" {
			return nil, errors.New("ACK_REACTION must be jev or an arrival name in reactions.json")
		}
	}
	c.client = &http.Client{Timeout: ChoiceBudget, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, nil
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
}
func (c *Choice) Enabled() bool { return c != nil && c.mode != "" }
func (c *Choice) All() bool     { return c != nil && c.scope == "all" }
func (c *Choice) KeyStatus() string {
	if c != nil && c.key != "" {
		return "set"
	}
	return "not set"
}
func (c *Choice) Names() []string {
	if !c.Enabled() {
		return nil
	}
	if c.mode != "jev" {
		return []string{c.fixed.Name}
	}
	names := []string{Default.Name}
	for _, r := range c.rows {
		names = append(names, r.Name)
	}
	return names
}

func (c *Choice) Choose(parent context.Context, text string, seen time.Time) (Row, string) {
	if !c.Enabled() {
		return Row{}, ""
	}
	if c.mode != "jev" {
		return c.fixed, "fixed"
	}
	if c.redact != nil {
		text = c.redact(text)
	}
	chars := []rune(text)
	if len(chars) > 1000 {
		chars = chars[:1000]
	}
	text = string(chars)
	remaining := ChoiceBudget - c.now().Sub(seen)
	if c.key == "" || len(c.rows) == 0 || strings.TrimSpace(text) == "" || remaining <= 0 {
		return Default, "default"
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	timer := c.after(remaining)
	select {
	case c.slots <- struct{}{}:
	case <-timer:
		return Default, "default"
	case <-ctx.Done():
		return Default, "default"
	}
	// The slot remains owned until the canceled HTTP request actually returns.
	// A slow transport cannot cause a fifth in-flight vendor request.
	result := make(chan Row, 1)
	go func() { defer func() { <-c.slots }(); result <- c.request(ctx, text) }()
	select {
	case r := <-result:
		if r.Name != "" {
			return r, "jev"
		}
	case <-timer:
	case <-ctx.Done():
	}
	return Default, "default"
}

func (c *Choice) request(ctx context.Context, text string) Row {
	criteria := map[string]string{}
	for key, r := range c.rows {
		criteria[key] = r.Meaning
	}
	body, _ := json.Marshal(map[string]any{"model": c.model, "state": text, "questions": map[string]any{"ack": map[string]any{"type": "choice", "instructions": Instructions, "criteria": criteria}}})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.base, "/")+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return Row{}
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return Row{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Row{}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(data) > 65536 {
		return Row{}
	}
	var answer struct {
		Answers struct {
			Ack struct {
				Choice string `json:"choice"`
			} `json:"ack"`
		} `json:"answers"`
	}
	if json.Unmarshal(data, &answer) != nil {
		return Row{}
	}
	return c.rows[answer.Answers.Ack.Choice]
}
