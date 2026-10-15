package main

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

type slackReadbackLink struct {
	textStart, textEnd int
	target, label      string
}

func slackReadbackLinks(text string) []slackReadbackLink {
	text = slackEntityUnescape.Replace(text)
	var result []slackReadbackLink
	rawEnd, textEnd := 0, 0
	for _, span := range slackCodeOrAngleSpan.FindAllStringIndex(text, -1) {
		if text[span[0]] != '<' {
			continue
		}
		target, label, _ := strings.Cut(text[span[0]+1:span[1]-1], "|")
		if slackLinkScheme.MatchString(target) {
			textStart := textEnd + len(slackNormalizeUnescapedLinks(text[rawEnd:span[0]]))
			textEnd = textStart + len(slackNormalizeUnescapedLinks(text[span[0]:span[1]]))
			rawEnd = span[1]
			result = append(result, slackReadbackLink{textStart, textEnd, target, label})
		}
	}
	return result
}

// Slack's returned auto-link supplies the literal URL boundary. Matching its
// label exactly against the original normalized text prevents emoji folding
// from erasing URL characters, without guessing whether numeric prose is a
// hostname or swallowing punctuation/emoji after a URL.
func slackAutoLinkLabelsMatch(expected, stored string) bool {
	expectedText := slackNormalizeLinks(expected)
	storedText := slackNormalizeLinks(stored)
	explicit := slackReadbackLinks(expected)
	verbatim := slackVerbatimSpan.FindAllStringIndex(expectedText, -1)
	explicitCursor, verbatimCursor := 0, 0
	expectedCursor, storedCursor := 0, 0
	for _, link := range slackReadbackLinks(stored) {
		if link.label == "" || (link.target != "http://"+link.label && link.target != "https://"+link.label &&
			!(strings.HasPrefix(link.label, "www.") && link.target == "http://"+strings.TrimPrefix(link.label, "www."))) {
			continue
		}
		prefix := storedText[storedCursor:link.textStart]
		matched := false
		// Host:port is a bare URL, but a URI scheme or HTTP userinfo needs
		// an explicit matching link: plain user@host denotes an email address.
		destination, err := url.Parse(link.target)
		requiresExplicit := err != nil || destination.User != nil || slackAbsoluteLinkScheme.MatchString(link.label)
		// Advance both sides after each match. Repeated labels must not
		// rescan earlier occurrences or re-fold growing message prefixes.
		for offset := expectedCursor; offset <= len(expectedText); {
			at := strings.Index(expectedText[offset:], link.label)
			if at < 0 {
				break
			}
			at += offset
			if slackNormalizedPrefixesMatch(expectedText[expectedCursor:at], prefix) {
				for explicitCursor < len(explicit) && explicit[explicitCursor].textEnd <= at {
					explicitCursor++
				}
				original := explicitCursor < len(explicit) && explicit[explicitCursor].textStart <= at
				if original {
					if explicit[explicitCursor].textStart != at || explicit[explicitCursor].target != link.target {
						return false
					}
				} else {
					for verbatimCursor < len(verbatim) && verbatim[verbatimCursor][1] <= at {
						verbatimCursor++
					}
					if requiresExplicit || (verbatimCursor < len(verbatim) && verbatim[verbatimCursor][0] <= at) {
						return false
					}
					// A label cannot begin inside a larger domain or URL.
					if at > 0 {
						before, _ := utf8.DecodeLastRuneInString(expectedText[:at])
						if unicode.IsLetter(before) || unicode.IsNumber(before) || strings.ContainsRune(".-/@", before) {
							return false
						}
					}
				}
				matched = true
				expectedCursor = at + len(link.label)
				storedCursor = link.textStart + len(link.label)
				break
			}
			offset = at + len(link.label)
		}
		if !matched {
			return false
		}
	}
	return true
}

// Inputs have already had links/entities normalized exactly once.
func slackNormalizedPrefixesMatch(expected, stored string) bool {
	return expected == stored || slackFoldEmoji(expected) == slackFoldEmoji(stored) ||
		slackEmojiExpansionMatchesNormalized(expected, stored)
}

