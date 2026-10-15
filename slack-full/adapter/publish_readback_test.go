package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

// readbackTarget builds the common top-level-post target so each test names
// only what it is actually varying.
func readbackTarget(expectedText string) slackReadbackTarget {
	return slackReadbackTarget{channel: "C123", messageTS: "1700.001", expectedText: expectedText}
}

// fastReadbackTimings collapses the bounded re-check window for tests that
// exercise a retryable failure. Without it every such test pays the production
// window (3 attempts, 200ms apart) to prove something that does not depend on
// the wall clock. Tests that assert on the retry *behavior* set their own
// values; this only removes the wait.
func fastReadbackTimings(t *testing.T) {
	t.Helper()
	original := slackReadbackTimings
	slackReadbackTimings.delay = 0
	t.Cleanup(func() { slackReadbackTimings = original })
}

// TestSlackReadbackTimingDefaults pins the production window in literals.
//
// The retry loop and the two HTTP clients are only safe because their worst
// case fits inside the 30s budget gc gives an adapter call, and that argument
// is arithmetic over these numbers: slackPostTimeout + attempts*clientTimeout +
// (attempts-1)*delay. A test that recomputed the bound from the same constants
// would agree with any values at all, including ones that blow the budget — so
// the numbers are written out here, and a change to them has to be argued for
// against this comment rather than absorbed silently.
func TestSlackReadbackTimingDefaults(t *testing.T) {
	if slackReadbackTimings.attempts != 3 {
		t.Errorf("readback attempts = %d, want 3", slackReadbackTimings.attempts)
	}
	if slackReadbackTimings.delay != 200*time.Millisecond {
		t.Errorf("readback delay = %v, want 200ms", slackReadbackTimings.delay)
	}
	if slackReadbackTimings.clientTimeout != 4*time.Second {
		t.Errorf("readback client timeout = %v, want 4s", slackReadbackTimings.clientTimeout)
	}
	if slackReadbackHTTPClient.Timeout != 4*time.Second {
		t.Errorf("readback http client timeout = %v, want 4s", slackReadbackHTTPClient.Timeout)
	}
	if slackPostTimeout != 10*time.Second {
		t.Errorf("post timeout = %v, want 10s", slackPostTimeout)
	}
	if slackPostHTTPClient.Timeout != slackPostTimeout {
		t.Errorf("post http client timeout = %v, want %v", slackPostHTTPClient.Timeout, slackPostTimeout)
	}
	worst := slackPostTimeout +
		time.Duration(slackReadbackTimings.attempts)*slackReadbackTimings.clientTimeout +
		time.Duration(slackReadbackTimings.attempts-1)*slackReadbackTimings.delay
	if worst >= 30*time.Second {
		t.Fatalf("worst-case /publish cost = %v, want under gc's 30s adapter budget", worst)
	}
}

// TestSlackReadbackReceiptFailureMapsOntoGCVocabulary pins the wire contract.
//
// gc's PublishFailureKind is a closed set, but it is a string typedef, so an
// adapter-invented kind deserializes without error and every consumer written
// against the documented enum silently takes its default arm. The bug that
// causes is invisible from this side — nothing here would fail — so the mapping
// is pinned against the literal gc vocabulary instead, and the adapter's own
// classification is asserted to survive in metadata rather than on the wire.
func TestSlackReadbackReceiptFailureMapsOntoGCVocabulary(t *testing.T) {
	gcVocabulary := map[string]bool{
		"unsupported": true, "transient": true, "rate_limited": true,
		"permanent": true, "auth": true, "not_found": true,
	}
	tests := []struct {
		readback string
		wantKind string
	}{
		{slackReadbackUnconfirmed, "permanent"},
		{slackReadbackUnavailable, "transient"},
		{slackReadbackAuth, "auth"},
	}
	for _, test := range tests {
		t.Run(test.readback, func(t *testing.T) {
			kind, detail := slackReadbackReceiptFailure(
				readbackFailure(test.readback, false, "readback failed"))
			if kind != test.wantKind {
				t.Errorf("failure kind = %q, want %q", kind, test.wantKind)
			}
			if !gcVocabulary[kind] {
				t.Errorf("failure kind %q is outside gc's PublishFailureKind set %v", kind, gcVocabulary)
			}
			if detail != test.readback {
				t.Errorf("metadata detail = %q, want %q: the closed enum cannot carry it, so metadata must",
					detail, test.readback)
			}
		})
	}
	// An untyped error is the "we do not know" case and must not claim more
	// than transient.
	if kind, detail := slackReadbackReceiptFailure(errors.New("unknown")); kind != "transient" || detail != slackReadbackUnavailable {
		t.Errorf("untyped error mapped to (%q, %q), want (transient, %q)", kind, detail, slackReadbackUnavailable)
	}
}

func TestReadBackPublishedMessageFromChannelHistory(t *testing.T) {
	newSlackReadbackServer(t, "/conversations.history", func(query url.Values) {
		assertSlackReadbackQuery(t, query, "C123", "1700.001", "1")
	}, `{"ok":true,"messages":[{"ts":"1700.001","text":"exact body"}]}`)

	if err := readBackPublishedMessage(nil, "xoxb-test", readbackTarget("exact body")); err != nil {
		t.Fatalf("readBackPublishedMessage: %v", err)
	}
}

func TestReadBackPublishedThreadReplyUsesConversationReplies(t *testing.T) {
	server := newSlackReadbackServer(t, "/conversations.replies", func(query url.Values) {
		assertSlackReadbackQuery(t, query, "C123", "1700.002", "2")
		if got := query.Get("ts"); got != "1699.001" {
			t.Errorf("thread ts = %q, want 1699.001", got)
		}
	}, `{"ok":true,"messages":[{"ts":"1699.001","text":"parent"},{"ts":"1700.002","thread_ts":"1699.001","text":"reply"}]}`)

	target := slackReadbackTarget{
		channel: "C123", messageTS: "1700.002", threadTS: "1699.001", expectedText: "reply",
	}
	if err := readBackPublishedMessage(server.Client(), "xoxb-test", target); err != nil {
		t.Fatalf("readBackPublishedMessage: %v", err)
	}
}

