package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const slackReadbackResponseLimit = 1 << 20

// Readback classifications. These are the adapter's own vocabulary and never
// reach the wire on their own: slackReadbackReceiptFailure maps them onto gc's
// closed PublishFailureKind set and carries the value here as receipt metadata
// instead. See that function for why.
const (
	slackReadbackUnconfirmed = "readback_unconfirmed"
	slackReadbackUnavailable = "readback_unavailable"
	slackReadbackAuth        = "readback_auth"
)

// gc's PublishFailureKind vocabulary, as far as this file needs it
// (gascity internal/extmsg/types.go: unsupported|transient|rate_limited|
// permanent|auth|not_found). Named here so the mapping below reads as a
// mapping rather than as bare strings.
const (
	publishFailureTransient = "transient"
	publishFailurePermanent = "permanent"
	publishFailureAuth      = "auth"
)

// receiptMetadataKeyReadback carries the readback classification that
// PublishFailureKind cannot express. Metadata is the only additive channel gc
// actually forwards (see publishReceipt.Metadata), so it is where the detail
// the closed enum would drop has to live.
const receiptMetadataKeyReadback = "readback"

// slackReadbackTiming bounds what the readback can cost the caller. gc gives
// the adapter a 30s budget per call (gascity internal/extmsg/http_adapter.go),
// and /publish now spends it on a write plus a read: the worst case here is
// slackPostTimeout + attempts*clientTimeout + (attempts-1)*delay =
// 10 + 3*4 + 2*0.2 = 22.4s, which leaves headroom inside that budget instead
// of racing it. TestSlackReadbackTimingDefaults pins these numbers.
type slackReadbackTiming struct {
	// attempts is the total number of readback GETs, not the number of
	// retries after the first.
	attempts int
	// delay separates those attempts.
	delay time.Duration
	// clientTimeout bounds one GET.
	clientTimeout time.Duration
}

// slackReadbackTimings is a var so tests can shorten the window; production
// code never assigns it.
var slackReadbackTimings = slackReadbackTiming{
	attempts:      3,
	delay:         200 * time.Millisecond,
	clientTimeout: 4 * time.Second,
}

var slackReadbackHTTPClient = &http.Client{Timeout: slackReadbackTimings.clientTimeout}

// slackReadbackTarget names the message a readback must find and the evidence
// it must find it by.
type slackReadbackTarget struct {
	// channel and messageTS identify the posted message.
	channel   string
	messageTS string
	// threadTS is set when the message was posted as a thread reply, which
	// conversations.history does not return.
	threadTS string
	// expectedText is what was posted, after the alias rewrite and the
	// marker stamp — not the caller's original request text.
	expectedText string
	// marker is the reference marker stamped on this publish, or "" when the
	// publish is unkeyed or the stamp was skipped. See the compare in
	// matchReadbackMessage for how the two arms differ.
	marker string
}

type slackReadbackMessage struct {
	TS   string `json:"ts"`
	Text string `json:"text"`
}

type slackReadbackResponse struct {
	OK       bool                   `json:"ok"`
	Messages []slackReadbackMessage `json:"messages"`
	Error    string                 `json:"error"`
}

type slackReadbackError struct {
	kind string
	// retryable marks the failures a bounded re-check can plausibly clear:
	// a transport blip, and a message Slack has accepted but not yet
	// indexed. A content mismatch and an auth rejection are answers, not
	// blips, and re-asking cannot change either.
	retryable bool
	err       error
}

func (e *slackReadbackError) Error() string { return e.err.Error() }

func (e *slackReadbackError) Unwrap() error { return e.err }

func readbackFailure(kind string, retryable bool, format string, args ...any) error {
	return &slackReadbackError{kind: kind, retryable: retryable, err: fmt.Errorf(format, args...)}
}

func slackReadbackFailureKind(err error) string {
	if typed, ok := err.(*slackReadbackError); ok {
		return typed.kind
	}
	return slackReadbackUnavailable
}

func slackReadbackIsRetryable(err error) bool {
	typed, ok := err.(*slackReadbackError)
	return ok && typed.retryable
}

