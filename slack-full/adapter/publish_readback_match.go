package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type slackReadbackLink struct {
	textStart, textEnd int
	target, label      string
	labeled            bool
}

func slackReadbackLinks(text string) ([]slackReadbackLink, [][2]int) {
	text = slackEntityUnescape.Replace(text)
	var result []slackReadbackLink
	var markup [][2]int
	rawEnd, textEnd := 0, 0
	for _, span := range slackCodeOrAngleSpan.FindAllStringIndex(text, -1) {
		textStart := textEnd + len(slackNormalizeUnescapedLinks(text[rawEnd:span[0]]))
		textEnd = textStart + len(slackNormalizeUnescapedLinks(text[span[0]:span[1]]))
		rawEnd = span[1]
		markup = append(markup, [2]int{textStart, textEnd})
		if text[span[0]] != '<' {
			continue
		}
		target, label, labeled := strings.Cut(text[span[0]+1:span[1]-1], "|")
		// A colon in angle-delimited prose is not enough to make a link.
		if strings.IndexFunc(target, unicode.IsSpace) < 0 && (slackReferenceTarget.MatchString(target) || slackAbsoluteLinkScheme.MatchString(target) ||
			strings.HasPrefix(target, "mailto:") || (labeled && slackLinkScheme.MatchString(target))) {
			result = append(result, slackReadbackLink{textStart, textEnd, target, label, labeled})
		}
	}
	return result, markup
}

