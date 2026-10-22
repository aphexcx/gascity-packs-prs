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
	for line := range strings.SplitAfterSeq(md, "\n") {
		if fence != nil {
			out.WriteString(line)
			if fence.closes(line) {
				fence = nil
			}
			continue
		}
		if fence = openingFence(line); fence != nil {
			out.WriteString(line)
			continue
		}
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
			out.WriteString("*" + inline(body[heading+1:]) + "*")
		case strings.HasPrefix(body, "- ") || strings.HasPrefix(body, "* "):
			out.WriteString("* " + inline(body[2:]))
		default:
			out.WriteString(inline(body))
		}
		out.WriteString(ending)
	}
	return out.String()
}

func inline(s string) string {
	var out strings.Builder
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
			if label, url, end, ok := link(s, i); ok {
				if i > 0 && s[i-1] == '!' {
					out.WriteString(s[i:end])
				} else {
					out.WriteString("<" + url + "|" + inline(label) + ">")
				}
				i = end
				continue
			}
		}
		if strings.ContainsRune("*_~", rune(s[i])) {
			n := runLength(s[i:])
			marker := s[i : i+n]
			chat := map[string]string{"**": "*", "__": "*", "*": "_", "_": "_", "~~": "~", "***": "*_", "___": "*_"}[marker]
			if chat != "" && i+n < len(s) && !spaceAt(s[i+n:]) && !(s[i] == '_' && wordBefore(s[:i])) {
				if end := emphasisEnd(s, i+n, marker); end >= 0 {
					close := chat
					if chat == "*_" {
						close = "_*"
					}
					out.WriteString(chat + inline(s[i+n:end]) + close)
					i = end + n
					continue
				}
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

func link(s string, start int) (label, url string, end int, ok bool) {
	closeLabel := -1
	depth := 1
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '`':
			i = codeSpanEnd(s, i) - 1
		case '[':
			depth++
		case ']':
			depth--
		}
		if depth == 0 {
			if i+1 >= len(s) || s[i+1] != '(' {
				return
			}
			closeLabel = i
			break
		}
	}
	if closeLabel < 0 {
		return
	}
	urlStart := closeLabel + 2
	depth = 1
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
// Empty text returns nil. For nonempty text, limit must be positive and large
// enough to hold each rune and any required fence wrappers. Split panics if
// those constraints are impossible; it never drops bytes or returns an oversize
// part. A complete message that fits, including an unclosed fence, is unchanged.
func Split(text string, limit int) []string {
	if text == "" {
		return nil
	}
	if limit <= 0 {
		panic("markdown: split limit must be positive")
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
				size += 1 + len(point.fence.marker)
			}
			if size <= limit && (best < 0 || point.priority >= points[best].priority) {
				best = i
			}
		}
		if best < 0 {
			panic("markdown: split limit cannot hold a rune and required code fences")
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

// Fence lines are atomic, so a cut never divides a marker or info string. Each
// other candidate is a rune boundary carrying the fence state at that offset.
func splitPoints(text string) []splitPoint {
	var points []splitPoint
	var fence *codeFence
	offset := 0
	for line := range strings.SplitAfterSeq(text, "\n") {
		end := offset + len(line)
		switch {
		case fence != nil && fence.closes(line):
			fence = nil
			points = append(points, splitPoint{end: end})
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