// slackReadbackReceiptFailure maps a readback failure onto gc's
// PublishFailureKind vocabulary and returns the adapter-level detail to record
// under receiptMetadataKeyReadback.
//
// gc's PublishFailureKind is a closed, documented set. Because it is a string
// typedef, an adapter-invented value deserializes silently and every consumer
// written against the documented enum falls through its default arm — the
// receipt would carry a kind no retry policy can read. So the wire gets a value
// from gc's vocabulary and the metadata carries which readback outcome produced
// it, which is strictly more than an unknown enum value would have told anyone:
//
//   - unconfirmed → permanent. The read path answered and the message was not
//     there (or was different text). Re-posting the identical payload cannot
//     change that; the truncation path is the clearest case, since an oversized
//     body truncates the same way every time.
//   - unavailable → transient. The read leg failed, so nothing is known about
//     the message; retrying is the right response and is safe because
//     handlePublish caches this receipt for re-verification rather than
//     re-posting (see publishReceiptBlocksRepost).
//   - auth      → auth. A token missing history scope is a configuration
//     fault, and gc's vocabulary already has the word for it.
func slackReadbackReceiptFailure(err error) (kind, detail string) {
	detail = slackReadbackFailureKind(err)
	switch detail {
	case slackReadbackUnconfirmed:
		return publishFailurePermanent, detail
	case slackReadbackAuth:
		return publishFailureAuth, detail
	default:
		return publishFailureTransient, detail
	}
}

// slackEntityUnescape reverses the three entities Slack escapes when it stores
// message text. Replacer scans the input once and does not re-scan what it
// substitutes, so "&amp;lt;" unescapes to "&lt;" rather than collapsing two
// levels into "<".
var slackEntityUnescape = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")

// slackAngleSpan matches a well-formed <...> span, which is the shape Slack
// wraps an auto-linked URL in.
var slackAngleSpan = regexp.MustCompile(`<([^<>]*)>`)

// slackEmojiShortcodeRun matches a run of one or more Slack emoji
// shortcodes standing on their own (`:tada:`, `:+1::skin-tone-4:`,
// `:family_man_woman_girl:`): the colon form Slack stores for an emoji that
// was posted as a Unicode character. A run must not be glued to a letter or
// digit on either side: a time such as 12:30:45 or a ratio 1:2:3 carries
// colon-delimited text that is not an emoji and has to stay in the compare,
// or two different timestamps would fold to the same text (the lane's codex
// round 1). The groups keep the boundary characters.
var slackEmojiShortcodeRun = regexp.MustCompile(`(^|[^A-Za-z0-9])((?::[a-z0-9_+\-]+:)+)($|[^A-Za-z0-9])`)

// slackKeycapSequence matches a keycap emoji (`1️⃣`, `#️⃣`): a digit, `#` or
// `*` followed by an optional variation selector and the combining enclosing
// keycap, which Slack stores as `:one:`, `:hash:` and the like. The base
// character goes with it, since the shortcode carries none of it.
var slackKeycapSequence = regexp.MustCompile(`[0-9#*]\x{FE0F}?\x{20E3}`)

// slackNormalizeText folds the transformations Slack applies between
// chat.postMessage and conversations.history: it entity-escapes `&`, `<` and
// `>` in the stored text, auto-links bare URLs into `<url>` spans, and
// stores a Unicode emoji as its colon shortcode (`Done 🎉` reads back as
// `Done :tada:`; https://docs.slack.dev/messaging/formatting-message-text/#emoji).
//
// It is applied to BOTH sides of the compare, never to one. Normalizing only
// the expected text would require predicting Slack's output exactly — the
// auto-link shape in particular is not something this repo can pin, and the
// Unicode-to-shortcode table is Slack's, not this repo's — whereas folding
// both sides only requires that the fold be the same on each, which is
// checkable here. The fold is deliberately lossy in exactly the way Slack is
// lossy: two texts that differ only by escaping, link markup or emoji
// compare equal, because Slack cannot tell an operator which of the two it
// stored either. Divergence that survives the fold is still reported
// unconfirmed.
func slackNormalizeText(text string) string {
	return slackFoldEmoji(slackAngleSpan.ReplaceAllString(slackEntityUnescape.Replace(text), "$1"))
}