// TestReadBackPublishedMessageAcceptsSlackCanonicalizedText is F1's regression
// control, and the case the original readback got wrong.
//
// Slack does not store what you post: it entity-escapes `&`, `<` and `>` and
// auto-links bare URLs into `<url>` spans. Agent traffic through this adapter is
// mostly PR links, dashboards and shell snippets, so that is the common shape,
// not a corner — and a byte compare against the posted text reports every one of
// those delivered messages as unconfirmed. The negative arm is what stops the
// fix from degrading into "accept anything": a genuinely different body still
// fails.
func TestReadBackPublishedMessageAcceptsSlackCanonicalizedText(t *testing.T) {
	posted := "R&D update: see https://github.com/o/r/pull/313 <not a tag>"
	// entityLiteral is text that already contains an entity-shaped substring
	// when it is posted. It is the one payload class on which the shipped
	// both-sides fold and its mirror image (folding an entity-escaped expected
	// side instead) disagree, so without this row the suite is green under
	// either, and the fold's semantics here are an accident rather than a
	// decision. See the row's comment for what the outcome means.
	entityLiteral := "docs say &amp; is the escape"
	tests := []struct {
		name string
		// postedText overrides the shared posted text for rows whose subject
		// is the payload rather than the stored form; "" means use posted.
		postedText string
		stored     string
		wantErr    bool
	}{
		{name: "slack canonicalization", stored: slackCanonicalizeForTest(posted)},
		{name: "verbatim echo", stored: posted},
		{name: "different body", stored: "R&D update: see something else entirely", wantErr: true},
		{
			name:    "truncated body",
			stored:  slackCanonicalizeForTest("R&D update: see https://github.com/o/r/pull/313"),
			wantErr: true,
		},
		{
			// Entity-literal text under the fake's escape model reads back
			// UNCONFIRMED, and that is the shipped behaviour, pinned here so it
			// cannot flip silently in either direction.
			//
			// The fake escapes unconditionally (canonicalizePlainTextForTest),
			// so it stores "&amp;amp;" for the "&amp;" posted here; the fold's
			// single-pass unescape then takes the stored side to "&amp;" and
			// the posted side to "&", which differ. Slack's documented contract
			// is the weaker one — senders are told to escape `&`, and only the
			// listed characters are decoded for display, which is self-defeating
			// if the platform re-escaped a valid entity — so under real Slack
			// this same payload is expected to confirm. The uncertainty is about
			// the fake, not about the matcher: do NOT "fix" matchReadbackMessage
			// to make this row confirm. Escaping the expected side instead is an
			// exact inversion, not an improvement — it would confirm this class
			// and start rejecting escaped-URL query strings (?a=1&amp;b=2),
			// which is the more common shape and the one Slack's docs describe.
			name:       "entity-literal payload under the fake's stricter escape",
			postedText: entityLiteral,
			stored:     slackCanonicalizeForTest(entityLiteral),
			wantErr:    true,
		},
	}
	if slackCanonicalizeForTest(posted) == posted {
		t.Fatalf("the fake Slack stored %q unchanged; it models the identity function, which is the blind spot this test exists to close",
			posted)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			postedText := test.postedText
			if postedText == "" {
				postedText = posted
			}
			newSlackReadbackServer(t, "/conversations.history", func(url.Values) {}, mustJSON(t, map[string]any{
				"ok":       true,
				"messages": []map[string]string{{"ts": "1700.001", "text": test.stored}},
			}))
			err := readBackPublishedMessage(nil, "xoxb-test", readbackTarget(postedText))
			if (err != nil) != test.wantErr {
				t.Fatalf("readBackPublishedMessage = %v, wantErr %v (posted %q, stored %q)", err, test.wantErr, postedText, test.stored)
			}
		})
	}
}

