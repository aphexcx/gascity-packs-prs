package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// A reminder protects the entire new message, its anchor and attachments.
// The budget limits optional context and help; an oversized message is sent
// whole. All authors share the same context ordering and omission rule.

const (
	// defaultReminderTextBudget bounds one delivery's Text field
	// (SLACK_REMINDER_TEXT_BUDGET overrides; 0 disables). gc wraps
	// Text in ~300 bytes of reminder envelope; 3500 keeps the whole
	// rendered reminder comfortably bounded without touching normal
	// traffic — the observed offender was boilerplate riding small
	// messages, not large bodies.
	defaultReminderTextBudget = 3500
	// threadAnchorParentLineRunes caps the parent's quoted first line
	// inside the thread-reply anchor.
	threadAnchorParentLineRunes = 120
)

// channelReminderParts are the per-message pieces of one channel
// delivery, in layout order. anchor/preamble/files may be empty; body
// is the whole (rewritten) message text. Coordinates remain available to
// older spool entries and callers.
type channelReminderParts struct {
	anchor   string
	preamble string
	// Read compatibility for older spool entries. These fields no longer
	// participate in composition: all authors use the same preamble.
	preambleLean string
	botQuotes    bool
	body         string
	files        string
	ts           string
	channelID    string
}

// assemble joins the message unit around the given body, reproducing
// the legacy joins exactly: preamble is already newline-terminated,
// the files block attaches with a blank line (or stands alone when
// everything else is blank), and the anchor leads on its own line.
func (p channelReminderParts) assemble(body string) string {
	unit := p.preamble + body
	if p.anchor != "" {
		unit = p.anchor + "\n" + unit
	}
	if p.files != "" {
		if strings.TrimSpace(unit) == "" {
			unit = p.files
		} else {
			unit += "\n\n" + p.files
		}
	}
	return unit
}

// contextOmissionNotice counts quoted messages without inspecting body text.
// Every quote occupies one line; context text has already collapsed newlines.
func contextOmissionNotice(preamble string) string {
	count := 0
	for _, line := range strings.Split(preamble, "\n") {
		if strings.HasPrefix(line, "@") || strings.HasPrefix(line, "[bot ") {
			count++
		}
	}
	if count == 0 {
		count = 1
	}
	return fmt.Sprintf("[%d omitted — thread context over budget]\n\n", count)
}

// composeChannelReminderText sheds help, recovery, then the complete context
// window, with no author preference. The current message is never clipped.
func composeChannelReminderText(p channelReminderParts, peerBlock, helpBlock string, budget int) (text string, usedPeer, usedHelp, usedPreamble, trimmed bool) {
	unit := p.assemble(p.body)
	withPeer := unit
	if peerBlock != "" {
		withPeer = peerBlock + "\n\n" + unit
	}
	full := withPeer
	if helpBlock != "" {
		full += "\n\n" + helpBlock
	}
	if budget <= 0 || len(full) <= budget {
		return full, peerBlock != "", helpBlock != "", true, false
	}
	if len(withPeer) <= budget {
		return withPeer, peerBlock != "", false, true, false
	}
	if len(unit) <= budget {
		return unit, false, false, true, false
	}
	usedPreamble = true
	if p.preamble != "" {
		usedPreamble = false
		p.preamble = contextOmissionNotice(p.preamble)
	}
	return p.assemble(p.body), false, false, usedPreamble, false
}

// formatThreadReplyAnchor renders the protected first line of a
// thread-reply delivery: the thread ts an agent replies with, and the
// parent's first line for orientation (bead gp-0qw item 2 — the 8/26
// incident delivered a thread reply the reader could not place, and
// the approval it carried went unread for ~50 minutes). Empty
// parentAuthor/parentFirstLine degrade gracefully — the anchor always
// carries the thread ts. Every interpolation is neutralized: thread
// ts, author, and parent text are provider-supplied.
//
// The anchor names the thread, not a flag (jg-fc1csv): it used to say
// "reply with --thread-ts <ts>", a flag reply-current does not take —
// the per-message reply line already spells the command with
// --reply-to — so the wording was both stale and per-message overhead.
func formatThreadReplyAnchor(threadTS, parentAuthor, parentFirstLine string) string {
	if threadTS == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("[reply in thread ")
	b.WriteString(neutralizeMarkupBoundaries(threadTS))
	if line := firstLineClipped(parentFirstLine, threadAnchorParentLineRunes); line != "" {
		b.WriteString("; parent")
		if parentAuthor != "" {
			b.WriteString(" @")
			b.WriteString(neutralizeMarkupBoundaries(parentAuthor))
		}
		b.WriteString(": \"")
		b.WriteString(neutralizeMarkupBoundaries(line))
		b.WriteString("\"")
	}
	b.WriteString("]")
	return b.String()
}

// firstLineClipped returns the first non-blank-trimmed line of s,
// clipped to maxRunes runes with an ellipsis when longer.
func firstLineClipped(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if b := runePrefixBytes(s, maxRunes); b < len(s) {
		return s[:b] + "…"
	}
	return s
}

// runePrefixBytes returns the byte length of the first n runes of s
// (len(s) when s has fewer).
func runePrefixBytes(s string, n int) int {
	count := 0
	for i := range s {
		if count == n {
			return i
		}
		count++
	}
	return len(s)
}

// clipRuneSafe cuts s to at most n bytes without splitting a rune.
func clipRuneSafe(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