// Slack's returned auto-link supplies the literal URL boundary. Matching its
// label exactly against the original normalized text prevents emoji folding
// from erasing URL characters, without guessing whether numeric prose is a
// hostname or swallowing punctuation/emoji after a URL. Explicit targets are
// checked separately from their display labels; all intervening and trailing
// text is compared with the existing prose emoji rules.
func slackReadbackTextMatches(expected, stored string) bool {
	expectedText, storedText := slackNormalizeLinks(expected), slackNormalizeLinks(stored)
	expectedLiterals, storedLiterals := slackLiteralPrefixCounts(expectedText), slackLiteralPrefixCounts(storedText)
	explicit, markup := slackReadbackLinks(expected)
	storedLinks, storedMarkup := slackReadbackLinks(stored)
	storedMarkupStarts := make(map[int]bool, len(storedMarkup))
	for _, span := range storedMarkup {
		storedMarkupStarts[span[0]] = true
	}
	// Removing angle markup must not join an explicit URI to its neighbors.
	// Scan each original span separately, retaining normalized byte offsets.
	var verbatim [][]int
	appendVerbatim := func(start, end int) {
		for _, span := range slackVerbatimSpan.FindAllStringIndex(expectedText[start:end], -1) {
			verbatim = append(verbatim, []int{start + span[0], start + span[1]})
		}
	}
	last := 0
	markupEnds := make(map[int]bool, len(markup))
	for _, span := range markup {
		appendVerbatim(last, span[0])
		appendVerbatim(span[0], span[1])
		last = span[1]
		markupEnds[last] = true
	}
	appendVerbatim(last, len(expectedText))
	explicitCursor, verbatimCursor := 0, 0
	suffixMarkupCursor := 0
	expectedCursor, storedCursor := 0, 0
	type suffixCheck struct {
		linkStart, expectedStart, storedStart int
		expectedEnd, storedEnd, limit         int
		prefix                                string
		shortcodes, emoji, markup             bool
	}
	var pendingSuffix *suffixCheck
	var suffixes []*suffixCheck
	var matchedSpans [][2]int
	finishSuffix := func(expectedEnd, storedEnd int) {
		if pendingSuffix != nil {
			pendingSuffix.expectedEnd = min(expectedEnd, pendingSuffix.limit)
			pendingSuffix.storedEnd = storedEnd
			suffixes = append(suffixes, pendingSuffix)
		}
	}
	// A mismatch with many repeated labels must not repeatedly fold growing
	// prefixes. Successful alignment consumes each span once; ambiguous failed
	// candidates share this byte budget, like the emoji fallback's state budget.
	budget := 8*(len(expectedText)+len(storedText)) + 100000
	compare := func(a, b string) bool {
		budget -= len(a) + len(b) + 1
		return budget >= 0 && slackNormalizedPrefixesMatch(a, b)
	}
	for _, link := range storedLinks {
		implicit := link.label != "" && (link.target == "http://"+link.label || link.target == "https://"+link.label ||
			(strings.HasPrefix(link.label, "www.") && len(link.label) > len("www.") && link.target == "http://"+strings.TrimPrefix(link.label, "www.")))
		fullURI := slackAbsoluteLinkScheme.MatchString(link.target) && (!link.labeled || link.label == link.target)
		prefix := storedText[storedCursor:link.textStart]
		rendered := storedText[link.textStart:link.textEnd]
		if rendered == "" {
			return false
		}
		exactPrefix := strings.HasPrefix(expectedText[expectedCursor:], prefix)
		if explicitCursor < len(explicit) {
			original := explicit[explicitCursor]
			// Use the known source offset before searching repeated labels.
			// When the prefix is exact, it already identifies the offset.
			// Literal counts reject a far-away same-target link in constant
			// time, so intervening auto-links do not repeatedly fold its prefix.
			sameLiterals := expectedLiterals[original.textStart]-expectedLiterals[expectedCursor] == storedLiterals[link.textStart]-storedLiterals[storedCursor]
			if original.target == link.target && sameLiterals && (!exactPrefix || original.textStart == expectedCursor+len(prefix)) &&
				compare(expectedText[expectedCursor:original.textStart], prefix) && slackExplicitLabelsMatch(original, link) {
				finishSuffix(original.textStart, link.textStart)
				pendingSuffix = nil
				matchedSpans = append(matchedSpans, [2]int{original.textStart, original.textEnd})
				expectedCursor, storedCursor = original.textEnd, link.textEnd
				explicitCursor++
				continue
			}
		}
		matched := false
		// Exact prefixes give the candidate directly, even if a message repeats
		// a domain many times in prose before Slack links its last occurrence.
		offset := expectedCursor
		candidateVerbatim := verbatimCursor
		if exactPrefix {
			offset += len(prefix)
		}
		for offset <= len(expectedText) && budget >= 0 {
			at := strings.Index(expectedText[offset:], rendered)
			if at < 0 {
				break
			}
			at += offset
			for candidateVerbatim < len(verbatim) && verbatim[candidateVerbatim][1] <= at {
				candidateVerbatim++
			}
			if candidateVerbatim < len(verbatim) && verbatim[candidateVerbatim][0] <= at && expectedText[verbatim[candidateVerbatim][0]] == '`' {
				offset = verbatim[candidateVerbatim][1]
				continue
			}
			sameLiterals := expectedLiterals[at]-expectedLiterals[expectedCursor] == storedLiterals[link.textStart]-storedLiterals[storedCursor]
			if sameLiterals && compare(expectedText[expectedCursor:at], prefix) {
				end := at + len(rendered)
				var nextSuffix *suffixCheck
				original := explicitCursor < len(explicit) && explicit[explicitCursor].textStart < end
				if original {
					if explicit[explicitCursor].textStart != at || explicit[explicitCursor].textEnd != end || explicit[explicitCursor].target != link.target {
						return false
					}
					explicitCursor++
				} else if implicit || fullURI {
					_, authority, _ := strings.Cut(link.target, "://")
					hasPath := strings.IndexAny(authority, "/?#")
					if hasPath >= 0 {
						authority = authority[:hasPath]
					}
					for verbatimCursor < len(verbatim) && verbatim[verbatimCursor][1] <= at {
						verbatimCursor++
					}
					inVerbatim := verbatimCursor < len(verbatim) && verbatim[verbatimCursor][0] <= at
					if inVerbatim && verbatim[verbatimCursor][0] < expectedCursor {
						// A previously matched link supplies its own end, even
						// when the URI regex extends across adjacent punctuation.
						// Scan only the unconsumed prefix to retain protection for
						// any new URI there without rescanning the remaining text.
						prefix := expectedText[expectedCursor:at]
						spans := slackVerbatimSpan.FindAllStringIndex(prefix, -1)
						inVerbatim = len(spans) > 0 && spans[len(spans)-1][1] == len(prefix) && prefix[spans[len(spans)-1][0]] != '`'
					}
					// Slack can leave an earlier URI unwrapped while linking
					// the next comma/semicolon-separated URL. Its already
					// matched prefix retains the earlier URI's literal text.
					if at > 0 && strings.ContainsRune(",;", rune(expectedText[at-1])) {
						inVerbatim = false
					}
					if (implicit && (strings.Contains(authority, "@") || slackAbsoluteLinkScheme.MatchString(link.label))) ||
						(inVerbatim && (implicit || verbatim[verbatimCursor][0] != at)) {
						return false
					}
					if implicit && at > 0 && !markupEnds[at] {
						before, _ := utf8.DecodeLastRuneInString(expectedText[:at])
						if unicode.IsLetter(before) || unicode.IsNumber(before) || strings.ContainsRune(".-/@", before) {
							return false
						}
					}
					for suffixMarkupCursor < len(markup) && markup[suffixMarkupCursor][0] < end {
						suffixMarkupCursor++
					}
					limit := len(expectedText)
					if suffixMarkupCursor < len(markup) {
						limit = markup[suffixMarkupCursor][0]
					}
					// Check only the suffix before the next aligned link or
					// original markup span. Each link retains its own rules and
					// each suffix is scanned once, without a shared range cache.
					nextSuffix = &suffixCheck{
						linkStart: at, expectedStart: end, storedStart: link.textEnd,
						limit: limit, prefix: rendered,
						shortcodes: implicit && hasPath < 0, emoji: implicit, markup: suffixMarkupCursor < len(markup),
					}
				}
				finishSuffix(at, link.textStart)
				pendingSuffix = nextSuffix
				matchedSpans = append(matchedSpans, [2]int{at, end})
				expectedCursor, storedCursor = end, link.textEnd
				matched = true
				break
			}
			offset = at + len(rendered)
		}
		if !matched {
			return false
		}
	}
	if explicitCursor != len(explicit) || !compare(expectedText[expectedCursor:], storedText[storedCursor:]) {
		return false
	}
	finishSuffix(len(expectedText), len(storedText))
	if len(suffixes) == 0 {
		return true
	}
	// Verify each suffix as the prose scan reaches its link. Only the state
	// before that link can close formatting; literal URL tail characters are
	// excluded before scanning subsequent prose and links.
	suffixCursor := 0
	checkSuffix := func(at int, state uint8) (int, bool) {
		if suffixCursor == len(suffixes) || suffixes[suffixCursor].linkStart != at {
			return at, true
		}
		p := suffixes[suffixCursor]
		suffixCursor++
		atMarkupEnd := func(offset int) bool {
			return p.markup && p.expectedEnd == p.limit && storedMarkupStarts[p.storedStart+offset]
		}
		atProseBoundary := func(offset int) bool {
			at := p.expectedStart + offset
			if slackReadbackMarker(expectedText, at)&state == 0 {
				return false
			}
			// A marker followed by a URI continuation belongs to the path,
			// query or fragment, even inside surrounding formatting.
			return at+1 == len(expectedText) || !strings.ContainsRune("/?#", rune(expectedText[at+1]))
		}
		literal, ok := slackPreservedURLSuffix(expectedText[p.expectedStart:p.expectedEnd], storedText[p.storedStart:p.storedEnd], p.prefix, p.shortcodes, p.emoji, atMarkupEnd, atProseBoundary)
		return p.expectedStart + literal, ok
	}
	return slackReadbackFormattingMatches(expectedText, markup, matchedSpans, checkSuffix)
}

