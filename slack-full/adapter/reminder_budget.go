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
	// A protected unit that exhausts the text budget still carries a small
	// context window. SLACK_REMINDER_CONTEXT_FLOOR overrides this fallback.
	defaultReminderContextFloor = 1200
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

// contextLines excludes framing, but retains every quote and notice. Notices
// describe the window rather than a dated post, so quotes get first choice of
// space, newest first. The selected suffix is still displayed chronologically.
func contextLines(preamble string) []string {
	var notices, quotes []string
	for _, line := range strings.Split(preamble, "\n") {
		if strings.TrimSpace(line) == "" || line == "---" || strings.HasPrefix(line, "Thread context (") {
			continue
		}
		if strings.HasPrefix(line, "@") || strings.HasPrefix(line, "[bot ") {
			quotes = append(quotes, line)
		} else {
			notices = append(notices, line)
		}
	}
	return append(notices, quotes...)
}

func contextOmissionNotice(count int) string {
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("[%d omitted — thread context over budget]\n", count)
}

// fitThreadContext retains a whole newest suffix and counts all older lines,
// including already-delivered and incomplete-window notices. Only an oversized
// newest line is clipped. Even a tiny budget retains its truncation marker and
// the count; these mandatory diagnostics may exceed that pathological budget.
func fitThreadContext(preamble string, budget int) string {
	if len(preamble) <= budget {
		return preamble
	}
	lines := contextLines(preamble)
	if len(lines) == 0 {
		return ""
	}
	// Removing framing may make the entire window fit. Check that before
	// reserving an omission count that is unnecessary when no line is lost.
	compact := strings.Join(lines, "\n") + "\n\n"
	if len(compact) <= budget {
		return compact
	}
	selected := ""
	for i := len(lines) - 1; i >= 0; i-- {
		candidate := lines[i] + "\n" + selected
		if len(candidate)+len(contextOmissionNotice(i))+1 > budget {
			if selected == "" {
				const marker = "… [truncated]"
				available := budget - len(contextOmissionNotice(i)) - len(marker) - 2
				selected = clipRuneSafe(lines[i], available) + marker + "\n"
				return selected + contextOmissionNotice(i) + "\n"
			}
			return selected + contextOmissionNotice(i+1) + "\n"
		}
		selected = candidate
	}
	return selected + "\n"
}

// composeChannelReminderText sheds help and optional recovery before fitting
// context, with no author preference. The current message is never clipped.
// usedPreamble reports an unchanged preamble, not whether any context survived.
func composeChannelReminderText(p channelReminderParts, peerBlock, helpBlock string, budget int, contextFloors ...int) (text string, usedPeer, usedHelp, usedPreamble, trimmed bool) {
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
		original := p.preamble
		// Measure the surrounding unit with context present: a files-only
		// post gains its attachment separator when context is included.
		contextBudget := budget - (len(unit) - len(original))
		if contextBudget <= 0 {
			contextBudget = defaultReminderContextFloor
			if len(contextFloors) > 0 && contextFloors[0] > 0 {
				contextBudget = contextFloors[0]
			}
		}
		p.preamble = fitThreadContext(original, contextBudget)
		usedPreamble = p.preamble == original
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
