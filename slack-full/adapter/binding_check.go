package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// --- alias-dispatch turn-dedup (gp-729 item 5) ------------------------------
//
// A targeted inbound (`@mayor: …`) in a channel whose bound session IS
// the alias target used to arrive twice: once through the channel
// binding (postInbound → gc extmsg fan-out) and once through
// dispatchToAliasedSession's direct session-message POST. gc delivers
// one copy as the session's prompt and the other as a mid-turn
// injection — the same message id twice in one turn, pure overhead.
//
// The fix consults gc's binding registry before alias-dispatching:
// when the aliased session already holds an active binding for the
// originating conversation, the channel copy is the delivery and the
// direct dispatch is suppressed (the `@handle:` address marker is
// restored into the channel copy's text so addressed-ness survives —
// see processSlackEvent). Lookup errors fail open to dispatching:
// duplicate delivery is overhead, a missed delivery is loss.
//
// Only NOT-BOUND verdicts are cached (briefly): a stale not-bound
// merely dispatches both copies — today's behavior — while a stale
// BOUND verdict after an unbind would suppress the addressed session's
// ONLY delivery. Suppression therefore re-verifies against gc on every
// targeted message; targeted messages are rare and gc is local.

// bindingCheckTTL bounds how long a not-bound verdict is reused.
const bindingCheckTTL = time.Minute

// bindingCheckTimeout bounds the gc bindings GET on the dispatch path.
const bindingCheckTimeout = 3 * time.Second

// bindingCheckMaxEntries caps the verdict cache; entries are keyed by
// (session, conversation) so the population is small in practice.
const bindingCheckMaxEntries = 1024

type bindingVerdict struct {
	bound     bool
	expiresAt time.Time
}

// bindingCheckCache caches session-bound-to-conversation verdicts.
// Nil-safe: a nil cache reports not-bound, preserving the historical
// double-dispatch behavior (and keeping bare test configs network-
// inert).
type bindingCheckCache struct {
	mu      sync.Mutex
	entries map[string]bindingVerdict
}

func newBindingCheckCache() *bindingCheckCache {
	return &bindingCheckCache{entries: make(map[string]bindingVerdict)}
}

// gcBindingRecord is the subset of a gc extmsg binding list item the
// checker reads. gc marshals Go struct fields without json tags, so
// the capitalized forms are canonical; lowercase fallbacks mirror the
// tolerance in slack_intake_common.interpret_publish_receipt.
type gcBindingRecord struct {
	Status       string            `json:"Status"`
	StatusLower  string            `json:"status"`
	Conversation gcBindingConvRef  `json:"Conversation"`
	ConvLower    *gcBindingConvRef `json:"conversation"`
}

type gcBindingConvRef struct {
	Provider       string `json:"Provider"`
	ProviderLower  string `json:"provider"`
	ConversationID string `json:"ConversationID"`
	ConvIDLower    string `json:"conversation_id"`
}

func (r gcBindingRecord) status() string {
	if r.Status != "" {
		return r.Status
	}
	return r.StatusLower
}

func (r gcBindingRecord) conversation() gcBindingConvRef {
	if r.ConvLower != nil {
		return *r.ConvLower
	}
	return r.Conversation
}

func (c gcBindingConvRef) provider() string {
	if c.Provider != "" {
		return c.Provider
	}
	return c.ProviderLower
}

func (c gcBindingConvRef) conversationID() string {
	if c.ConversationID != "" {
		return c.ConversationID
	}
	return c.ConvIDLower
}

// sessionBoundToConversation reports whether sessionID holds an active
// gc extmsg binding for the given provider conversation. Errors and
// nil caches report false (dispatch proceeds — fail open to duplicate,
// never to loss).
func sessionBoundToConversation(cfg config, sessionID, conversationID string) bool {
	if cfg.bindingCheck == nil || sessionID == "" || conversationID == "" {
		return false
	}
	key := sessionID + "|" + conversationID
	now := time.Now()
	cfg.bindingCheck.mu.Lock()
	if v, ok := cfg.bindingCheck.entries[key]; ok && now.Before(v.expiresAt) {
		cfg.bindingCheck.mu.Unlock()
		return v.bound
	}
	cfg.bindingCheck.mu.Unlock()

	bound, err := fetchSessionBinding(cfg, sessionID, conversationID)
	if err != nil {
		log.Printf("binding check: session=%s conv=%s: %v (dispatching both copies)", sessionID, conversationID, err)
		return false
	}
	if !bound {
		cfg.bindingCheck.mu.Lock()
		if len(cfg.bindingCheck.entries) >= bindingCheckMaxEntries {
			// Small map in practice; wholesale reset beats tracking order.
			cfg.bindingCheck.entries = make(map[string]bindingVerdict)
		}
		cfg.bindingCheck.entries[key] = bindingVerdict{bound: false, expiresAt: now.Add(bindingCheckTTL)}
		cfg.bindingCheck.mu.Unlock()
	}
	return bound
}

// fetchSessionBinding queries gc for the session's bindings and scans
// for an active one on (cfg.provider, conversationID).
func fetchSessionBinding(cfg config, sessionID, conversationID string) (bool, error) {
	target := fmt.Sprintf("%s/v0/city/%s/extmsg/bindings?session_id=%s",
		cfg.gcAPIBase, url.PathEscape(cfg.cityName), url.QueryEscape(sessionID))
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return false, err
	}
	client := &http.Client{Timeout: bindingCheckTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("GET %s -> %s: %s", target, resp.Status, clipBodyForLog(body))
	}
	var listing struct {
		Items      []gcBindingRecord `json:"items"`
		ItemsUpper []gcBindingRecord `json:"Items"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return false, fmt.Errorf("decode bindings: %w", err)
	}
	items := listing.Items
	if len(items) == 0 {
		items = listing.ItemsUpper
	}
	for _, item := range items {
		if item.status() != "active" {
			continue
		}
		conv := item.conversation()
		if conv.provider() == cfg.provider && conv.conversationID() == conversationID {
			return true, nil
		}
	}
	return false, nil
}