// Keyed publishes follow the same timestamp + canonicalized text rule.
func TestReadBackPublishedMessageMatchesKeyedPublishByTimestampAndText(t *testing.T) {
	fastReadbackTimings(t)
	const posted = "R&D <b> shipping the fix"
	tests := []struct {
		name    string
		ts      string
		stored  string
		wantErr bool
	}{
		{name: "canonicalized text", ts: "1700.001", stored: slackCanonicalizeForTest(posted)},
		{name: "different text at same ts", ts: "1700.001", stored: "shipping the fix", wantErr: true},
		{name: "same text at different ts", ts: "1700.002", stored: posted, wantErr: true},
		{name: "truncated text", ts: "1700.001", stored: "R&D <b> shipping", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newSlackReadbackServer(t, "/conversations.history", func(url.Values) {}, mustJSON(t, map[string]any{
				"ok":       true,
				"messages": []map[string]string{{"ts": test.ts, "text": test.stored}},
			}))
			err := readBackPublishedMessage(nil, "xoxb-test", readbackTarget(posted))
			if (err != nil) != test.wantErr {
				t.Fatalf("readBackPublishedMessage = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

// These fixtures use Slack's retrieved link format, not the fake's simpler
// angle-wrap model. A label may repeat the original URL; a different target or
// label still must fail. See https://docs.slack.dev/messaging/formatting-message-text/#links-in-retrieved-messages.
func TestHandlePublishKeyedAutoLinksReadBackWithoutDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name, posted, stored string
		wantDelivered        bool
	}{
		{"labeled URL", "see https://docs.slack.dev/", "see <https://docs.slack.dev/|https://docs.slack.dev/>", true},
		{"bare domain auto-link", "see hou.vc", "see <http://hou.vc|hou.vc>", true},
		{"bare domain HTTPS auto-link", "see wendy.sh/docs", "see <https://wendy.sh/docs|wendy.sh/docs>", true},
		{"bare domain different destination", "see hou.vc", "see <http://attacker.com|hou.vc>", false},
		{"bare domain path stays literal", "see hou.vc/:id:/x 🎉", "see <http://hou.vc/:id:/x|hou.vc/:id:/x> :tada:", true},
		{"bare domain changed path", "see hou.vc/:id:/x", "see <http://hou.vc/:100:/x|hou.vc/:100:/x>", false},
		{"bare IPv4 changed path", "see 127.0.0.1/:id:/x", "see <http://127.0.0.1/:ok:/x|127.0.0.1/:ok:/x>", false},
		{"bare IPv6 changed path", "see [::1]/:id:/x", "see <http://[::1]/:ok:/x|[::1]/:ok:/x>", false},
		{"bare domain changed query", "see hou.vc?q=:id:", "see <https://hou.vc?q=:ok:|hou.vc?q=:ok:>", false},
		{"bare domain changed fragment", "see hou.vc#:id:", "see <http://hou.vc#:ok:|hou.vc#:ok:>", false},
		{"bare domain changed path beside emoji", "state:ready🎉 see hou.vc/:id:/x", "state:ready:tada: see <http://hou.vc/:ok:/x|hou.vc/:ok:/x>", false},
		{"bare domain unchanged path beside emoji", "state:ready🎉 see hou.vc/:id:/x", "state:ready:tada: see <http://hou.vc/:id:/x|hou.vc/:id:/x>", true},
		{"numeric prose with emoji", "Score 4.5/5🎉", "Score 4.5/5:tada:", true},
		{"numeric prose beside fallback emoji", "state:ready🎉 score 0.5/2✅", "state:ready:tada: score 0.5/2:white_check_mark:", true},
		{"explicit scheme unchanged", "see <https://example.com|example.com>", "see <https://example.com|example.com>", true},
		{"explicit scheme changed", "see <https://example.com|example.com>", "see <http://example.com|example.com>", false},
		{"implicit and explicit schemes", "example.com <https://example.com|example.com>", "<http://example.com|example.com> <https://example.com|example.com>", true},
		{"repeated label cannot hide changed scheme", "example.com <https://example.com|example.com>", "<https://example.com|example.com> <http://example.com|example.com>", false},
		{"explicit link after fallback emoji", "state:ready🎉 <https://example.com|example.com>", "state:ready:tada: <https://example.com|example.com>", true},
		{"explicit link ampersand escaping", "<https://example.com/?x=1&y=2|example.com/?x=1&y=2>", "<https://example.com/?x=1&amp;y=2|example.com/?x=1&amp;y=2>", true},
		{"parenthesized URL beside emoji", "See (www.example.com/docs)🎉", "See (<http://www.example.com/docs|www.example.com/docs>):tada:", true},
		{"numeric version prose", "version v1.2.3.4/5🎉", "version v1.2.3.4/5:tada:", true},
		{"bare IPv6 authority changed", "see [2001:ab::1]", "see <http://[2001:cd::1]|[2001:cd::1]>", false},
		{"mapped IPv6 path changed", "see [::ffff:192.0.2.1]/:id:/x", "see <http://[::ffff:192.0.2.1]/:ok:/x|[::ffff:192.0.2.1]/:ok:/x>", false},
		{"FQDN path changed with fallback", "state:ready🎉 see hou.vc./:id:/x", "state:ready:tada: see <http://hou.vc./:ok:/x|hou.vc./:ok:/x>", false},
		{"scheme-bearing label changed destination", "see https://example.com", "see <http://https://example.com|https://example.com>", false},
		{"mailto cannot become HTTP userinfo", "<mailto:jane@example.com|jane@example.com>", "<https://jane@example.com|jane@example.com>", false},
		{"bare email cannot become HTTP userinfo", "contact jane@example.com", "contact <https://jane@example.com|jane@example.com>", false},
		{"explicit HTTP userinfo remains explicit", "see <https://jane@example.com|jane@example.com>", "see <https://jane@example.com|jane@example.com>", true},
		{"explicit HTTP userinfo beside emoji", "Done🎉 <https://jane@example.com|jane@example.com>", "Done:tada: <https://jane@example.com|jane@example.com>", true},
		{"HTTP host and port", "see www.example.com:8080/docs", "see <http://www.example.com:8080/docs|www.example.com:8080/docs>", true},
		{"HTTPS host and port", "see example.com:8443/docs", "see <https://example.com:8443/docs|example.com:8443/docs>", true},
		{"legacy www host and port", "see www.example.com:8080/docs", "see <http://example.com:8080/docs|www.example.com:8080/docs>", true},
		{"many repeated links", strings.Repeat("www.a.co ", 4000), strings.Repeat("<http://www.a.co|www.a.co> ", 4000), true},
		{"many automatic links then explicit link", strings.Repeat("www.a.co ", 4000) + "<https://last.example|last.example>", strings.Repeat("<http://www.a.co|www.a.co> ", 4000) + "<https://last.example|last.example>", true},
		{"escaped angle prose", "Use <std::vector>", "Use &lt;std::vector&gt;", true},
		{"escaped explicit scheme changed", "<https://example.com|example.com>", "&lt;http://example.com|example.com&gt;", false},
		{"partial explicit URL changed", "https://example.com", "https://<http://example.com|example.com>", false},
		{"partial domain changed", "docs.example.com", "docs.<http://example.com|example.com>", false},
		{"URL following punctuation", "See:(hou.vc)🎉", "See:(<http://hou.vc|hou.vc>):tada:", true},
		{"truncated path shortcode", "see hou.vc/:id:", "see <http://hou.vc/|hou.vc/>", false},
		{"truncated path Unicode", "see hou.vc/🎉", "see <http://hou.vc/|hou.vc/>", false},
		{"bare host beside emoji", "see hou.vc🎉", "see <http://hou.vc|hou.vc>:tada:", true},
		{"quoted URL path", "see \"hou.vc/docs\"", "see \"<http://hou.vc/docs|hou.vc/docs>\"", true},
		{"comma separated hosts", "hou.vc,wendy.sh", "<http://hou.vc|hou.vc>,<https://wendy.sh|wendy.sh>", true},
		{"comma separated paths", "www.a.co/x,www.b.co/y", "<http://www.a.co/x|www.a.co/x>,<http://www.b.co/y|www.b.co/y>", true},
		{"explicit link then comma and auto-link", "<https://a.co>,www.b.co", "<https://a.co>,<http://www.b.co|www.b.co>", true},
		{"explicit link adjacent to auto-link", "<https://a.co>www.b.co", "<https://a.co><http://www.b.co|www.b.co>", true},
		{"mention adjacent to auto-link", "<@U123>www.b.co", "<@U123|bob><http://www.b.co|www.b.co>", true},
		{"bare URI then punctuation and auto-link", "See (https://a.co),(www.b.co)", "See (<https://a.co>),(<http://www.b.co|www.b.co>)", true},
		{"changed URI after matched link", "https://a.co,https://b.co", "<https://a.co>,https://<http://b.co|b.co>", false},
		{"broadcast here adjacent to auto-link", "<!here>www.a.co", "<!here><http://www.a.co|www.a.co>", true},
		{"broadcast channel adjacent to auto-link", "<!channel>www.a.co", "<!channel><http://www.a.co|www.a.co>", true},
		{"broadcast everyone adjacent to auto-link", "<!everyone>www.a.co", "<!everyone><http://www.a.co|www.a.co>", true},
		{"changed broadcast before auto-link", "<!here>www.a.co", "<!channel><http://www.a.co|www.a.co>", false},
		{"angle prose adjacent to auto-link", "Use <std::vector>www.a.co", "Use &lt;std::vector&gt;<http://www.a.co|www.a.co>", true},
		{"escaped angle prose adjacent to auto-link", "Use &lt;tag&gt;www.a.co🎉", "Use &lt;tag&gt;<http://www.a.co|www.a.co>:tada:", true},
		{"repeated IDs before explicit mention", "🎉 " + strings.Repeat("@U123 ", 200) + "<@U123>", ":tada: " + strings.Repeat("@U123 ", 200) + "<@U123|bob>", true},
		{"same target auto-links before explicit link", strings.Repeat("🎉 www.a.co ", 1000) + "<http://www.a.co|www.a.co>", strings.Repeat(":tada: <http://www.a.co|www.a.co> ", 1000) + "<http://www.a.co|www.a.co>", true},
		{"redundant label emoji conversion", "<https://example.com/🎉|example.com/🎉>", "<https://example.com/🎉|example.com/:tada:>", true},
		{"redundant label changed destination", "<https://example.com/🎉|example.com/🎉>", "<https://example.com/other|example.com/:tada:>", false},
		{"redundant label changed literal text", "<https://example.com/🎉|example.com/🎉>", "<https://example.com/🎉|example.com/other:tada:>", false},
		{"explicit link changed trailing text", "<https://example.com|Done🎉> good", "<https://example.com|Done:tada:> bad", false},
		{"balanced path suffix unchanged", "hou.vc/wiki/Foo_(bar)/:id:", "<http://hou.vc/wiki/Foo_(bar|hou.vc/wiki/Foo_(bar>)/:id:", true},
		{"balanced path suffix changed", "hou.vc/wiki/Foo_(bar)/:id:", "<http://hou.vc/wiki/Foo_(bar|hou.vc/wiki/Foo_(bar>)/:ok:", false},
		{"balanced path suffix dropped", "hou.vc/wiki/Foo_(bar)/:id:", "<http://hou.vc/wiki/Foo_(bar|hou.vc/wiki/Foo_(bar>)/", false},
		{"nested balanced path suffix changed", "hou.vc/wiki/Foo_([bar])/:id:", "<http://hou.vc/wiki/Foo_([bar|hou.vc/wiki/Foo_([bar>])/:ok:", false},
		{"unlabeled URI dropped shortcode suffix", "https://example.com/:id:", "<https://example.com/>", false},
		{"full labeled URI dropped shortcode suffix", "https://example.com/:id:", "<https://example.com/|https://example.com/>", false},
		{"other scheme dropped shortcode suffix", "s3://bucket/:id:/x", "<s3://bucket/>/x", false},
		{"unlabeled URI dropped Unicode suffix", "https://example.com/🎉", "<https://example.com/>", false},
		{"unlabeled URI retained literal Unicode suffix", "https://example.com/🎉", "<https://example.com/>🎉", true},
		{"unlabeled URI changed literal Unicode suffix", "https://example.com/🎉", "<https://example.com/>:tada:", false},
		{"implicit suffix cache preserves URI strictness", "www.a.co,https://b.co/🎉", "<http://www.a.co|www.a.co>,<https://b.co/>:tada:", false},
		{"implicit suffix cache accepts literal URI suffix", "www.a.co,https://b.co/🎉", "<http://www.a.co|www.a.co>,<https://b.co/>🎉", true},
		{"auto-link before explicit label emoji", "www.a.co,<https://b.co/🎉|b.co/🎉>", "<http://www.a.co|www.a.co>,<https://b.co/🎉|b.co/:tada:>", true},
		{"auto-link before changed explicit target", "www.a.co,<https://b.co/🎉|b.co/🎉>", "<http://www.a.co|www.a.co>,<https://b.co/other|b.co/:tada:>", false},
		{"auto-link before angle prose emoji", "www.a.co,<status: done 🎉>", "<http://www.a.co|www.a.co>,&lt;status: done :tada:&gt;", true},
		{"explicit link before changed partial URI", "<https://a.co>,https://b.co", "<https://a.co>,https://<http://b.co|b.co>", false},
		{"changed shortcode after query delimiter", "hou.vc?:id:", "<http://hou.vc?|hou.vc?>:ok:", false},
		{"changed shortcode after fragment delimiter", "hou.vc#:id:", "<http://hou.vc#|hou.vc#>:ok:", false},
		{"retained shortcode after query delimiter", "hou.vc?:id:", "<http://hou.vc?|hou.vc?>:id:", true},
		{"retained shortcode after fragment delimiter", "hou.vc#:id:", "<http://hou.vc#|hou.vc#>:id:", true},
		{"path followed by retained emoji", "www.a.co/x🎉", "<http://www.a.co/x|www.a.co/x>:tada:", true},
		{"hostname followed by flag", "www.a.co🇺🇸", "<http://www.a.co|www.a.co>:us:", true},
		{"path followed by flag", "www.a.co/x🇺🇸", "<http://www.a.co/x|www.a.co/x>:us:", true},
		{"hostname followed by family", "www.a.co👨‍👩‍👧", "<http://www.a.co|www.a.co>:man-woman-girl:", true},
		{"path followed by family", "www.a.co/x👨‍👩‍👧", "<http://www.a.co/x|www.a.co/x>:man-woman-girl:", true},
		{"hostname followed by skin tone and text", "www.a.co👍🏽now", "<http://www.a.co|www.a.co>:+1::skin-tone-4:now", true},
		{"path followed by skin tone and text", "www.a.co/x👍🏽now", "<http://www.a.co/x|www.a.co/x>:+1::skin-tone-4:now", true},
		{"missing literal tone after path emoji", "www.a.co/x🎉:skin-tone-4:", "<http://www.a.co/x|www.a.co/x>:tada:", false},
		{"retained literal tone after path emoji", "www.a.co/x🎉:skin-tone-4:", "<http://www.a.co/x|www.a.co/x>:tada::skin-tone-4:", true},
		{"retained literal tone after toned path emoji", "www.a.co/x👍🏽:skin-tone-4:", "<http://www.a.co/x|www.a.co/x>:+1::skin-tone-4::skin-tone-4:", true},
		{"hostname emoji with typed tone", "www.a.co👍:skin-tone-4:", "<http://www.a.co|www.a.co>:+1::skin-tone-4:", true},
		{"missing flag beside path", "www.a.co/x🇺🇸", "<http://www.a.co/x|www.a.co/x>", false},
		{"escaped prose with colon and emoji", "Use <status: done 🎉>", "Use &lt;status: done :tada:&gt;", true},
		{"escaped compact prose with emoji", "Use &lt;Status:ready🎉&gt;", "Use &lt;Status:ready:tada:&gt;", true},
		{"code occurrences before actual link", "🎉\n```\n" + strings.Repeat("www.a.co\n", 200) + "```\nwww.a.co", ":tada:\n```\n" + strings.Repeat("www.a.co\n", 200) + "```\n<http://www.a.co|www.a.co>", true},
		{"HTTP userinfo cannot become mailto", "<https://jane@example.com|jane@example.com>", "<mailto:jane@example.com|jane@example.com>", false},
		{"explicit link cannot disappear", "<https://example.com|example.com>", "example.com", false},
		{"explicit display label emoji", "<https://example.com|Done🎉>", "<https://example.com|Done:tada:>", true},
		{"many failed prefix candidates", strings.Repeat("a.co ", 8000), "x <http://a.co|a.co>", false},
		{"www auto-link", "see www.example.com", "see <http://www.example.com|www.example.com>", true},
		{"legacy documented www auto-link", "see www.example.com", "see <http://example.com|www.example.com>", true},
		{"different destination", "see www.example.com", "see <http://www.attacker.com|www.example.com>", false},
		{"different label", "see www.example.com", "see <http://www.example.com|changed>", false},
		{"channel label", "see <#C123>", "see <#C123|general>", true},
		{"changed channel ID", "see <#C123|general>", "see <#C456|general>", false},
		{"user label", "hello <@U123>", "hello <@U123|bob>", true},
		{"changed user ID", "hello <@U123|bob>", "hello <@U456|bob>", false},
		{"group label", "hello <!subteam^S123>", "hello <!subteam^S123|@team>", true},
		{"changed group ID", "hello <!subteam^S123|@team>", "hello <!subteam^S456|@team>", false},
		{"email label", "contact jane@example.com", "contact <mailto:jane@example.com|jane@example.com>", true},
		{"email without label", "contact jane@example.com", "contact <mailto:jane@example.com>", true},
		{"changed email destination", "contact jane@example.com", "contact <mailto:other@example.com|jane@example.com>", false},
		{"changed email label", "contact jane@example.com", "contact <mailto:jane@example.com|other@example.com>", false},
		{"emoji after clock", "Done at 10:30🎉", "Done at 10:30:tada:", true},
		{"emoji after colon word", "state:ready🎉", "state:ready:tada:", true},
		{"emoji at both ends of colon words", "✅status:ready🎉", ":white_check_mark:status:ready:tada:", true},
		{"emoji-named word before emoji", "key:value:ok✅", "key:value:ok:white_check_mark:", true},
		{"emoji before colon suffix", "✅ok:", ":white_check_mark:ok:", true},
		{"changed clock before emoji", "Done at 10:30🎉", "Done at 10:45:tada:", false},
		{"changed word before emoji", "state:ready🎉", "state:changed:tada:", false},
		{"emoji in code is literal", "`10:30🎉`", "`10:30:tada:`", false},
		{"emoji in URL is literal", "https://example.com/🎉", "<https://example.com/:tada:>", false},
		{"literal inline code", "`www.example.com`", "`<http://www.example.com|www.example.com>`", false},
		{"literal fenced code", "```www.example.com```", "```<http://www.example.com|www.example.com>```", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := slackAPIBase
			t.Cleanup(func() { slackAPIBase = original })
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/chat.postMessage" {
					posts++
					var post slackPostMessageReq
					if err := json.NewDecoder(r.Body).Decode(&post); err != nil {
						t.Error(err)
					}
					if post.Text != tc.posted {
						t.Errorf("posted text changed: %q", post.Text)
					}
					_, _ = w.Write([]byte(`{"ok":true,"ts":"1700.001"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]string{{"ts": "1700.001", "text": tc.stored}}})
			}))
			defer server.Close()
			slackAPIBase = server.URL
			body := mustJSON(t, publishRequest{SessionID: "s1", Conversation: conversationRef{ConversationID: "C1"}, Text: tc.posted, IdempotencyKey: "link-key"})
			handler := handlePublish(config{slackBotToken: "xoxb-test"}, nil, nil, newPublishDedupCache(publishDedupTTL))
			attempts := 1
			if tc.wantDelivered {
				attempts = 2
			}
			for range attempts {
				w := httptest.NewRecorder()
				handler(w, httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(body)))
				var receipt publishReceipt
				if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
					t.Fatal(err)
				}
				if receipt.Delivered != tc.wantDelivered {
					t.Fatalf("receipt = %+v, want delivered=%v", receipt, tc.wantDelivered)
				}
			}
			if posts != 1 {
				t.Fatalf("posts = %d, want one even on a retry", posts)
			}
		})
	}
}

// TestReadBackPublishedMessageRetriesWhileSlackIndexes pins F5's window.
//
// conversations.history offers no read-your-writes guarantee, so a message Slack
// has accepted can be missing from the very next read purely because indexing
// has not caught up. Calling that first empty read "confirmed absent" is a false
// negative whose consequence is a second copy of the message in the channel.
func TestReadBackPublishedMessageRetriesWhileSlackIndexes(t *testing.T) {
	fastReadbackTimings(t)
	var reads atomic.Int32
	newDynamicSlackReadbackServer(t, func(w http.ResponseWriter) {
		if reads.Add(1) < 3 {
			_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"messages":[{"ts":"1700.001","text":"exact body"}]}`))
	})

	if err := readBackPublishedMessage(nil, "xoxb-test", readbackTarget("exact body")); err != nil {
		t.Fatalf("readBackPublishedMessage: %v, want the later read to confirm it", err)
	}
	if got := reads.Load(); got != 3 {
		t.Fatalf("readback GETs = %d, want 3 (the message appeared on the third)", got)
	}
}

// TestReadBackPublishedMessageStopsRetryingWhatRetryingCannotFix is the control
// for the test above: the bounded window must not be spent re-asking a question
// that already has an answer. A content mismatch and an auth rejection are
// answers.
func TestReadBackPublishedMessageStopsRetryingWhatRetryingCannotFix(t *testing.T) {
	fastReadbackTimings(t)
	tests := []struct {
		name     string
		response string
	}{
		{name: "content mismatch", response: `{"ok":true,"messages":[{"ts":"1700.001","text":"mutated"}]}`},
		{name: "auth rejection", response: `{"ok":false,"error":"missing_scope"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var reads atomic.Int32
			newDynamicSlackReadbackServer(t, func(w http.ResponseWriter) {
				reads.Add(1)
				_, _ = w.Write([]byte(test.response))
			})

			if err := readBackPublishedMessage(nil, "xoxb-test", readbackTarget("exact body")); err == nil {
				t.Fatal("readBackPublishedMessage succeeded, want failure")
			}
			if got := reads.Load(); got != 1 {
				t.Fatalf("readback GETs = %d, want 1: re-asking cannot change this answer", got)
			}
		})
	}
}

func TestReadBackPublishedMessageFailsClosed(t *testing.T) {
	fastReadbackTimings(t)
	tests := []struct {
		name     string
		status   int
		response string
		want     string
		wantKind string
	}{
		{
			name: "message absent", status: http.StatusOK, response: `{"ok":true,"messages":[]}`,
			want: "not present", wantKind: slackReadbackUnconfirmed,
		},
		{
			name:   "content mismatch",
			status: http.StatusOK, response: `{"ok":true,"messages":[{"ts":"1700.001","text":"mutated"}]}`,
			want: "content mismatch", wantKind: slackReadbackUnconfirmed,
		},
		{
			name: "Slack auth rejection", status: http.StatusOK, response: `{"ok":false,"error":"missing_scope"}`,
			want: "missing_scope", wantKind: slackReadbackAuth,
		},
		{
			name: "Slack non-auth rejection", status: http.StatusOK, response: `{"ok":false,"error":"ratelimited"}`,
			want: "ratelimited", wantKind: slackReadbackUnavailable,
		},
		{
			name: "transport status", status: http.StatusTooManyRequests, response: `rate limited`,
			want: "HTTP 429", wantKind: slackReadbackUnavailable,
		},
		{
			name: "malformed response", status: http.StatusOK, response: `{`,
			want: "decode", wantKind: slackReadbackUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.response))
			}))
			t.Cleanup(server.Close)

			originalBase := slackAPIBase
			slackAPIBase = server.URL
			t.Cleanup(func() { slackAPIBase = originalBase })

			err := readBackPublishedMessage(server.Client(), "xoxb-test", readbackTarget("exact body"))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
			if got := slackReadbackFailureKind(err); got != test.wantKind {
				t.Fatalf("failure kind = %q, want %q", got, test.wantKind)
			}
		})
	}
}

func TestReadBackPublishedMessageBoundsAndClassifiesInstrumentFailure(t *testing.T) {
	fastReadbackTimings(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", slackReadbackResponseLimit+1)))
	}))
	t.Cleanup(server.Close)
	originalBase := slackAPIBase
	slackAPIBase = server.URL
	t.Cleanup(func() { slackAPIBase = originalBase })

	err := readBackPublishedMessage(server.Client(), "xoxb-test", readbackTarget("exact body"))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize error = %v, want bounded-response failure", err)
	}
	if got := slackReadbackFailureKind(err); got != slackReadbackUnavailable {
		t.Fatalf("failure kind = %q, want %q", got, slackReadbackUnavailable)
	}

	transportErr := errors.New("transport unavailable")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, transportErr
	})}
	err = readBackPublishedMessage(client, "xoxb-test", readbackTarget("exact body"))
	if !errors.Is(err, transportErr) {
		t.Fatalf("transport error = %v, want wrapped sentinel", err)
	}
	if got := slackReadbackFailureKind(errors.New("unknown")); got != slackReadbackUnavailable {
		t.Fatalf("fallback failure kind = %q, want %q", got, slackReadbackUnavailable)
	}
}

func TestNewSlackReadbackRequestRejectsMissingIdentity(t *testing.T) {
	for _, args := range [][4]string{
		{"", "C123", "1700.001", ""},
		{"xoxb-test", "", "1700.001", ""},
		{"xoxb-test", "C123", "", ""},
	} {
		if _, err := newSlackReadbackRequest(args[0], args[1], args[2], args[3]); err == nil {
			t.Fatalf("newSlackReadbackRequest%q succeeded, want error", args)
		}
	}
}

func TestHandlePublishRequiresExactSlackReadback(t *testing.T) {
	fastReadbackTimings(t)
	tests := []struct {
		name          string
		history       string
		wantDelivered bool
		wantFailure   string
		wantReadback  string
	}{
		{
			name:          "exact message is delivered",
			history:       `{"ok":true,"messages":[{"ts":"1700.001","text":"hello"}]}`,
			wantDelivered: true,
		},
		{
			// The read path answered and the message was not there. Re-posting
			// the identical payload cannot change that, so the kind a caller's
			// retry policy reads is permanent — with the adapter's own
			// classification alongside it, since gc's enum cannot say it.
			name:         "missing message fails loudly",
			history:      `{"ok":true,"messages":[]}`,
			wantFailure:  "permanent",
			wantReadback: slackReadbackUnconfirmed,
		},
		{
			// A token that cannot read history is a provisioning fault, and
			// gc's vocabulary already has the word for it. Distinguishing this
			// from the case above is the point: one says "fix the token", the
			// other says "the message is not there".
			name:         "broken read path is distinct from absent acknowledgement",
			history:      `{"ok":false,"error":"missing_scope"}`,
			wantFailure:  "auth",
			wantReadback: slackReadbackAuth,
		},
		{
			name:         "unavailable read path is retryable",
			history:      `{"ok":false,"error":"internal_error"}`,
			wantFailure:  "transient",
			wantReadback: slackReadbackUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/chat.postMessage":
					_, _ = w.Write([]byte(`{"ok":true,"channel":"C123","ts":"1700.001"}`))
				case "/conversations.history":
					_, _ = w.Write([]byte(test.history))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)

			originalBase := slackAPIBase
			slackAPIBase = server.URL
			t.Cleanup(func() { slackAPIBase = originalBase })

			req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(
				`{"session_id":"gc-1","conversation":{"conversation_id":"C123","kind":"room"},"text":"hello"}`,
			))
			recorder := httptest.NewRecorder()
			handlePublish(config{slackBotToken: "xoxb-test"}, nil, nil, newPublishDedupCache(publishDedupTTL))(recorder, req)

			var receipt publishReceipt
			if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
				t.Fatalf("decode receipt %q: %v", recorder.Body.String(), err)
			}
			if receipt.Delivered != test.wantDelivered || receipt.FailureKind != test.wantFailure {
				t.Fatalf("receipt = %+v, want delivered=%v failure=%q", receipt, test.wantDelivered, test.wantFailure)
			}
			if got := receipt.Metadata[receiptMetadataKeyReadback]; got != test.wantReadback {
				t.Fatalf("receipt metadata[%q] = %q, want %q", receiptMetadataKeyReadback, got, test.wantReadback)
			}
		})
	}
}

// TestHandlePublishReplaysPostedButUnconfirmedReceipt is F2's control on the
// side where the message is probably in Slack.
//
// A write Slack accepted whose read leg then failed leaves the adapter knowing
// only that Slack took the message. Refusing to remember that receipt hands the
// caller's retry to the post path, which duplicates a message that is very
// probably in the channel — the exact duplicate the dedup chokepoint exists to
// absorb. So the receipt is remembered and the retry re-verifies by readback
// instead of re-posting; here the second read succeeds, and the caller gets a
// delivered receipt for the message it already sent.
func TestHandlePublishReplaysPostedButUnconfirmedReceipt(t *testing.T) {
	fastReadbackTimings(t)
	var posts, reads atomic.Int32
	var captured slackPostMessageReq
	fake := newFakeSlackPublishHandler(t, &captured, "1700.001", func() { posts.Add(1) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The first publish's read leg is down end to end (every attempt in the
		// bounded window), so nothing is known about the message except that
		// Slack accepted the write. It comes back up before the retry.
		if strings.HasPrefix(r.URL.Path, "/conversations.") &&
			reads.Add(1) <= int32(slackReadbackTimings.attempts) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fake(w, r)
	}))
	t.Cleanup(server.Close)
	originalBase := slackAPIBase
	slackAPIBase = server.URL
	t.Cleanup(func() { slackAPIBase = originalBase })

	handler := handlePublish(config{slackBotToken: "xoxb-test"}, nil, nil, newPublishDedupCache(publishDedupTTL))
	publish := func() publishReceipt {
		t.Helper()
		body := `{"session_id":"gc-1","conversation":{"conversation_id":"C123","kind":"room"},"text":"hello","idempotency_key":"k-1"}`
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(body)))
		var receipt publishReceipt
		if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
			t.Fatalf("decode receipt %q: %v", recorder.Body.String(), err)
		}
		return receipt
	}

	first := publish()
	if first.Delivered || first.MessageID != "1700.001" {
		t.Fatalf("first receipt = %+v, want undelivered with the real ts Slack answered", first)
	}

	second := publish()
	if !second.Delivered || second.MessageID != "1700.001" {
		t.Fatalf("retry receipt = %+v, want the re-verified message marked delivered", second)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("chat.postMessage called %d times, want 1: the retry must re-verify, not re-post", got)
	}
	if second.Metadata[receiptMetadataKeyReadback] != "" {
		t.Errorf("re-verified receipt still blames the readback: metadata = %v", second.Metadata)
	}
}

// TestHandlePublishReleasesTheKeyWhenSlackDoesNotHaveTheMessage is the other
// side of F2, and the reason the remembered receipt is not simply replayed. If
// the re-verification positively reports the message absent, the key must be
// released so the caller's retry actually posts — remembering it forever would
// turn one lost message into permanent silence on that key.
func TestHandlePublishReleasesTheKeyWhenSlackDoesNotHaveTheMessage(t *testing.T) {
	fastReadbackTimings(t)
	var posts, reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/conversations.") {
			// First publish: the read leg is down, so the receipt is
			// remembered. Afterwards the read works and reports the message
			// absent, which releases the key.
			if reads.Add(1) <= int32(slackReadbackTimings.attempts) {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
			return
		}
		posts.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C123","ts":"1700.001"}`))
	}))
	t.Cleanup(server.Close)
	originalBase := slackAPIBase
	slackAPIBase = server.URL
	t.Cleanup(func() { slackAPIBase = originalBase })

	handler := handlePublish(config{slackBotToken: "xoxb-test"}, nil, nil, newPublishDedupCache(publishDedupTTL))
	publish := func() {
		t.Helper()
		body := `{"session_id":"gc-1","conversation":{"conversation_id":"C123","kind":"room"},"text":"hello","idempotency_key":"k-1"}`
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(body)))
	}

	publish()
	if got := posts.Load(); got != 1 {
		t.Fatalf("chat.postMessage called %d times on the first publish, want 1", got)
	}
	publish()
	if got := posts.Load(); got != 2 {
		t.Fatalf("chat.postMessage called %d times, want 2: a readback that proves the message absent must free the key", got)
	}
}

