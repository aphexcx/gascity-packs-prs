package main

import (
	"strings"
	"unicode/utf8"
)

// --- head-protected reminder composition (gp-0qw + gp-9gc) -------------------
//
// Two live incidents (pc_a59c241284b8, pc_b334cff7f9c6, and the 8/27
// 22:57Z escalation on gp-0qw) delivered a bound session ONLY the tail
// of an inbound reminder — the reply-hint boilerplate and a dangling
// ts, with the channel, author, and message body cut entirely. The
// downstream injection keeps the TAIL when a delivery is mangled, so
// whatever the adapter lets grow unbounded is paid for by the HEAD —
// the part that actually identifies the message.
//
// This file owns the adapter-side contract for one channel delivery's
// Text field:
//
//   - The message unit — thread anchor, thread-context preamble, body,
//     files block — always leads, and boilerplate never displaces it:
//     the once-per-channel reply how-to is appended only when the
//     delivery stays inside the budget (gp-9gc: "append how-to only if
//     budget remains"), and buffered peer context is included only when
//     it fits (it restores to its buffer and rides a later delivery).
//   - When the body ALONE overflows the budget, its tail is trimmed —
//     never the head: the first reminderBodyHeadRunes runes survive
//     unconditionally, and an explicit marker names the ts and channel
//     so the full text is always recoverable from Slack. The thread
//     anchor and the files block are never trimmed.
//   - A zero/negative budget disables all of this and reproduces the
//     legacy assembly byte-for-byte (directly-constructed test configs
//     get budget 0 and stay on the historical layout).
//
// The budget bounds the adapter's OWN rendering only. The transport
// underneath (gc's member notification) can still mangle a delivery —
// see the claim-commit contract note at the postInbound success path
// in main.go for where that boundary lies.

const (
	// defaultReminderTextBudget bounds one delivery's Text field
	// (SLACK_REMINDER_TEXT_BUDGET overrides; 0 disables). gc wraps
	// Text in ~300 bytes of reminder envelope; 3500 keeps the whole
	// rendered reminder comfortably bounded without touching normal
	// traffic — the observed offender was boilerplate riding small
	// messages, not large bodies.
	defaultReminderTextBudget = 3500
	// reminderBodyHeadRunes is the body prefix that survives trimming
	// unconditionally — the bead contract's "first ~200 chars".
	reminderBodyHeadRunes = 200
	// threadAnchorParentLineRunes caps the parent's quoted first line
	// inside the thread-reply anchor.
	threadAnchorParentLineRunes = 120
)

// channelReminderParts are the per-message pieces of one channel
// delivery, in layout order. anchor/preamble/files may be empty; body
// is the (rewritten) message text and is the only trimmable part.
// ts/channelID feed the trim marker so a trimmed body always names
// where the full text lives.
type channelReminderParts struct {
	anchor    string
	preamble  string
	body      string
	files     string
	ts        string
	channelID string
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

// preambleOmittedNotice replaces a thread-context preamble dropped for
// budget (codex round-1 finding 2: the preamble is the one unbounded
// decoration — Slack messages up to 40k chars quote into it — so a
// hard bound must be able to shed it). The thread stays reachable via
// the anchor's --thread-ts.
const preambleOmittedNotice = "[thread context omitted — delivery over budget]\n\n"

// composeChannelReminderText assembles one channel delivery's Text
// under the head-protection contract above. It reports which optional
// blocks were included so the caller can unwind the side effects of
// the ones that were not (replyHelp.unmark, peerContext.restore) or
// log the loss (usedPreamble=false), and whether the body was
// tail-trimmed. Shedding order: help block, peer block, preamble,
// body tail — the anchor, the first reminderBodyHeadRunes runes of
// body, and the files block (bounded by Slack's per-message file cap)
// are never sacrificed; only a budget smaller than that protected
// residue still overflows.
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
	// Boilerplate first (gp-9gc): the how-to re-arms via the caller and
	// rides a later, smaller delivery.
	if len(withPeer) <= budget {
		return withPeer, peerBlock != "", false, true, false
	}
	// Peer context second: it restores to its buffer, nothing is lost.
	if len(unit) <= budget {
		return unit, false, false, true, false
	}
	// Preamble third: replaced by an explicit omission notice. Unlike
	// peer/help there is nothing to unwind — the delta was consumed at
	// fetch time — so the notice (and the caller's log) is the record.
	usedPreamble = true
	if p.preamble != "" {
		usedPreamble = false
		p.preamble = preambleOmittedNotice
		unit = p.assemble(p.body)
		if len(unit) <= budget {
			return unit, false, false, false, false
		}
	}
	// Last resort: trim the body TAIL behind the protected head. A
	// delivery that still overflows after the floor is delivered
	// oversized rather than mutilated.
	if p.body == "" {
		// Nothing to trim (files-only unit); the omission notice still
		// yields if it alone breaks the bound (codex round-3 finding 1).
		if len(unit) > budget && p.preamble == preambleOmittedNotice {
			p.preamble = ""
			unit = p.assemble(p.body)
		}
		return unit, false, false, usedPreamble, false
	}
	marker := "\n[message trimmed to fit the delivery budget — full text at ts " +
		neutralizeMarkupBoundaries(p.ts) + " in channel " + neutralizeMarkupBoundaries(p.channelID) + "]"
	trim := func(pp channelReminderParts) (string, bool) {
		u := pp.assemble(pp.body)
		keep := len(pp.body) - (len(u) - budget) - len(marker)
		if floor := runePrefixBytes(pp.body, reminderBodyHeadRunes); keep < floor {
			keep = floor
		}
		if keep >= len(pp.body) {
			// The overflow is entirely marker-sized rounding; trimming
			// would only add bytes.
			return u, false
		}
		return pp.assemble(clipRuneSafe(pp.body, keep) + marker), true
	}
	text, trimmed = trim(p)
	if len(text) > budget && p.preamble == preambleOmittedNotice {
		// Even the omission notice yields before the bound breaks (codex
		// round-2 finding 1): only the anchor, the protected body head,
		// the files block, and the trim marker are tolerated overflow.
		p.preamble = ""
		text, trimmed = trim(p)
	}
	return text, false, false, usedPreamble, trimmed
}

// formatThreadReplyAnchor renders the protected first line of a
// thread-reply delivery: the thread ts an agent replies with, and the
// parent's first line for orientation (bead gp-0qw item 2 — the 8/26
// incident delivered a thread reply the reader could not place, and
// the approval it carried went unread for ~50 minutes). Empty
// parentAuthor/parentFirstLine degrade gracefully — the anchor always
// carries the thread ts. Every interpolation is neutralized: thread
// ts, author, and parent text are provider-supplied.
func formatThreadReplyAnchor(threadTS, parentAuthor, parentFirstLine string) string {
	if threadTS == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("[thread reply — reply with --thread-ts ")
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
