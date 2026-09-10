package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// --- mention-only room bindings (jg-vobf70) ----------------------------------
//
// An ambient room binding (`gc slack bind-room C session`) makes the session
// a member of gc's conversation group for the channel, and gc then wakes
// EVERY member with a system reminder for EVERY inbound the adapter forwards.
// gc has no per-member delivery filter, so a session that only wants to be
// woken when it is addressed cannot be an ambient member at all — and an
// unbound room delivers nothing to it, not even @mentions of its own bot
// user (live finding, C0B2Y13DRMK 2026-09-10: three @mentions of the mayor's
// bot in an unbound channel, zero wakes).
//
// Mention-only mode is therefore an ADAPTER-SIDE lane that runs beside the
// gc fan-out instead of through it:
//
//   - `gc slack bind-room C session --mentions-only` registers (channel,
//     session) here via POST /mention-only, and does NOT add the session
//     as a gc participant. Ambient members of the same room are untouched:
//     the channel copy still goes to gc exactly as before, and gc still
//     wakes them.
//   - For every routable human message in the channel, processSlackEvent
//     asks selectMentionOnlyTargets which registered sessions qualify and
//     injects ONE system reminder per qualifying session through gc's
//     session-message endpoint (the same transport the `@handle:` alias
//     dispatcher uses). Everything else in the room is dropped for that
//     session: no wake, no reminder. The room stays readable on demand via
//     `gc slack read`.
//
// A message qualifies for a mention-only session when:
//
//   (a) it @mentions the adapter's bot user (or arrives as app_mention),
//       or its parsed address handle — `@handle:` prefix, a Slack User
//       Group mention, or a thread-sticky handle — is the binding's handle
//       or resolves through the handle-alias registry to the session; or
//   (b) it is a reply in a thread whose root or an earlier reply was
//       posted by that session through this adapter (own-thread registry,
//       fed by /publish and /publish-file), so follow-ups to the session's
//       own posts still arrive. For threads that predate the registry
//       (adapter upgrades) the conversations.replies fetch the thread
//       preamble already performs is scanned for the adapter's own bot
//       user; a hit admits every mention-only session on the channel,
//       because a bot-authored reply carries no session identity.
//
// When the `@handle:` alias leg already injects the message into the same
// session (aliasedSessionID matches and the alias dispatch is not
// suppressed), the mention-only lane skips it — one delivery per session
// per message. Slack's message + app_mention twin pair is collapsed by a
// per-(session, channel, ts) claim on the shared claims cache.

// mentionOnlyBinding is one (channel, session) registration. SessionID is
// the identifier the injection is addressed to (gc session id or name —
// gc's session-message endpoint accepts either); SessionName is the
// alternate identifier so own-post matching works whichever form the
// publish request carried; Handle is the participant handle a human can
// address with the `@handle:` prefix.
type mentionOnlyBinding struct {
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name,omitempty"`
	Handle      string `json:"handle,omitempty"`
}

// matchesSession reports whether id names this binding's session under
// either identifier.
func (b mentionOnlyBinding) matchesSession(id string) bool {
	if id == "" {
		return false
	}
	return id == b.SessionID || (b.SessionName != "" && id == b.SessionName)
}

type mentionOnlyDiskFile struct {
	Version  int                             `json:"version"`
	Channels map[string][]mentionOnlyBinding `json:"channels"`
}

// maxMentionOnlyRegistryBytes bounds the on-disk registry read.
const maxMentionOnlyRegistryBytes = 4 << 20

// mentionOnlyRegistry maps channel id → mention-only bindings. Written by
// the /mention-only admin endpoints (POST/DELETE), persisted as JSON so an
// adapter restart keeps the bindings. Nil-safe: a nil registry has no
// bindings, which keeps every existing test config network-inert.
type mentionOnlyRegistry struct {
	mu       sync.Mutex
	diskPath string
	channels map[string][]mentionOnlyBinding
}

func newMentionOnlyRegistry(diskPath string) (*mentionOnlyRegistry, error) {
	r := &mentionOnlyRegistry{
		diskPath: diskPath,
		channels: make(map[string][]mentionOnlyBinding),
	}
	if err := r.load(); err != nil {
		return nil, fmt.Errorf("load mention-only registry from %s: %w", diskPath, err)
	}
	return r, nil
}