func TestHandlePublishReadsBackTheRewrittenSlackContent(t *testing.T) {
	var posted slackPostMessageReq
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat.postMessage":
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Errorf("decode post: %v", err)
			}
			_, _ = w.Write([]byte(`{"ok":true,"channel":"C123","ts":"1700.001"}`))
		case "/conversations.history":
			response := map[string]any{
				"ok":       true,
				"messages": []map[string]string{{"ts": "1700.001", "text": slackCanonicalizeForTest(posted.Text)}},
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	originalBase := slackAPIBase
	slackAPIBase = server.URL
	t.Cleanup(func() { slackAPIBase = originalBase })
	aliases := newUserAliasMapForTest(t, map[string]string{"mayor": "U123ABC"})
	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(
		`{"session_id":"gc-1","conversation":{"conversation_id":"C123","kind":"room"},"text":"hello @mayor"}`,
	))
	recorder := httptest.NewRecorder()
	handlePublish(config{slackBotToken: "xoxb-test"}, nil, aliases, newPublishDedupCache(publishDedupTTL))(recorder, req)

	var receipt publishReceipt
	if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("decode receipt %q: %v", recorder.Body.String(), err)
	}
	if !receipt.Delivered {
		t.Fatalf("receipt = %+v, want delivered", receipt)
	}
	if posted.Text != "hello <@U123ABC>" {
		t.Fatalf("posted text = %q, want rewritten mention", posted.Text)
	}
}