// slackFoldEmoji removes every emoji from text in both of its spellings: the
// colon shortcodes Slack stores, and the Unicode form that was posted —
// keycap sequences, then the emoji characters themselves (slackEmojiRunes:
// the pictographic blocks, the scattered emoji of the symbol blocks such as
// ©, the regional indicators of flags, the skin-tone modifiers), the
// zero-width joiner and variation selectors that build sequences from
// them, and the tag characters of subdivision flags. Without
// this an unkeyed publish of a message with an emoji in it read back as a
// permanent failure although Slack had delivered it (gf-4c9 round 2,
// finding 3c).
func slackFoldEmoji(text string) string {
	text = slackKeycapSequence.ReplaceAllString(text, "")
	// A match consumes the boundary after a run, which is also the boundary
	// before the next one, so adjacent runs ("a :x: :y: b") take a second
	// pass; the loop ends when a pass changes nothing.
	for {
		folded := slackEmojiShortcodeRun.ReplaceAllString(text, "$1$3")
		if folded == text {
			break
		}
		text = folded
	}
	return strings.Map(func(r rune) rune {
		if isSlackEmojiRune(r) {
			return -1
		}
		return r
	}, text)
}

// slackEmojiRunes is the set of emoji characters the fold removes: the
// pictographic blocks and the scattered emoji of the symbol blocks, after
// Unicode's Extended_Pictographic list as Slack renders it, plus the
// regional indicators and the skin-tone modifiers. It is deliberately NOT
// the whole "other symbol" category: ⌘, box drawing, Braille and the like
// are text Slack stores as typed, and folding them away would confirm a
// changed body (the lane's codex round 2).
var slackEmojiRunes = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x00A9, Hi: 0x00A9, Stride: 1}, // ©
		{Lo: 0x00AE, Hi: 0x00AE, Stride: 1}, // ®
		{Lo: 0x203C, Hi: 0x203C, Stride: 1}, // ‼
		{Lo: 0x2049, Hi: 0x2049, Stride: 1}, // ⁉
		{Lo: 0x2122, Hi: 0x2122, Stride: 1}, // ™
		{Lo: 0x2139, Hi: 0x2139, Stride: 1}, // ℹ
		{Lo: 0x2194, Hi: 0x2199, Stride: 1}, // ↔ … ↙
		{Lo: 0x21A9, Hi: 0x21AA, Stride: 1}, // ↩ ↪
		{Lo: 0x231A, Hi: 0x231B, Stride: 1}, // ⌚ ⌛
		{Lo: 0x2328, Hi: 0x2328, Stride: 1}, // ⌨
		{Lo: 0x23CF, Hi: 0x23CF, Stride: 1}, // ⏏
		{Lo: 0x23E9, Hi: 0x23F3, Stride: 1}, // ⏩ … ⏳
		{Lo: 0x23F8, Hi: 0x23FA, Stride: 1}, // ⏸ ⏹ ⏺
		{Lo: 0x24C2, Hi: 0x24C2, Stride: 1}, // Ⓜ
		{Lo: 0x25AA, Hi: 0x25AB, Stride: 1}, // ▪ ▫
		{Lo: 0x25B6, Hi: 0x25B6, Stride: 1}, // ▶
		{Lo: 0x25C0, Hi: 0x25C0, Stride: 1}, // ◀
		{Lo: 0x25FB, Hi: 0x25FE, Stride: 1}, // ◻ … ◾
		{Lo: 0x2600, Hi: 0x27BF, Stride: 1}, // Miscellaneous Symbols, Dingbats
		{Lo: 0x2934, Hi: 0x2935, Stride: 1}, // ⤴ ⤵
		{Lo: 0x2B05, Hi: 0x2B07, Stride: 1}, // ⬅ ⬆ ⬇
		{Lo: 0x2B1B, Hi: 0x2B1C, Stride: 1}, // ⬛ ⬜
		{Lo: 0x2B50, Hi: 0x2B50, Stride: 1}, // ⭐
		{Lo: 0x2B55, Hi: 0x2B55, Stride: 1}, // ⭕
		{Lo: 0x3030, Hi: 0x3030, Stride: 1}, // 〰
		{Lo: 0x303D, Hi: 0x303D, Stride: 1}, // 〽
		{Lo: 0x3297, Hi: 0x3297, Stride: 1}, // ㊗
		{Lo: 0x3299, Hi: 0x3299, Stride: 1}, // ㊙
	},
	R32: []unicode.Range32{
		{Lo: 0x1F000, Hi: 0x1F0FF, Stride: 1}, // Mahjong, Domino, Playing Cards (🀄 🃏)
		{Lo: 0x1F170, Hi: 0x1F251, Stride: 1}, // Enclosed Alphanumeric / Ideographic Supplement (🅰 … 🉑)
		{Lo: 0x1F300, Hi: 0x1F6FF, Stride: 1}, // Pictographs, Emoticons, Transport; regional indicators and skin tones inside
		{Lo: 0x1F780, Hi: 0x1F7FF, Stride: 1}, // Geometric Shapes Extended (🟠 …)
		{Lo: 0x1F900, Hi: 0x1F9FF, Stride: 1}, // Supplemental Symbols and Pictographs
		{Lo: 0x1FA70, Hi: 0x1FAFF, Stride: 1}, // Symbols and Pictographs Extended-A
	},
	LatinOffset: 2,
}

