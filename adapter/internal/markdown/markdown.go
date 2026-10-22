// Package markdown converts the supported Markdown subset to Chat text and
// divides messages without losing source bytes.
package markdown

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ToChat translates only the supported formatting, leaving code and unrelated
// text (including whitespace) intact. Generated Chat markers are never parsed
// again as Markdown.
func ToChat(md string) string {
	var out strings.Builder
	var fence *codeFence
	for len(md) > 0 {
		end := lineEnd(md, 0)
		line := md[:end]
		if fence != nil {
			md = md[end:]
			out.WriteString(line)
			if fence.closes(line) {
				fence = nil
			}
			continue
		}
		if fence = openingFence(line); fence != nil {
			md = md[end:]
			out.WriteString(line)
			continue
		}
		// A complete code span can cross physical lines. Keep those lines
		// together so neither inline nor block formatting changes its bytes.
		for i := 0; i < end; {
			switch {
			case md[i] == '\\' && i+1 < len(md):
				i = escapedEnd(md, i)
			case md[i] == '`':
				i = codeSpanEnd(md, i)
				if i > end {
					end = lineEnd(md, i)
				}
			default:
				i++
			}
		}
		line, md = md[:end], md[end:]
		body := strings.TrimRight(line, "\r\n")
		ending := line[len(body):]
		indent := len(body) - len(strings.TrimLeft(body, " \t"))
		out.WriteString(body[:indent])
		body = body[indent:]
		heading := 0
		for heading < len(body) && body[heading] == '#' {
			heading++
		}
		switch {
		case heading > 0 && heading <= 6 && strings.HasPrefix(body[heading:], " "):
			if content := headingContent(body[heading+1:]); content != "" {
				out.WriteString("*" + inline(content, true) + "*")
			}
		case strings.HasPrefix(body, "- ") || strings.HasPrefix(body, "* "):
			out.WriteString("* " + inline(body[2:], false))
		default:
			out.WriteString(inline(body, false))
		}
		out.WriteString(ending)
	}
	return out.String()
}

func lineEnd(s string, start int) int {
	if i := strings.IndexByte(s[start:], '\n'); i >= 0 {
		return start + i + 1
	}
	return len(s)
}

func headingContent(s string) string {
	s = strings.Trim(s, " \t")
	end := len(strings.TrimRight(s, "#"))
	if end == 0 || (end < len(s) && (s[end-1] == ' ' || s[end-1] == '\t')) {
		s = strings.TrimRight(s[:end], " \t")
	}
	return s
}

func inline(s string, heading bool) string {
	var out strings.Builder
	// Failed emphasis searches remain failed at all later positions. Bracket
	// pairs are indexed once so unmatched '[' runs never rescan the suffix.
	noCloser := make(map[string]bool)
	labels := linkLabelEnds(s)
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+1 < len(s) {
			end := escapedEnd(s, i)
			out.WriteString(s[i:end])
			i = end
			continue
		}
		if s[i] == '`' {
			end := codeSpanEnd(s, i)
			out.WriteString(s[i:end])
			i = end
			continue
		}
		if s[i] == '[' {
			if label, url, end, ok := link(s, i, labels); ok {
				if i > 0 && s[i-1] == '!' {
					out.WriteString(s[i:end])
				} else {
					out.WriteString("<" + chatDestination(url) + "|" + inline(label, heading) + ">")
				}
				i = end
				continue
			}
		}
		if strings.ContainsRune("*_~", rune(s[i])) {
			n := runLength(s[i:])
			marker := s[i : i+n]
			chat := map[string]string{"**": "*", "__": "*", "*": "_", "_": "_", "~~": "~", "***": "*_", "___": "*_"}[marker]
			if chat != "" && !noCloser[marker] && i+n < len(s) && !spaceAt(s[i+n:]) && !(s[i] == '_' && wordBefore(s[:i])) {
				if end := emphasisEnd(s, i+n, marker); end >= 0 {
					if heading {
						chat = strings.TrimPrefix(chat, "*")
					}
					close := chat
					if chat == "*_" {
						close = "_*"
					}
					out.WriteString(chat + inline(s[i+n:end], heading) + close)
					i = end + n
					continue
				}
				noCloser[marker] = true
			}
			out.WriteString(marker)
			i += n
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

func runLength(s string) int {
	i := 1
	for i < len(s) && s[i] == s[0] {
		i++
	}
	return i
}

func escapedEnd(s string, i int) int {
	if strings.ContainsRune("*_~", rune(s[i+1])) {
		return i + 1 + runLength(s[i+1:])
	}
	return i + 2
}

func codeSpanEnd(s string, start int) int {
	n := runLength(s[start:])
	for i := start + n; i < len(s); i++ {
		if s[i] == '`' {
			count := runLength(s[i:])
			if count == n {
				return i + n
			}
			i += count - 1
		}
	}
	return start + n
}

func emphasisEnd(s string, start int, marker string) int {
	for i := start; i < len(s); {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			i = escapedEnd(s, i)
		case s[i] == '`':
			i = codeSpanEnd(s, i)
		case s[i] == marker[0]:
			n := runLength(s[i:])
			previous, _ := utf8.DecodeLastRuneInString(s[:i])
			if n == len(marker) && i > start && !unicode.IsSpace(previous) && !(marker[0] == '_' && wordAt(s[i+n:])) {
				return i
			}
			i += n
		default:
			i++
		}
	}
	return -1
}

func spaceAt(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsSpace(r)
}

func wordAt(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

func wordBefore(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

func linkLabelEnds(s string) map[int]int {
	ends := make(map[int]int)
	var stack []int
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '`':
			i = codeSpanEnd(s, i) - 1
		case '[':
			stack = append(stack, i)
		case ']':
			if len(stack) > 0 {
				ends[stack[len(stack)-1]] = i
				stack = stack[:len(stack)-1]
			}
		}
	}
	return ends
}

func link(s string, start int, labels map[int]int) (label, url string, end int, ok bool) {
	closeLabel, found := labels[start]
	if !found || closeLabel+1 >= len(s) || s[closeLabel+1] != '(' {
		return
	}
	urlStart := closeLabel + 2
	depth := 1
	for i := urlStart; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			continue
		}
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[start+1 : closeLabel], s[urlStart:i], i + 1, true
			}
		}
	}
	return
}