// TestHandlePublishDeliversTextSlackCanonicalizes is F1 end to end: an ordinary
// unkeyed publish whose body carries an ampersand, a bare URL and angle
// brackets. This is what most agent traffic through this adapter looks like —
// PR links, dashboards, shell snippets — and under a byte-compare readback every
// one of these messages lands in Slack and is reported Delivered:false.
//
// The downstream consequence is what makes it a regression rather than a
// cosmetic mislabel: a non-delivered receipt was not cached, and gc's outbound
// path returns before the transcript append, so the authoritative transcript
// omits messages a human can see in the channel while the caller's retry posts
// them again.
func TestHandlePublishDeliversTextSlackCanonicalizes(t *testing.T) {
	fastReadbackTimings(t)
	const text = "R&D: ship https://github.com/o/r/pull/313 <today>"
	var captured slackPostMessageReq
	server := httptest.NewServer(newFakeSlackPublishHandler(t, &captured, "1700.001", nil))
	t.Cleanup(server.Close)
	originalBase := slackAPIBase
	slackAPIBase = server.URL
	t.Cleanup(func() { slackAPIBase = originalBase })

	body := mustJSON(t, map[string]any{
		"session_id":   "gc-1",
		"conversation": map[string]string{"conversation_id": "C123", "kind": "room"},
		"text":         text,
	})
	recorder := httptest.NewRecorder()
	handlePublish(config{slackBotToken: "xoxb-test"}, nil, nil, newPublishDedupCache(publishDedupTTL))(
		recorder, httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(body)))

	var receipt publishReceipt
	if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("decode receipt %q: %v", recorder.Body.String(), err)
	}
	if !receipt.Delivered {
		t.Fatalf("receipt = %+v, want delivered: Slack stored this message, it is just not stored byte-for-byte",
			receipt)
	}
	if stored := slackCanonicalizeForTest(captured.Text); stored == captured.Text {
		t.Fatalf("fake stored the posted text unchanged (%q); the test proves nothing about canonicalization",
			stored)
	}
}