func (r *mentionOnlyRegistry) load() error {
	if r.diskPath == "" {
		return nil
	}
	f, err := os.Open(r.diskPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open %s: %w", r.diskPath, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxMentionOnlyRegistryBytes+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", r.diskPath, err)
	}
	if int64(len(data)) > maxMentionOnlyRegistryBytes {
		return fmt.Errorf("registry file %s exceeds %d bytes", r.diskPath, maxMentionOnlyRegistryBytes)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var stored mentionOnlyDiskFile
	if err := json.Unmarshal(data, &stored); err != nil {
		// A corrupt file must not take the adapter (every Slack lane) down,
		// but unlike the thread-session cache these bindings are operator
		// intent and are NOT rebuildable — so the file is moved aside
		// intact for repair rather than overwritten from the empty
		// in-memory state the adapter starts with.
		aside := fmt.Sprintf("%s.corrupt-%d", r.diskPath, time.Now().Unix())
		if mvErr := os.Rename(r.diskPath, aside); mvErr != nil {
			return fmt.Errorf("malformed JSON in %s (%v) and could not move it aside (%v); repair or remove the file", r.diskPath, err, mvErr)
		}
		log.Printf("WARN: mention-only registry: malformed file %q (%v); moved to %q and starting EMPTY — re-run `gc slack bind-room … --mentions-only` for each binding", r.diskPath, err, aside)
		return nil
	}
	for channel, list := range stored.Channels {
		channel = strings.TrimSpace(channel)
		if channel == "" {
			continue
		}
		for _, b := range list {
			b.SessionID = strings.TrimSpace(b.SessionID)
			if b.SessionID == "" {
				log.Printf("WARN: mention-only registry: skipping record with empty session_id in channel %s", channel)
				continue
			}
			r.upsertLocked(channel, b)
		}
	}
	return nil
}

func (r *mentionOnlyRegistry) upsertLocked(channel string, b mentionOnlyBinding) {
	list := r.channels[channel]
	for i := range list {
		if list[i].SessionID == b.SessionID {
			list[i] = b
			r.channels[channel] = list
			return
		}
	}
	r.channels[channel] = append(list, b)
}

func (r *mentionOnlyRegistry) saveLocked() error {
	if r.diskPath == "" {
		return nil
	}
	out := mentionOnlyDiskFile{Version: 1, Channels: make(map[string][]mentionOnlyBinding, len(r.channels))}
	for channel, list := range r.channels {
		if len(list) == 0 {
			continue
		}
		cp := make([]mentionOnlyBinding, len(list))
		copy(cp, list)
		out.Channels[channel] = cp
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("encode mention-only registry: %w", err)
	}
	return writeFile0600WithSync(r.diskPath, data)
}