// Track surrounding mrkdwn delimiters in prose. Code/link markup and escaped
// markers do not change the surrounding formatting state. An opening marker
// needs a word boundary, so identifiers such as status_value remain literal.
func slackReadbackFormattingMatches(text string, markup, links [][2]int, checkSuffix func(int, uint8) (int, bool)) bool {
	// Merge known boundaries, scanning only the intervening text for URI/code
	// tokens. A URI that Slack leaves unwrapped is just as literal as a link.
	var opaque [][2]int
	last := 0
	appendGap := func(end int) {
		if end > last {
			for _, span := range slackVerbatimSpan.FindAllStringIndex(text[last:end], -1) {
				opaque = append(opaque, [2]int{last + span[0], last + span[1]})
			}
		}
	}
	for m, l := 0, 0; m < len(markup) || l < len(links); {
		var span [2]int
		if l == len(links) || (m < len(markup) && markup[m][0] <= links[l][0]) {
			span = markup[m]
			m++
		} else {
			span = links[l]
			l++
		}
		appendGap(span[0])
		opaque = append(opaque, span)
		last = max(last, span[1])
	}
	appendGap(len(text))
	var state uint8
	cursor, skipEnd := 0, 0
	for i := range len(text) {
		end, ok := checkSuffix(i, state)
		if !ok {
			return false
		}
		skipEnd = max(skipEnd, end)
		for cursor < len(opaque) && opaque[cursor][0] <= i {
			skipEnd = max(skipEnd, opaque[cursor][1])
			cursor++
		}
		if i < skipEnd {
			continue
		}
		// Shortcode identifiers (including custom names) and Unicode
		// keycaps are atomic text, not formatting delimiters.
		if text[i] == ':' {
			if end := strings.IndexByte(text[i+1:], ':'); end >= 0 && slackEmojiShortcode.MatchString(strings.ToLower(text[i:i+end+2])) {
				skipEnd = i + end + 2
				continue
			}
		}
		mask := slackReadbackMarker(text, i)
		if mask == 0 {
			continue
		}
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[i+1:])
		beforeWord := unicode.IsLetter(before) || unicode.IsNumber(before) || before == '_'
		quote := text[i] == '\'' || text[i] == '"'

		if state&mask != 0 || ((quote || !beforeWord) && !unicode.IsSpace(after) && i+1 < len(text)) {
			state ^= mask
		}
	}
	return true
}