// The ordinary symmetric fold cannot resolve overlapping colon tokens:
// 10:30🎉 becomes 10:30:tada:, where an unknown :30: hides the emoji's opening
// colon. Reversing the scan instead breaks ✅ok: -> :white_check_mark:ok:.
// This fallback retains the posted text's literal boundaries and permits the
// same lossy emoji class as slackFoldEmoji only where that text had an emoji.
// Code and URI spans remain exact literals. It neither guesses which unknown
// token is an emoji nor discards changed timestamp/namespace/URL characters.
//
// Matching is a small finite-state walk: a literal consumes itself; an emoji
// consumes zero or more Unicode/standard-shortcode units. Memoization and the
// state budget bound ambiguous runs. Exhaustion retains the strict mismatch;
// ordinary symmetric matches never need this fallback.
func slackEmojiExpansionMatches(expected, stored string) bool {
	return slackEmojiExpansionMatchesNormalized(slackNormalizeLinks(expected), slackNormalizeLinks(stored))
}

func slackEmojiExpansionMatchesNormalized(expected, stored string) bool {
	type part struct {
		literal string
		emoji   bool
	}
	var parts []part
	var literal strings.Builder
	flush := func() {
		if literal.Len() > 0 {
			parts = append(parts, part{literal: literal.String()})
			literal.Reset()
		}
	}
	prose := func(text string) {
		for len(text) > 0 {
			if n := slackEmojiUnitPrefix(text); n > 0 {
				flush()
				if len(parts) == 0 || !parts[len(parts)-1].emoji {
					parts = append(parts, part{emoji: true})
				}
				text = text[n:]
			} else {
				_, n := utf8.DecodeRuneInString(text)
				literal.WriteString(text[:n])
				text = text[n:]
			}
		}
	}
	last := 0
	for _, span := range slackVerbatimSpan.FindAllStringIndex(expected, -1) {
		prose(expected[last:span[0]])
		literal.WriteString(expected[span[0]:span[1]])
		last = span[1]
	}
	prose(expected[last:])
	flush()
	type state struct{ part, pos int }
	pending := []state{{}}
	seen := make(map[state]bool)
	const maxStates = 100000
	for len(pending) > 0 {
		at := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[at] {
			continue
		}
		if len(seen) >= maxStates {
			return false
		}
		seen[at] = true
		if at.part == len(parts) {
			if at.pos == len(stored) {
				return true
			}
			continue
		}
		p := parts[at.part]
		if p.emoji {
			if n := slackEmojiUnitPrefix(stored[at.pos:]); n > 0 {
				pending = append(pending, state{at.part, at.pos + n})
			}
			pending = append(pending, state{at.part + 1, at.pos})
		} else if strings.HasPrefix(stored[at.pos:], p.literal) {
			pending = append(pending, state{at.part + 1, at.pos + len(p.literal)})
		}
	}
	return false
}

// slackEmojiUnitPrefix consumes a unit at the current boundary only. Unknown
// colon tokens consume nothing, leaving their closing colon available when
// the caller advances through literal text one rune at a time.
func slackEmojiUnitPrefix(text string) int {
	if text == "" {
		return 0
	}
	if text[0] == ':' {
		if end := strings.IndexByte(text[1:], ':'); end >= 0 && slackIsStandardEmojiName(text[1:1+end]) {
			return end + 2
		}
	}
	r, n := utf8.DecodeRuneInString(text)
	if r >= '0' && r <= '9' || r == '#' || r == '*' {
		end := n
		if strings.HasPrefix(text[end:], "\ufe0f") {
			end += len("\ufe0f")
		}
		if strings.HasPrefix(text[end:], "\u20e3") {
			return end + len("\u20e3")
		}
	}
	if !isSlackEmojiRune(r) {
		return 0
	}
	for n < len(text) {
		next, width := utf8.DecodeRuneInString(text[n:])
		if !isEmojiSequenceGlue(next) {
			break
		}
		n += width
	}
	return n
}