func newSlackReadbackServer(
	t *testing.T,
	wantPath string,
	checkQuery func(url.Values),
	response string,
) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-test" {
			t.Errorf("authorization = %q, want bearer token", got)
		}
		checkQuery(r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)

	originalBase := slackAPIBase
	slackAPIBase = server.URL
	t.Cleanup(func() { slackAPIBase = originalBase })
	return server
}

// newDynamicSlackReadbackServer is newSlackReadbackServer for tests whose
// subject is the sequence of reads rather than one answer.
func newDynamicSlackReadbackServer(t *testing.T, respond func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		respond(w)
	}))
	t.Cleanup(server.Close)

	originalBase := slackAPIBase
	slackAPIBase = server.URL
	t.Cleanup(func() { slackAPIBase = originalBase })
	return server
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fake response: %v", err)
	}
	return string(encoded)
}

// testSlackBareURL matches the URL shape Slack auto-links. `|` is excluded
// because it is Slack's own label separator inside a link span.
var testSlackBareURL = regexp.MustCompile(`https?://[^\s<>|]+`)

// slackCanonicalizeForTest models what Slack does to message text between
// chat.postMessage and conversations.history: it entity-escapes `&`, `<` and
// `>`, and auto-links bare URLs into `<url>` spans. Markup spans Slack itself
// produced (`<@U…>`, `<#C…>`, `<url|label>`) are passed through, which is why
// the escape runs on the text between spans rather than over the whole string.
//
// This exists because every publish fake in this package used to echo the
// posted text back verbatim, which models a Slack that stores exactly what it
// is given. No such Slack exists, and the suite's agreement with that fiction
// is what let a byte-compare readback gate ship: the fakes could not produce
// the input that breaks it. It is deliberately an approximation — the point is
// that it is not the identity function.
func slackCanonicalizeForTest(text string) string {
	var out strings.Builder
	last := 0
	for _, span := range slackAngleSpan.FindAllStringIndex(text, -1) {
		out.WriteString(canonicalizePlainTextForTest(text[last:span[0]]))
		out.WriteString(text[span[0]:span[1]])
		last = span[1]
	}
	out.WriteString(canonicalizePlainTextForTest(text[last:]))
	return out.String()
}