func chatDestination(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			continue
		}
		if s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n' {
			s = s[:i] // Optional Markdown titles are not part of the URL.
			break
		}
	}
	// These delimiters have structural meaning in Chat's <url|label> syntax.
	s = strings.ReplaceAll(s, "|", "%7C")
	return strings.ReplaceAll(s, ">", "%3E")
}

type codeFence struct {
	opener string
	marker string
}

func openingFence(line string) *codeFence {
	body := strings.TrimLeft(line, " ")
	indent := len(line) - len(body)
	if indent > 3 || len(body) < 3 || (body[0] != '`' && body[0] != '~') {
		return nil
	}
	n := runLength(body)
	if n < 3 || (body[0] == '`' && strings.Contains(body[n:], "`")) {
		return nil
	}
	return &codeFence{opener: line, marker: line[:indent+n]}
}

func (f *codeFence) closes(line string) bool {
	body := strings.TrimLeft(line, " ")
	marker := strings.TrimLeft(f.marker, " ")
	if len(line)-len(body) > 3 || len(body) < len(marker) || body[0] != marker[0] {
		return false
	}
	n := runLength(body)
	return n >= len(marker) && strings.TrimSpace(body[n:]) == ""
}

// Split returns parts of at most limit bytes, preferring blank lines, then
// spaces, then UTF-8 rune boundaries. Source whitespace is retained. When a
// boundary falls in code, the part gets a synthetic newline and closing fence;
// the next part starts with the original opening fence and info string. Removing
// only those synthetic wrappers and concatenating restores the original text.
//
// Empty text returns nil. For nonempty text, limit must be at least utf8.UTFMax;
// smaller limits panic. If no fenced cut fits, or reopening and closing a fence
// would consume more than half the limit, a hard rune-boundary cut without
// synthetic wrappers guarantees progress. A complete message that fits,
// including an unclosed fence, is unchanged.
func Split(text string, limit int) []string {
	if text == "" {
		return nil
	}
	if limit < utf8.UTFMax {
		panic("markdown: split limit must be at least utf8.UTFMax")
	}
	if len(text) <= limit {
		return []string{text}
	}
	points := splitPoints(text)
	var parts []string
	start, next := 0, 0
	prefix := ""
	for start < len(text) {
		if len(prefix)+len(text)-start <= limit {
			parts = append(parts, prefix+text[start:])
			break
		}
		best := -1
		for i := next; i < len(points); i++ {
			point := points[i]
			size := len(prefix) + point.end - start
			if size > limit {
				break
			}
			if point.fence != nil {
				if len(point.fence.opener)+1+len(point.fence.marker) > limit/2 {
					continue
				}
				size += 1 + len(point.fence.marker)
			}
			if size <= limit && (best < 0 || point.priority >= points[best].priority) {
				best = i
			}
		}
		if best < 0 {
			end := start
			for end < len(text) {
				_, size := utf8.DecodeRuneInString(text[end:])
				if end-start+size > limit {
					break
				}
				end += size
			}
			parts = append(parts, text[start:end])
			start, prefix = end, ""
			for next < len(points) && points[next].end <= start {
				next++
			}
			continue
		}
		point := points[best]
		part := prefix + text[start:point.end]
		prefix = ""
		if point.fence != nil {
			part += "\n" + point.fence.marker
			prefix = point.fence.opener
		}
		parts = append(parts, part)
		start, next = point.end, best+1
	}
	return parts
}

type splitPoint struct {
	end      int
	fence    *codeFence
	priority int // rune = 0, space = 1, paragraph = 2
}

// Fence markers and opening lines are preferred intact; trailing whitespace
// after a closing marker is cuttable. Split can fall back to a hard cut when
// these candidates cannot fit. Each point carries the fence state at its offset.
func splitPoints(text string) []splitPoint {
	var points []splitPoint
	var fence *codeFence
	offset := 0
	for line := range strings.SplitAfterSeq(text, "\n") {
		end := offset + len(line)
		switch {
		case fence != nil && fence.closes(line):
			fence = nil
			body := strings.TrimLeft(line, " ")
			markerEnd := len(line) - len(body) + runLength(body)
			points = append(points, splitPoint{end: offset + markerEnd})
			for i, r := range line[markerEnd:] {
				_, size := utf8.DecodeRuneInString(line[markerEnd+i:])
				priority := 0
				if r == ' ' {
					priority = 1
				}
				points = append(points, splitPoint{end: offset + markerEnd + i + size, priority: priority})
			}
		case fence == nil && openingFence(line) != nil:
			fence = openingFence(line)
			points = append(points, splitPoint{end: end, fence: fence})
		default:
			for i, r := range line {
				_, size := utf8.DecodeRuneInString(line[i:])
				priority := 0
				if r == ' ' {
					priority = 1
				}
				points = append(points, splitPoint{end: offset + i + size, fence: fence, priority: priority})
			}
			if fence == nil && offset > 0 && strings.HasSuffix(line, "\n") && strings.TrimSpace(line) == "" {
				points[len(points)-1].priority = 2
			}
		}
		offset = end
	}
	return points
}