// Set upserts a binding for channel (keyed by SessionID) and persists.
func (r *mentionOnlyRegistry) Set(channel string, b mentionOnlyBinding) error {
	if r == nil {
		return errors.New("mention-only registry not configured")
	}
	channel = strings.TrimSpace(channel)
	b.SessionID = strings.TrimSpace(b.SessionID)
	b.SessionName = strings.TrimSpace(b.SessionName)
	b.Handle = strings.TrimSpace(strings.TrimPrefix(b.Handle, "@"))
	if channel == "" || b.SessionID == "" {
		return errors.New("channel_id and session_id are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.channels[channel]
	prevCopy := make([]mentionOnlyBinding, len(prev))
	copy(prevCopy, prev)
	r.upsertLocked(channel, b)
	if err := r.saveLocked(); err != nil {
		r.channels[channel] = prevCopy
		return err
	}
	return nil
}

// Delete removes the binding for (channel, sessionID). existed reports
// whether anything was removed; a miss is not an error.
func (r *mentionOnlyRegistry) Delete(channel, sessionID string) (existed bool, err error) {
	if r == nil {
		return false, errors.New("mention-only registry not configured")
	}
	channel = strings.TrimSpace(channel)
	sessionID = strings.TrimSpace(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.channels[channel]
	kept := list[:0:0]
	for _, b := range list {
		if b.SessionID == sessionID || (b.SessionName != "" && b.SessionName == sessionID) {
			existed = true
			continue
		}
		kept = append(kept, b)
	}
	if !existed {
		return false, nil
	}
	if len(kept) == 0 {
		delete(r.channels, channel)
	} else {
		r.channels[channel] = kept
	}
	if err := r.saveLocked(); err != nil {
		r.channels[channel] = list
		return true, err
	}
	return true, nil
}

// ForChannel returns a copy of the bindings registered for channel.
func (r *mentionOnlyRegistry) ForChannel(channel string) []mentionOnlyBinding {
	if r == nil || channel == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.channels[channel]
	if len(list) == 0 {
		return nil
	}
	out := make([]mentionOnlyBinding, len(list))
	copy(out, list)
	return out
}

// All returns a copy of every binding, keyed by channel.
func (r *mentionOnlyRegistry) All() map[string][]mentionOnlyBinding {
	out := make(map[string][]mentionOnlyBinding)
	if r == nil {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for channel, list := range r.channels {
		cp := make([]mentionOnlyBinding, len(list))
		copy(cp, list)
		out[channel] = cp
	}
	return out
}

// --- own-thread registry ------------------------------------------------------

// ownThreadRecord remembers which sessions posted (through this adapter)
// into the thread rooted at (ChannelID, RootTS).
type ownThreadRecord struct {
	ChannelID string    `json:"channel_id"`
	RootTS    string    `json:"root_ts"`
	Sessions  []string  `json:"sessions"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ownThreadMaxEntries bounds the registry; the oldest-updated records are
// evicted past it. Thread follow-ups older than the eviction horizon fall
// back to the conversations.replies scan in processSlackEvent.
const ownThreadMaxEntries = 4000

const maxOwnThreadRegistryBytes = 8 << 20

// ownThreadRegistry is fed by /publish and /publish-file on every delivered
// post and consulted by the mention-only lane for thread replies. Persisted
// as JSON so an adapter restart keeps the map. Nil-safe: a nil registry
// records nothing and reports no posters.
type ownThreadRegistry struct {
	mu       sync.Mutex
	diskPath string
	entries  map[string]*ownThreadRecord
}

func ownThreadKey(channel, rootTS string) string {
	return channel + "|" + rootTS
}

func newOwnThreadRegistry(diskPath string) (*ownThreadRegistry, error) {
	r := &ownThreadRegistry{diskPath: diskPath, entries: make(map[string]*ownThreadRecord)}
	if err := r.load(); err != nil {
		return nil, fmt.Errorf("load own-thread registry from %s: %w", diskPath, err)
	}
	return r, nil
}

func (r *ownThreadRegistry) load() error {
	if r.diskPath == "" {
		return nil
	}
	f, err := os.Open(r.diskPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open %s: %w", r.diskPath, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxOwnThreadRegistryBytes+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", r.diskPath, err)
	}
	if int64(len(data)) > maxOwnThreadRegistryBytes {
		return fmt.Errorf("registry file %s exceeds %d bytes", r.diskPath, maxOwnThreadRegistryBytes)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var stored []ownThreadRecord
	if err := json.Unmarshal(data, &stored); err != nil {
		// Rebuildable cache (every new post re-records): a corrupt file
		// costs at most the thread-follow-up fallback to the Slack scan.
		log.Printf("WARN: own-thread registry: malformed file %q (%v); starting empty", r.diskPath, err)
		return nil
	}
	for i := range stored {
		rec := stored[i]
		if rec.ChannelID == "" || rec.RootTS == "" {
			continue
		}
		cp := rec
		r.entries[ownThreadKey(rec.ChannelID, rec.RootTS)] = &cp
	}
	return nil
}

func (r *ownThreadRegistry) saveLocked() error {
	if r.diskPath == "" {
		return nil
	}
	out := make([]ownThreadRecord, 0, len(r.entries))
	for _, rec := range r.entries {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ChannelID != out[j].ChannelID {
			return out[i].ChannelID < out[j].ChannelID
		}
		return out[i].RootTS < out[j].RootTS
	})
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("encode own-thread registry: %w", err)
	}
	return writeFile0600WithSync(r.diskPath, data)
}

// record notes that sessionID posted into the thread rooted at rootTS in
// channel. Best-effort: a persistence failure is logged and the in-memory
// record stands.
func (r *ownThreadRegistry) record(channel, rootTS, sessionID string) {
	if r == nil || channel == "" || rootTS == "" {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	key := ownThreadKey(channel, rootTS)
	rec, ok := r.entries[key]
	if !ok {
		rec = &ownThreadRecord{ChannelID: channel, RootTS: rootTS}
		r.entries[key] = rec
	}
	if sessionID != "" {
		found := false
		for _, s := range rec.Sessions {
			if s == sessionID {
				found = true
				break
			}
		}
		if !found {
			rec.Sessions = append(rec.Sessions, sessionID)
		}
	}
	rec.UpdatedAt = time.Now().UTC()
	for len(r.entries) > ownThreadMaxEntries {
		oldestKey := ""
		var oldest time.Time
		for k, e := range r.entries {
			if oldestKey == "" || e.UpdatedAt.Before(oldest) {
				oldestKey, oldest = k, e.UpdatedAt
			}
		}
		delete(r.entries, oldestKey)
	}
	if err := r.saveLocked(); err != nil {
		log.Printf("own-thread registry: persist failed (in-memory record kept): %v", err)
	}
}

// posters returns the sessions on record for the thread rooted at rootTS
// (a copy; nil when the thread is unknown). An entry with an empty
// session list means "posted, but by an unidentified session".
func (r *ownThreadRegistry) posters(channel, rootTS string) (sessions []string, known bool) {
	if r == nil || channel == "" || rootTS == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.entries[ownThreadKey(channel, rootTS)]
	if !ok {
		return nil, false
	}
	out := make([]string, len(rec.Sessions))
	copy(out, rec.Sessions)
	return out, true
}

// --- delivery log (for the pack scripts' reply anchoring) ----------------------

// mentionOnlyDelivery is one injection the lane made. The pack scripts'
// latest-inbound resolution (reply-current --thread-current, react, upload
// --thread-current) reads gc's extmsg.inbound events — which gc only emits
// for its own members — so the lane keeps its own recent deliveries and
// serves them on GET /mention-only/deliveries. In-memory only: the
// reminder body carries the explicit reply command as the durable path.
type mentionOnlyDelivery struct {
	SessionID  string    `json:"session_id"`
	ChannelID  string    `json:"channel_id"`
	TS         string    `json:"ts"`
	ThreadTS   string    `json:"thread_ts,omitempty"`
	Reason     string    `json:"reason"`
	ReceivedAt time.Time `json:"received_at"`
}

const mentionOnlyDeliveryLogPerSession = 50

type mentionOnlyDeliveryLog struct {
	mu        sync.Mutex
	bySession map[string][]mentionOnlyDelivery // newest first
}

func newMentionOnlyDeliveryLog() *mentionOnlyDeliveryLog {
	return &mentionOnlyDeliveryLog{bySession: make(map[string][]mentionOnlyDelivery)}
}

// record files d under every identifier the binding is known by.
func (l *mentionOnlyDeliveryLog) record(b mentionOnlyBinding, d mentionOnlyDelivery) {
	if l == nil {
		return
	}
	keys := []string{b.SessionID}
	if b.SessionName != "" && b.SessionName != b.SessionID {
		keys = append(keys, b.SessionName)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		list := append([]mentionOnlyDelivery{d}, l.bySession[k]...)
		if len(list) > mentionOnlyDeliveryLogPerSession {
			list = list[:mentionOnlyDeliveryLogPerSession]
		}
		l.bySession[k] = list
	}
}

// remove drops the (channel, ts) record under every identifier the
// binding is known by — the rollback for an injection gc did not accept.
func (l *mentionOnlyDeliveryLog) remove(b mentionOnlyBinding, channel, ts string) {
	if l == nil {
		return
	}
	keys := []string{b.SessionID}
	if b.SessionName != "" && b.SessionName != b.SessionID {
		keys = append(keys, b.SessionName)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		list := l.bySession[k]
		kept := list[:0:0]
		for _, d := range list {
			if d.ChannelID == channel && d.TS == ts {
				continue
			}
			kept = append(kept, d)
		}
		l.bySession[k] = kept
	}
}

// forSession returns the session's recent deliveries, newest first.
func (l *mentionOnlyDeliveryLog) forSession(id string) []mentionOnlyDelivery {
	if l == nil || id == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	list := l.bySession[id]
	out := make([]mentionOnlyDelivery, len(list))
	copy(out, list)
	return out
}

// --- target selection ----------------------------------------------------------

const (
	mentionOnlyReasonBotMention = "bot_mention"
	mentionOnlyReasonHandle     = "handle"
	mentionOnlyReasonOwnThread  = "own_thread"
)

// mentionOnlyInput is everything selectMentionOnlyTargets needs, gathered
// by processSlackEvent so the rule itself stays a pure function.
type mentionOnlyInput struct {
	bindings     []mentionOnlyBinding
	botMentioned bool
	// target is the parsed address handle ("" when none): `@handle:`
	// prefix, labeled/unlabeled User Group mention, or thread-sticky.
	target string
	// aliasedSessionID is aliasReg.Get(target) ("" on miss).
	aliasedSessionID string
	// aliasLegDelivers is true when the alias dispatcher will inject this
	// message into aliasedSessionID itself (target resolved and the
	// dispatch was not suppressed by an existing gc binding).
	aliasLegDelivers bool
	// isThreadReply is msg.ThreadTS != "" && msg.ThreadTS != msg.TS.
	isThreadReply bool
	// threadPosters are the sessions on record for the thread root
	// (own-thread registry); threadKnown says the registry had an entry.
	threadPosters []string
	threadKnown   bool
	// botPostedInThread is the Slack-scan fallback verdict: an earlier
	// message in the thread was authored by the adapter's own bot user.
	// Consulted only when the registry has no entry for the thread.
	botPostedInThread bool
}

type mentionOnlyTarget struct {
	binding mentionOnlyBinding
	reason  string
}

// selectMentionOnlyTargets applies the delivery rules documented at the
// top of this file. Order of the result follows the binding order.
func selectMentionOnlyTargets(in mentionOnlyInput) []mentionOnlyTarget {
	var out []mentionOnlyTarget
	for _, b := range in.bindings {
		reason := ""
		switch {
		case in.botMentioned:
			reason = mentionOnlyReasonBotMention
		case in.target != "" && ((b.Handle != "" && strings.EqualFold(in.target, b.Handle)) ||
			(in.aliasedSessionID != "" && b.matchesSession(in.aliasedSessionID))):
			reason = mentionOnlyReasonHandle
		case in.isThreadReply && threadPostedBySession(in, b):
			reason = mentionOnlyReasonOwnThread
		}
		if reason == "" {
			continue
		}
		if in.aliasLegDelivers && b.matchesSession(in.aliasedSessionID) {
			// The alias injection IS this session's delivery.
			continue
		}
		out = append(out, mentionOnlyTarget{binding: b, reason: reason})
	}
	return out
}

func threadPostedBySession(in mentionOnlyInput, b mentionOnlyBinding) bool {
	if in.threadKnown {
		for _, s := range in.threadPosters {
			if b.matchesSession(s) {
				return true
			}
		}
		return false
	}
	return in.botPostedInThread
}

// threadHasOwnBotPost reports whether any message in replies that precedes
// currentTS (the thread root included) was authored by botUserID. Slack's
// conversations.replies returns oldest-first; ts values are decimal
// strings of equal epoch width, so a lexical comparison orders them.
func threadHasOwnBotPost(replies []slackThreadMessage, botUserID, currentTS string) bool {
	if botUserID == "" {
		return false
	}
	for _, m := range replies {
		if m.TS == "" || m.TS == currentTS || (currentTS != "" && m.TS > currentTS) {
			continue
		}
		if m.User == botUserID {
			return true
		}
	}
	return false
}

// --- injection ---------------------------------------------------------------

func mentionOnlyDeliveryClaimKey(sessionID, channel, ts string) string {
	return "mention-only|" + sessionID + "|" + channel + "|" + ts
}

// mentionOnlyReasonLine renders the reason for the reminder head.
func mentionOnlyReasonLine(reason, handle string) string {
	switch reason {
	case mentionOnlyReasonBotMention:
		return "you were @mentioned"
	case mentionOnlyReasonHandle:
		if handle != "" {
			return "@" + handle + " addressed you"
		}
		return "your handle was addressed"
	case mentionOnlyReasonOwnThread:
		return "a reply landed in a thread you posted in"
	}
	return reason
}

// formatMentionOnlyReminder renders the system reminder the lane injects.
// Every interpolated field is neutralized so a workspace member cannot
// forge a </system-reminder> boundary (cby.33 contract, same as the
// alias dispatcher). Exposed for tests.
func formatMentionOnlyReminder(cfg config, msg externalInboundMessage, reason, handle string) string {
	attachmentsBlock := ""
	if len(msg.Attachments) > 0 {
		var ab strings.Builder
		fmt.Fprintf(&ab, "\nAttachments (%d) — saved to local disk; Read the file:// path to view:\n", len(msg.Attachments))
		for i, att := range msg.Attachments {
			name := filepath.Base(strings.TrimPrefix(att.URL, "file://"))
			fmt.Fprintf(&ab, "  %d. %s (%s): %s\n",
				i+1,
				neutralizeMarkupBoundaries(name),
				neutralizeMarkupBoundaries(att.MIMEType),
				neutralizeMarkupBoundaries(att.URL))
		}
		attachmentsBlock = ab.String()
	}
	sender := msg.Actor.ID
	if msg.Actor.DisplayName != "" && msg.Actor.DisplayName != msg.Actor.ID {
		sender = msg.Actor.DisplayName + " (" + msg.Actor.ID + ")"
	}
	tsContext := neutralizeMarkupBoundaries(msg.ProviderMessageID)
	replyThreadTS := msg.ProviderMessageID
	if msg.ReplyToMessageID != "" && msg.ReplyToMessageID != msg.ProviderMessageID {
		tsContext += ", a reply in thread " + neutralizeMarkupBoundaries(msg.ReplyToMessageID)
		replyThreadTS = msg.ReplyToMessageID
	}
	channelID := neutralizeMarkupBoundaries(msg.Conversation.ConversationID)
	return fmt.Sprintf(
		"<system-reminder>\n"+
			"Slack mention-only room delivery: %s in channel %s (Slack ts %s) by user %s.\n"+
			"\n"+
			"Message text:\n"+
			"%s\n"+
			"%s"+
			"\n"+
			"You are bound to this room in mention-only mode: only @mentions of your bot user, messages addressed to your handle, and replies in threads you posted in reach you. Read the rest of the room on demand:\n"+
			"  gc slack read --conversation-id %s\n"+
			"\n"+
			"React to this message with writing_hand to signal you are actively working on it:\n"+
			"  gc slack react --conversation-id %s --message-id %s --emoji writing_hand\n"+
			"\n"+
			"To reply in that thread, write your reply to a tmpfile and run:\n"+
			"  gc slack publish-to-channel \\\n"+
			"    --conversation-id %s \\\n"+
			"    --thread-ts %s \\\n"+
			"    --body-file <tmpfile>\n"+
			"\n"+
			"This posts directly through the slack adapter with your registered identity (you hold no channel binding here, and it is unaffected by any company-room pointer your session may carry). `gc slack react`, `reply-current --thread-current` and `upload --thread-current` also resolve this delivery on their own.\n"+
			"</system-reminder>",
		mentionOnlyReasonLine(reason, neutralizeMarkupBoundaries(handle)),
		neutralizeMarkupBoundaries(channelDisplay(cfg, msg.Conversation.ConversationID)),
		tsContext,
		neutralizeMarkupBoundaries(sender),
		neutralizeMarkupBoundaries(msg.Text),
		attachmentsBlock,
		channelID,
		channelID,
		neutralizeMarkupBoundaries(msg.ProviderMessageID),
		channelID,
		neutralizeMarkupBoundaries(replyThreadTS),
	)
}

// postMentionOnlyReminder injects body into sessionID via gc's session-
// message endpoint. Returns gc's delivery receipt (zero when gc emits
// none) and whether gc ACCEPTED the injection — mirrors
// dispatchToAliasedSession's transport contract.
func postMentionOnlyReminder(cfg config, sessionID, body string) (deliveryReceipt, bool) {
	payload, _ := json.Marshal(gcSessionMessageRequest{Message: body})
	target := fmt.Sprintf("%s/v0/city/%s/session/%s/messages",
		cfg.gcAPIBase, url.PathEscape(cfg.cityName), url.PathEscape(sessionID))
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		log.Printf("mention-only dispatch: build request: %v", err)
		return deliveryReceipt{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "gc-slack-adapter-mention-only")
	req.Header.Set("X-GC-Delivery-Receipt", "require")
	resp, err := gcForwardClient.Do(req)
	if err != nil {
		log.Printf("mention-only dispatch: POST %s: %v", target, err)
		return deliveryReceipt{}, false
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, deliveryReceiptBodyLimit))
	if resp.StatusCode >= 400 {
		log.Printf("mention-only dispatch: %s -> %s: %s", target, resp.Status, string(respBody))
		return deliveryReceipt{}, false
	}
	if readErr != nil {
		log.Printf("mention-only dispatch: session=%s accepted; receipt unreadable: %v", sessionID, readErr)
		return deliveryReceipt{}, true
	}
	return parseDeliveryReceipt(respBody), true
}

// ownThreadScanLimit is the conversations.replies window the own-thread
// fallback scan reads when the preamble's (context-limit-sized) fetch did
// not run or may have been truncated. Slack pages oldest-first, so a bot
// reply deep in a long thread needs the wider window (codex r1 P2).
// Threads longer than this without a registry entry still read as
// "not posted"; the registry is the primary source.
const ownThreadScanLimit = 200

// mentionOnlyScanMinRefetch is the reply count at which a preamble fetch
// capped at limit may have been truncated.
func mentionOnlyScanMinRefetch(limit int) int {
	if limit <= 0 {
		limit = defaultThreadContextLimit
	}
	return limit
}

// recordAliasDeliveryForMentionOnly logs an alias-leg injection under any
// mention-only binding for the same session, so the pack scripts' latest-
// inbound resolution sees it (the alias leg skipped the lane's own
// injection, and no gc event exists for a non-member; codex r1 P2).
func recordAliasDeliveryForMentionOnly(cfg config, bindings []mentionOnlyBinding, aliasedSessionID string, inbound externalInboundMessage) {
	if cfg.mentionOnlyDeliveries == nil || aliasedSessionID == "" {
		return
	}
	for _, b := range bindings {
		if !b.matchesSession(aliasedSessionID) {
			continue
		}
		cfg.mentionOnlyDeliveries.record(b, mentionOnlyDelivery{
			SessionID:  b.SessionID,
			ChannelID:  inbound.Conversation.ConversationID,
			TS:         inbound.ProviderMessageID,
			ThreadTS:   inbound.ReplyToMessageID,
			Reason:     mentionOnlyReasonHandle,
			ReceivedAt: inbound.ReceivedAt,
		})
	}
}

// forgetAliasDeliveryForMentionOnly is the rollback of
// recordAliasDeliveryForMentionOnly for an alias injection gc rejected.
func forgetAliasDeliveryForMentionOnly(cfg config, bindings []mentionOnlyBinding, aliasedSessionID string, inbound externalInboundMessage) {
	if cfg.mentionOnlyDeliveries == nil || aliasedSessionID == "" {
		return
	}
	for _, b := range bindings {
		if b.matchesSession(aliasedSessionID) {
			cfg.mentionOnlyDeliveries.remove(b, inbound.Conversation.ConversationID, inbound.ProviderMessageID)
		}
	}
}

// deliverMentionOnly injects one reminder per target, synchronously, with
// a per-(session, channel, ts) claim collapsing Slack's twin deliveries.
// A failed injection releases its claim (a parked twin or Slack redelivery
// retries it), logs, and marks the message with ⚠️ so the human sees the
// session was not reached. Returns the number of injections gc accepted
// and the number that failed (skipped twins count as neither).
func deliverMentionOnly(cfg config, targets []mentionOnlyTarget, inbound externalInboundMessage) (delivered, failed int) {
	channel := inbound.Conversation.ConversationID
	ts := inbound.ProviderMessageID
	for _, t := range targets {
		key := mentionOnlyDeliveryClaimKey(t.binding.SessionID, channel, ts)
		proceed, wait := cfg.channelClaims.begin(key)
		skipped := false
		for !proceed {
			if wait == nil {
				log.Printf("mention-only: session=%s chan=%s ts=%s already delivered by same-ts twin — skipped",
					t.binding.SessionID, channel, ts)
				skipped = true
				break
			}
			<-wait
			proceed, wait = cfg.channelClaims.begin(key)
		}
		if skipped {
			continue
		}
		body := formatMentionOnlyReminder(cfg, inbound, t.reason, t.binding.Handle)
		// The anchor is logged BEFORE the POST (codex r3 P2): gc can hand
		// the reminder to the session — and the session can run the
		// prescribed react/reply — before the HTTP response returns here.
		// A rejected injection rolls it back below.
		record := mentionOnlyDelivery{
			SessionID:  t.binding.SessionID,
			ChannelID:  channel,
			TS:         ts,
			ThreadTS:   inbound.ReplyToMessageID,
			Reason:     t.reason,
			ReceivedAt: inbound.ReceivedAt,
		}
		cfg.mentionOnlyDeliveries.record(t.binding, record)
		receipt, ok := postMentionOnlyReminder(cfg, t.binding.SessionID, body)
		verdict := receipt.verdict(cfg.deliveryReceiptGate)
		for attempt := 0; ok && verdict == receiptUnconfirmed && attempt < deliveryReceiptRepostAttempts && receiptRepostAllowed(cfg); attempt++ {
			log.Printf("mention-only: session=%s chan=%s ts=%s gc did not vouch for the injection (%s) — re-dispatching in place (attempt %d/%d)",
				t.binding.SessionID, channel, ts, receipt.logField(verdict), attempt+1, deliveryReceiptRepostAttempts)
			receipt, ok = postMentionOnlyReminder(cfg, t.binding.SessionID, body)
			verdict = receipt.verdict(cfg.deliveryReceiptGate)
		}
		if ok && verdict == receiptUnconfirmed {
			log.Printf("mention-only: UNDELIVERED session=%s chan=%s ts=%s gc accepted the injection but did not vouch that it reached the session — %s",
				t.binding.SessionID, channel, ts, receipt.logField(verdict))
			ok = false
		}
		if !ok {
			cfg.mentionOnlyDeliveries.remove(t.binding, channel, ts)
			cfg.channelClaims.forget(key)
			log.Printf("mention-only: FAILED session=%s chan=%s ts=%s reason=%s — claim released for a twin/redelivery retry",
				t.binding.SessionID, channel, ts, t.reason)
			reactMentionOnlyDispatchFailure(cfg.slackBotToken, channel, ts)
			failed++
			continue
		}
		cfg.channelClaims.commit(key)
		log.Printf("mention-only: delivered session=%s chan=%s ts=%s thread=%s reason=%s %s",
			t.binding.SessionID, channel, ts, inbound.ReplyToMessageID, t.reason, receipt.logField(verdict))
		delivered++
	}
	return delivered, failed
}

// reactMentionOnlyDispatchFailure posts ⚠️ on the message whose mention-only
// injection failed. Best-effort, like the alias dispatcher's marker.
func reactMentionOnlyDispatchFailure(token, channelID, ts string) {
	if token == "" {
		return
	}
	resp, err := postReactionToSlack(token, slackReactionsAddReq{
		Channel:   channelID,
		Name:      "warning",
		Timestamp: ts,
	})
	if err != nil {
		log.Printf("react warning (mention-only dispatch failure): chan=%s ts=%s: %v", channelID, ts, err)
		return
	}
	if !resp.OK {
		log.Printf("react warning (mention-only dispatch failure): chan=%s ts=%s: slack error=%s", channelID, ts, resp.Error)
	}
}

// --- admin endpoints -----------------------------------------------------------

type mentionOnlyRequest struct {
	ChannelID   string `json:"channel_id"`
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name,omitempty"`
	Handle      string `json:"handle,omitempty"`
}

type mentionOnlyReceipt struct {
	Stored    bool               `json:"stored"`
	Removed   bool               `json:"removed,omitempty"`
	Existed   bool               `json:"existed,omitempty"`
	ChannelID string             `json:"channel_id"`
	Binding   mentionOnlyBinding `json:"binding,omitempty"`
}

// handleMentionOnlyBind serves POST /mention-only.
func handleMentionOnlyBind(reg *mentionOnlyRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req mentionOnlyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
			return
		}
		channel := strings.TrimSpace(req.ChannelID)
		b := mentionOnlyBinding{SessionID: req.SessionID, SessionName: req.SessionName, Handle: req.Handle}
		if channel == "" || strings.TrimSpace(b.SessionID) == "" {
			http.Error(w, "channel_id and session_id are required", http.StatusBadRequest)
			return
		}
		if reg == nil {
			http.Error(w, "mention-only registry not configured", http.StatusServiceUnavailable)
			return
		}
		if err := reg.Set(channel, b); err != nil {
			log.Printf("mention-only store error: %v", err)
			http.Error(w, "store failed", http.StatusInternalServerError)
			return
		}
		log.Printf("mention-only bind: channel=%s session=%q name=%q handle=%q",
			channel, strings.TrimSpace(b.SessionID), strings.TrimSpace(b.SessionName), strings.TrimSpace(strings.TrimPrefix(b.Handle, "@")))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mentionOnlyReceipt{
			Stored:    true,
			ChannelID: channel,
			Binding: mentionOnlyBinding{
				SessionID:   strings.TrimSpace(b.SessionID),
				SessionName: strings.TrimSpace(b.SessionName),
				Handle:      strings.TrimSpace(strings.TrimPrefix(b.Handle, "@")),
			},
		})
	}
}

// handleMentionOnlyDelete serves DELETE /mention-only?channel_id=&session_id=.
func handleMentionOnlyDelete(reg *mentionOnlyRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := strings.TrimSpace(r.URL.Query().Get("channel_id"))
		session := strings.TrimSpace(r.URL.Query().Get("session_id"))
		if channel == "" || session == "" {
			var req mentionOnlyRequest
			if r.ContentLength > 0 {
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
					return
				}
				if channel == "" {
					channel = strings.TrimSpace(req.ChannelID)
				}
				if session == "" {
					session = strings.TrimSpace(req.SessionID)
				}
			}
		}
		if channel == "" || session == "" {
			http.Error(w, "channel_id and session_id are required (?channel_id=&session_id= or JSON body)", http.StatusBadRequest)
			return
		}
		if reg == nil {
			http.Error(w, "mention-only registry not configured", http.StatusServiceUnavailable)
			return
		}
		existed, err := reg.Delete(channel, session)
		if err != nil {
			log.Printf("mention-only delete error: %v", err)
			http.Error(w, "store failed", http.StatusInternalServerError)
			return
		}
		log.Printf("mention-only unbind: channel=%s session=%q existed=%v", channel, session, existed)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mentionOnlyReceipt{Removed: true, Existed: existed, ChannelID: channel})
	}
}

// handleMentionOnlyList serves GET /mention-only[?channel_id=].
func handleMentionOnlyList(reg *mentionOnlyRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := strings.TrimSpace(r.URL.Query().Get("channel_id"))
		all := reg.All()
		if channel != "" {
			filtered := map[string][]mentionOnlyBinding{}
			if list, ok := all[channel]; ok {
				filtered[channel] = list
			}
			all = filtered
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"channels": all})
	}
}

// handleMentionOnlyDeliveries serves GET /mention-only/deliveries?session_id=[&ts=].
func handleMentionOnlyDeliveries(logStore *mentionOnlyDeliveryLog) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := strings.TrimSpace(r.URL.Query().Get("session_id"))
		if session == "" {
			http.Error(w, "session_id is required", http.StatusBadRequest)
			return
		}
		items := logStore.forSession(session)
		if ts := strings.TrimSpace(r.URL.Query().Get("ts")); ts != "" {
			filtered := items[:0:0]
			for _, d := range items {
				if d.TS == ts {
					filtered = append(filtered, d)
				}
			}
			items = filtered
		}
		if items == nil {
			items = []mentionOnlyDelivery{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	}
}