func isSlackEmojiRune(r rune) bool {
	switch r {
	case 0x200D, 0xFE0E, 0xFE0F, 0x20E3:
		return true
	}
	if r >= 0xE0020 && r <= 0xE007F { // tag characters (subdivision flags)
		return true
	}
	return unicode.Is(slackEmojiRunes, r)
}

// readBackPublishedMessage verifies a post through Slack's read API rather
// than trusting chat.postMessage's write response as delivery evidence.
//
// Retryable failures get a bounded re-check: conversations.history offers no
// read-your-writes guarantee, so a message Slack has accepted can be missing
// from the very next read purely because indexing has not caught up. Reporting
// that first empty read as "confirmed absent" would be a false negative on the
// delivery-evidence contract, and the caller's response to it is to post again.
func readBackPublishedMessage(client *http.Client, token string, target slackReadbackTarget) error {
	attempts := slackReadbackTimings.attempts
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for attempt := 1; ; attempt++ {
		err = readBackPublishedMessageOnce(client, token, target)
		if err == nil {
			return nil
		}
		if attempt >= attempts || !slackReadbackIsRetryable(err) {
			return err
		}
		time.Sleep(slackReadbackTimings.delay)
	}
}

func readBackPublishedMessageOnce(client *http.Client, token string, target slackReadbackTarget) error {
	if client == nil {
		client = http.DefaultClient
	}
	request, err := newSlackReadbackRequest(token, target.channel, target.messageTS, target.threadTS)
	if err != nil {
		// A malformed request is this process's own fault and re-issuing it
		// produces the same error, so it is not retryable.
		return readbackFailure(slackReadbackUnavailable, false, "build Slack readback: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return readbackFailure(slackReadbackUnavailable, true, "Slack readback request: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, slackReadbackResponseLimit+1))
	if err != nil {
		return readbackFailure(slackReadbackUnavailable, true, "read Slack readback response: %w", err)
	}
	if len(body) > slackReadbackResponseLimit {
		return readbackFailure(slackReadbackUnavailable, false, "Slack readback response exceeds %d bytes", slackReadbackResponseLimit)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return readbackFailure(slackReadbackUnavailable, true, "Slack readback HTTP %d: %s", response.StatusCode, clipBodyForLog(body))
	}

	var result slackReadbackResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return readbackFailure(slackReadbackUnavailable, false, "decode Slack readback response: %w", err)
	}
	if !result.OK {
		// An auth-class rejection is a configuration fault, not an instrument
		// blip: the same token will be refused on every re-check, and a caller
		// that reads the kind should be told to fix the token rather than to
		// retry. /publish-file already classifies these through mapSlackError;
		// this arm keeps the readback leg consistent with it.
		switch result.Error {
		case "invalid_auth", "not_authed", "token_revoked", "missing_scope":
			return readbackFailure(slackReadbackAuth, false, "Slack readback rejected: %s", result.Error)
		}
		return readbackFailure(slackReadbackUnavailable, true, "Slack readback rejected: %s", result.Error)
	}
	for _, message := range result.Messages {
		if message.TS != target.messageTS {
			continue
		}
		return matchReadbackMessage(message, target)
	}
	return readbackFailure(slackReadbackUnconfirmed, true,
		"Slack message %s not present in channel %s readback", target.messageTS, target.channel)
}