// Return only actual formatting characters, excluding escapes, keycaps and
// apostrophes inside words. The same rule governs prose and URL boundaries.
func slackReadbackMarker(text string, at int) uint8 {
	bit := strings.IndexByte("*_~'\"", text[at])
	if bit < 0 || (text[at] == '*' && slackEmojiUnitPrefix(text[at:]) > 0) || slackReadbackEscaped(text, at) {
		return 0
	}
	if text[at] == '\'' {
		before, _ := utf8.DecodeLastRuneInString(text[:at])
		after, _ := utf8.DecodeRuneInString(text[at+1:])
		word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }
		if word(before) && word(after) {
			return 0
		}
	}
	return uint8(1 << bit)
}

func slackReadbackEscaped(text string, at int) bool {
	slashes := 0
	for i := at - 1; i >= 0 && text[i] == '\\'; i-- {
		slashes++
	}
	return slashes%2 != 0
}

func slackLiteralPrefixCounts(text string) []int {
	counts := make([]int, len(text)+1)
	for i := range len(text) {
		counts[i+1] = counts[i]
		// Neither shortcode/Unicode emoji folding nor expansion consumes
		// these ASCII separators; equality of their count is necessary.
		if strings.ContainsRune(" \t\r\n/@.,;()[]{}<>|=\\!?'\"", rune(text[i])) {
			counts[i+1]++
		}
	}
	return counts
}

func slackExplicitLabelsMatch(expected, stored slackReadbackLink) bool {
	if slackReferenceTarget.MatchString(expected.target) {
		return true // Reference identity is its already-verified target.
	}
	label := func(link slackReadbackLink) string {
		if link.labeled {
			return link.label
		}
		return strings.TrimPrefix(link.target, "mailto:")
	}
	return slackNormalizedPrefixesMatch(label(expected), label(stored))
}