// canonicalizePlainTextForTest escapes UNCONDITIONALLY: it does not ask whether
// the `&` it is escaping already begins a valid entity, so it takes a posted
// "&amp;" to "&amp;amp;". That models a *stricter* Slack than the documented
// one — Slack tells senders to escape `&` themselves and promises that only the
// listed characters are decoded for display, a rule that is self-defeating if
// the platform then escaped the escape. The approximation is deliberate and
// safe for every other row (nothing else in the suite posts entity-shaped
// text), but it is the reason the entity-literal row in
// TestReadBackPublishedMessageAcceptsSlackCanonicalizedText pins an unconfirmed
// outcome that real Slack is expected to confirm. Read that row before treating
// this function as evidence about the platform.
func canonicalizePlainTextForTest(text string) string {
	escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
	return testSlackBareURL.ReplaceAllString(escaped, "<$0>")
}

// newFakeSlackPublishHandler is a fake Slack for tests whose subject is what
// handlePublish POSTS, not whether the readback works. It records the
// chat.postMessage into captured, then answers the readback that handlePublish
// now performs by serving that text at ts — canonicalized the way Slack
// canonicalizes it — on conversations.history for a top-level post and
// conversations.replies for a threaded one.
//
// Without this a fake that answers every path with a post response fails the
// readback two ways at once: the GET carries no body, so a handler that decodes
// unconditionally reports EOF, and the reply parses as zero messages, so the
// receipt comes back Delivered:false. Either one would make an assertion about
// the posted text measure the readback instead of its own subject.
//
// onPost, when non-nil, runs on the post path only, so a caller counting posts
// counts writes and not readbacks.
func newFakeSlackPublishHandler(t *testing.T, captured *slackPostMessageReq, ts string, onPost func()) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.history", "/conversations.replies":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]string{{"ts": ts, "text": slackCanonicalizeForTest(captured.Text)}},
			})
		default:
			if err := json.NewDecoder(r.Body).Decode(captured); err != nil {
				t.Errorf("decode Slack request: %v", err)
			}
			if onPost != nil {
				onPost()
			}
			_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"` + ts + `"}`))
		}
	}
}

func assertSlackReadbackQuery(t *testing.T, query url.Values, channel, messageTS, limit string) {
	t.Helper()
	for key, want := range map[string]string{
		"channel":   channel,
		"oldest":    messageTS,
		"latest":    messageTS,
		"inclusive": "true",
		"limit":     limit,
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