// matchReadbackMessage decides whether the message Slack stored at this ts is
// the message that was posted.
//
// It cannot be a byte compare against what was posted. Slack canonicalizes text
// at ingestion, so a delivered message containing `&`, `<`, `>` or a bare URL
// reads back as different bytes than were sent — and agent traffic through this
// adapter is mostly PR links, dashboards and shell snippets, so that is the
// common shape, not a corner. A byte compare therefore reports Delivered:false
// for messages that are sitting in the channel, and because a non-delivered
// receipt is not cached (publishReceiptBlocksRepost), the caller's retry
// re-posts and fails the same deterministic way.
//
// Two arms, because neither covers the other:
//
//   - Keyed publishes carry the reference marker as the tail of the posted
//     text. Its characters (`_`, `:`, hex) are outside Slack's escape set and
//     it is not link-shaped, so it survives ingestion verbatim; a tail match on
//     it identifies this exact publish by its idempotency key without depending
//     on any model of Slack's canonicalization. Truncation cannot smuggle a
//     message past this arm, because Slack truncates from the end — the marker
//     is the first thing to go — and handlePublish skips the stamp entirely for
//     text that could not fit under the ceiling with it.
//   - Everything else — unkeyed publishes, and keyed ones whose stamp was
//     skipped — is compared on text folded through slackNormalizeText.
func matchReadbackMessage(message slackReadbackMessage, target slackReadbackTarget) error {
	if target.marker != "" {
		if strings.HasSuffix(message.Text, target.marker) {
			return nil
		}
		return readbackFailure(slackReadbackUnconfirmed, false,
			"Slack readback missing reference marker for message %s", target.messageTS)
	}
	if slackNormalizeText(message.Text) != slackNormalizeText(target.expectedText) {
		return readbackFailure(slackReadbackUnconfirmed, false,
			"Slack readback content mismatch for message %s", target.messageTS)
	}
	return nil
}

// confirmPublishReceipt runs the readback for a write Slack accepted and
// records what it proved on the receipt. It is the only place Delivered is set
// for a successful post: chat.postMessage's own answer is not delivery
// evidence, which is the premise this whole path exists to enforce.
func confirmPublishReceipt(token string, receipt *publishReceipt, target slackReadbackTarget) {
	err := readBackPublishedMessage(slackReadbackHTTPClient, token, target)
	if err == nil {
		receipt.Delivered = true
		receipt.FailureKind = ""
		// A re-verified receipt must not keep the detail from the read that
		// failed, or a caller would see a delivered receipt still blaming the
		// readback.
		delete(receipt.Metadata, receiptMetadataKeyReadback)
		return
	}
	log.Printf("slack readback failed: %v", err)
	kind, detail := slackReadbackReceiptFailure(err)
	receipt.Delivered = false
	receipt.FailureKind = kind
	if receipt.Metadata == nil {
		receipt.Metadata = map[string]string{}
	}
	receipt.Metadata[receiptMetadataKeyReadback] = detail
}

// replayPublishReceipt answers a retry on an idempotency key that already
// produced a receipt, without posting to Slack again.
//
// A delivered receipt is returned as it stands. A posted-but-unconfirmed one —
// a write Slack accepted whose read leg was unavailable — is re-verified
// instead: the message very probably exists, so the one thing this must not do
// is post it a second time. Only a readback that positively reports the message
// absent releases the key, and then by returning ok=false so the caller's
// normal post path runs.
func replayPublishReceipt(
	token string,
	dedup *publishDedupCache,
	key string,
	target slackReadbackTarget,
) (publishReceipt, bool) {
	cached, ok := dedup.Get(key)
	if !ok {
		return publishReceipt{}, false
	}
	if cached.Delivered {
		return cached, true
	}
	target.messageTS = cached.MessageID
	confirmPublishReceipt(token, &cached, target)
	if !cached.Delivered && cached.Metadata[receiptMetadataKeyReadback] == slackReadbackUnconfirmed {
		log.Printf("publish: dedup re-verify idem=%s ts=%s -> Slack does not have the message; releasing the key",
			key, cached.MessageID)
		dedup.Delete(key)
		return publishReceipt{}, false
	}
	dedup.Put(key, cached)
	return cached, true
}

func newSlackReadbackRequest(token, channel, messageTS, threadTS string) (*http.Request, error) {
	if token == "" || channel == "" || messageTS == "" {
		return nil, fmt.Errorf("Slack readback requires token, channel, and message ts")
	}
	query := url.Values{
		"channel":   {channel},
		"oldest":    {messageTS},
		"latest":    {messageTS},
		"inclusive": {"true"},
		"limit":     {"1"},
	}
	method := "conversations.history"
	if threadTS != "" {
		method = "conversations.replies"
		query.Set("ts", threadTS)
		// Slack includes the thread parent in conversations.replies even when
		// the time range selects a reply. Leave room for that parent and the
		// exact posted reply; the timestamp match above remains exact.
		query.Set("limit", "2")
	}
	request, err := http.NewRequest(http.MethodGet, slackAPIBase+"/"+method+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build Slack readback request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	return request, nil
}