// A suffix outside Slack's returned URL span must still be present. Unlike
// the ordinary prose fold, emoji may convert but cannot disappear here.
// Typed shortcode-shaped URL paths stay literal; leading hostname-adjacent
// shortcodes are prose (they cannot be a numeric port).
func slackPreservedURLSuffix(expected, stored, urlPrefix string, allowShortcodes, allowEmoji bool, atMarkupEnd, atProseBoundary func(int) bool) (int, bool) {
	if !allowEmoji {
		// Preserve the entire remaining source URI token, using the same
		// delimiters as slackVerbatimSpan. Quotes and punctuation can be
		// literal path/query characters, regardless of Slack's link end.
		literal := slackURIToken(expected)
		return len(literal), literal == slackURIToken(stored) || (literal == expected && strings.HasPrefix(stored, literal) && atMarkupEnd(len(literal)))
	}
	var depth [3]int
	for _, r := range urlPrefix {
		if at := strings.IndexRune("([{", r); at >= 0 {
			depth[at]++
		} else if at := strings.IndexRune(")]}", r); at >= 0 && depth[at] > 0 {
			depth[at]--
		}
	}
	i, j := 0, 0
	for i < len(expected) {
		r, width := utf8.DecodeRuneInString(expected[i:])
		if unicode.IsSpace(r) || atProseBoundary(i) {
			return i, strings.HasPrefix(stored[j:], expected[i:i+width])
		}
		if at := strings.IndexRune(")]}", r); at >= 0 {
			if depth[at] == 0 {
				return i, strings.HasPrefix(stored[j:], expected[i:i+width])
			}
			depth[at]--
		} else if at := strings.IndexRune("([{", r); at >= 0 {
			depth[at]++
		}
		if allowShortcodes {
			// These characters end a bare hostname. A slash, query,
			// fragment, port or continued domain still keeps URL protection.
			if strings.ContainsRune(",;!({'\"~=&", r) || (r == '*' && slackEmojiUnitPrefix(expected[i:]) == 0) {
				return i, strings.HasPrefix(stored[j:], expected[i:i+width])
			}
			if r == '.' {
				next, _ := utf8.DecodeRuneInString(expected[i+width:])
				if !unicode.IsLetter(next) && !unicode.IsNumber(next) && next != '-' && next != '_' {
					return i, strings.HasPrefix(stored[j:], expected[i:i+width])
				}
			}
		}
		if r != ':' || allowShortcodes {
			if n := slackEmojiUnitPrefix(expected[i:]); n > 0 {
				if allowShortcodes {
					n = slackEmojiToneSuffix(expected[i:], n)
				}
				m := slackEmojiUnitPrefix(stored[j:])
				if m == 0 {
					return 0, false
				}
				// A typed tone token in a URL suffix remains literal, even
				// when Slack appends its own tone token to the preceding emoji.
				if !allowShortcodes {
					toneEnd := slackEmojiToneSuffix(expected[i:], n)
					literalTones := expected[i+n : i+toneEnd]
					if literalTones != "" && m > len(literalTones) && strings.HasSuffix(stored[j:j+m], literalTones) {
						m -= len(literalTones)
					}
				}
				i, j = i+n, j+m
				continue
			}
		}
		allowShortcodes = false
		if !strings.HasPrefix(stored[j:], expected[i:i+width]) {
			return 0, false
		}
		i, j = i+width, j+width
	}
	return i, j == len(stored) || atMarkupEnd(j)
}

func slackURIToken(text string) string {
	if end := strings.IndexAny(text, " \t\n\f\r<>|"); end >= 0 {
		return text[:end]
	}
	return text
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
			return slackEmojiToneSuffix(text, end+2)
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
	// Regional indicators form one flag; a ZWJ joins the following emoji
	// base into the same unit. Slack stores each such sequence as one name.
	if r >= 0x1F1E6 && r <= 0x1F1FF && n < len(text) {
		next, width := utf8.DecodeRuneInString(text[n:])
		if next >= 0x1F1E6 && next <= 0x1F1FF {
			n += width
		}
	}
	joined := false
	for n < len(text) {
		next, width := utf8.DecodeRuneInString(text[n:])
		if isEmojiSequenceGlue(next) {
			joined = joined || next == 0x200D
		} else if next >= 0x1F3FB && next <= 0x1F3FF {
			// A Unicode skin tone belongs to the preceding emoji base.
		} else if joined && isSlackEmojiRune(next) {
			joined = false
		} else {
			break
		}
		n += width
	}
	return n
}

// Slack represents a toned emoji with a base shortcode plus a tone shortcode.
func slackEmojiToneSuffix(text string, n int) int {
	for strings.HasPrefix(text[n:], ":skin-tone-") {
		end := strings.IndexByte(text[n+1:], ':')
		if end < 0 || !slackIsStandardEmojiName(text[n+1:n+1+end]) {
			break
		}
		n += end + 2
	}
	return n
}
