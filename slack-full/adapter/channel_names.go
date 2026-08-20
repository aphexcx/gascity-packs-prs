package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// --- inbound: Slack channel display-name resolution (gp-729 item 4) --------
//
// The delivery wrapper blocks the adapter renders (coalesced-burst
// headers, the once-per-channel reply how-to, the alias-dispatch
// reminder) name channels by raw id (C0BKF28CYUE). Humans and agents
// both think in "#fundraising-dataroom", so wherever the pack owns the
// wrapper text it renders "#name (Cid)" — id kept alongside because
// every reply/react CLI takes the id. Mirrors userNameCache
// (hq-uxln9): conversations.info with an in-memory TTL cache, raw id
// on any failure, nil cache keeps bare test configs network-inert.
// Requires the channels:read scope (groups:read for private channels);
// without it lookups fail and raw ids pass through unchanged.

// channelNameCacheTTL bounds how long a resolved channel name is
// reused. Renames are rare; an hour keeps lookups near zero.
const channelNameCacheTTL = time.Hour

// channelNameFailureTTL negative-caches a failed lookup so a burst on
// an unresolvable channel does not hammer conversations.info.
const channelNameFailureTTL = 5 * time.Minute

// channelInfoTimeout bounds the conversations.info call on the inbound
// dispatch path.
const channelInfoTimeout = 5 * time.Second

type cachedChannelName struct {
	name      string
	expiresAt time.Time
}

// channelNameCache is an in-memory conversations.info name cache,
// bounded by the workspace's channel population.
type channelNameCache struct {
	mu      sync.Mutex
	entries map[string]cachedChannelName
}

func newChannelNameCache() *channelNameCache {
	return &channelNameCache{entries: make(map[string]cachedChannelName)}
}

func (c *channelNameCache) get(channelID string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[channelID]
	if !ok || now.After(entry.expiresAt) {
		return "", false
	}
	return entry.name, true
}

func (c *channelNameCache) put(channelID, name string, now time.Time, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[channelID] = cachedChannelName{name: name, expiresAt: now.Add(ttl)}
}

// slackConversationInfoResp is the subset of a conversations.info
// response the resolver reads.
type slackConversationInfoResp struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Channel struct {
		Name string `json:"name"`
	} `json:"channel"`
}

// resolveChannelName resolves a channel id to its bare name (no '#').
// Returns "" when resolution is disabled (nil cache, empty token) or
// fails; callers fall back to the raw id. Failures are negative-cached.
func resolveChannelName(cfg config, channelID string) string {
	if channelID == "" || cfg.channelNames == nil || cfg.slackBotToken == "" {
		return ""
	}
	now := time.Now()
	if name, ok := cfg.channelNames.get(channelID, now); ok {
		return name
	}
	name := fetchChannelName(cfg, channelID)
	if name == "" {
		cfg.channelNames.put(channelID, "", now, channelNameFailureTTL)
		return ""
	}
	cfg.channelNames.put(channelID, name, now, channelNameCacheTTL)
	return name
}

// channelDisplay renders "#name (Cid)" when the name resolves and the
// bare id otherwise — the wrapper-line shape gp-729 item 4 asks for.
func channelDisplay(cfg config, channelID string) string {
	if name := resolveChannelName(cfg, channelID); name != "" {
		return "#" + name + " (" + channelID + ")"
	}
	return channelID
}

// fetchChannelName performs the conversations.info call, returning ""
// on any failure (logged with the reason). DMs and MPIMs have no
// operator-meaningful name field worth rendering; an empty name from
// Slack stays empty and the caller keeps the raw id.
func fetchChannelName(cfg config, channelID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), channelInfoTimeout)
	defer cancel()
	q := url.Values{}
	q.Set("channel", channelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, slackAPIBase+"/conversations.info?"+q.Encode(), nil)
	if err != nil {
		log.Printf("channel name lookup: build request for %s: %v", channelID, err)
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+cfg.slackBotToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("channel name lookup: conversations.info %s: %v", channelID, err)
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("channel name lookup: read body for %s: %v", channelID, err)
		return ""
	}
	if resp.StatusCode >= 300 {
		log.Printf("channel name lookup: conversations.info %s HTTP %d: %s", channelID, resp.StatusCode, clipBodyForLog(body))
		return ""
	}
	var ci slackConversationInfoResp
	if err := json.Unmarshal(body, &ci); err != nil {
		log.Printf("channel name lookup: decode for %s: %v", channelID, err)
		return ""
	}
	if !ci.OK {
		log.Printf("channel name lookup: conversations.info %s not ok: %s", channelID, ci.Error)
		return ""
	}
	return ci.Channel.Name
}
