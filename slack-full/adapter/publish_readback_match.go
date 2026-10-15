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
	expectedText, storedText := slackNormalizeLinks(expected), slackNormalizeLinks(stored)
	explicit := slackReadbackLinks(expected)
	verbatim := slackVerbatimSpan.FindAllStringIndex(expectedText, -1)
	explicitCursor, verbatimCursor := 0, 0
	expectedCursor, storedCursor := 0, 0
	// A mismatch with many repeated labels must not repeatedly fold growing
	// prefixes. Successful alignment consumes each span once; ambiguous failed
	// candidates share this byte budget, like the emoji fallback's state budget.
	budget := 8*(len(expectedText)+len(storedText)) + 100000
	compare := func(a, b string) bool {
		budget -= len(a) + len(b) + 1
		return budget >= 0 && slackNormalizedPrefixesMatch(a, b)
	}
	for _, link := range slackReadbackLinks(stored) {
		implicit := link.label != "" && (link.target == "http://"+link.label || link.target == "https://"+link.label ||
			(strings.HasPrefix(link.label, "www.") && len(link.label) > len("www.") && link.target == "http://"+strings.TrimPrefix(link.label, "www.")))
		prefix := storedText[storedCursor:link.textStart]
		rendered := storedText[link.textStart:link.textEnd]
		if rendered == "" {
			return false
		}
		matched := false
		// Exact prefixes give the candidate directly, even if a message repeats
		// a domain many times in prose before Slack links its last occurrence.
		offset := expectedCursor
		if strings.HasPrefix(expectedText[offset:], prefix) {
			offset += len(prefix)
		}
		for offset <= len(expectedText) && budget >= 0 {
			at := strings.Index(expectedText[offset:], rendered)
			if at < 0 {
				break
			}
			at += offset
			if compare(expectedText[expectedCursor:at], prefix) {
				end := at + len(rendered)
				original := explicitCursor < len(explicit) && explicit[explicitCursor].textStart < end
				if original {
					if explicit[explicitCursor].textStart != at || explicit[explicitCursor].textEnd != end || explicit[explicitCursor].target != link.target {
						return false
					}
					explicitCursor++
				} else if implicit {
					destination, err := url.Parse(link.target)
					for verbatimCursor < len(verbatim) && verbatim[verbatimCursor][1] <= at {
						verbatimCursor++
					}
					if err != nil || destination.User != nil || slackAbsoluteLinkScheme.MatchString(link.label) ||
						(verbatimCursor < len(verbatim) && verbatim[verbatimCursor][0] <= at) {
						return false
					}
					if at > 0 {
						before, _ := utf8.DecodeLastRuneInString(expectedText[:at])
						if unicode.IsLetter(before) || unicode.IsNumber(before) || strings.ContainsRune(".-/@", before) {
							return false
						}
					}
					if !slackAutoLinkSuffixBoundary(expectedText[end:], destination) {
						return false
					}
				}
				expectedCursor, storedCursor = end, link.textEnd
				matched = true
				break
			}
			offset = at + len(rendered)
		}
		if !matched && explicitCursor < len(explicit) {
			// Display labels can contain prose emoji. Their destination must
			// still match exactly, even if their rendered byte lengths differ.
			original := explicit[explicitCursor]
			if original.textStart >= expectedCursor && original.target == link.target &&
				compare(expectedText[expectedCursor:original.textStart], prefix) &&
				compare(expectedText[original.textStart:original.textEnd], rendered) {
				expectedCursor, storedCursor = original.textEnd, link.textEnd
				explicitCursor++
				matched = true
			}
		}
		if !matched {
			return false
		}
	}
	return explicitCursor == len(explicit)
}

// Do not let a shortened URL consume only the prefix of its literal path and
// leave an emoji-shaped path suffix for the prose fold to erase. Whitespace,
// closing punctuation and trailing sentence punctuation end the URL. A bare
// hostname may be followed immediately by prose emoji; a path/query may not.
func slackAutoLinkSuffixBoundary(suffix string, destination *url.URL) bool {
	if suffix == "" {
		return true
	}
	if destination.Path == "" && destination.RawQuery == "" && destination.Fragment == "" {
		if slackEmojiUnitPrefix(suffix) > 0 || strings.ContainsRune(",;!*", rune(suffix[0])) {
			return true
		}
	}
	for _, r := range suffix {
		if unicode.IsSpace(r) || strings.ContainsRune(")]}`\"'<>", r) {
			return true
		}
		if !strings.ContainsRune(".,!?;:*_-", r) {
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
