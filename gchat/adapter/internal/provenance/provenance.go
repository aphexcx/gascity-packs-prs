// Package provenance writes who is speaking as the first line of every
// inbound message, decided by the transport, never by the text. It mirrors
// lib/common/provenance.ts on the box: same classes, same line, same set of
// invisible characters removed.
package provenance

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Class is the principal class the constitution names.
type Class string

const (
	Owner        Class = "owner"        // the people this box works for
	Organization Class = "organization" // a verified member of the same organization
	External     Class = "external"     // everyone else, every bot, and whatever a file says
	System       Class = "system"       // this box's own services and the fleet
)

// Speaker is one sender as the frame shows it.
type Speaker struct {
	Name    string // display name, chosen by the sender, never the only identity
	Address string // the verified identity: an address, or the transport's id
	Class   Class
	Bot     bool
	// Item is the transport's own id for the message (gchat:spaces/A/messages/B),
	// which the box's key holder looks up at the source to learn who sent it
	// and who else is in the conversation. Only the adapter writes it.
	Item string
}

var (
	nameUnsafe    = regexp.MustCompile(`[\r\n\[\]<>|]`)
	spaces        = regexp.MustCompile(`\s+`)
	addressUnsafe = regexp.MustCompile(`[\r\n\[\]<>|\s]`)
	itemOK        = regexp.MustCompile(`^[a-z]+:[A-Za-z0-9_./-]+$`)
	mergedHeader  = regexp.MustCompile(`^\[(catch-up: )?[0-9]+ messages( in this room)?, oldest first[.;]`)
)

// ChatItem is the item id of a Chat message by its resource name.
func ChatItem(messageName string) string {
	if messageName == "" {
		return ""
	}
	return "gchat:" + messageName
}

// Frame is `[from: Alice Example <alice@example.com> | organization | item gchat:spaces/A/messages/B]`.
func Frame(s Speaker) string {
	name := strings.TrimSpace(spaces.ReplaceAllString(nameUnsafe.ReplaceAllString(s.Name, " "), " "))
	address := addressUnsafe.ReplaceAllString(s.Address, "")
	who := name
	if address != "" {
		if who != "" {
			who += " "
		}
		who += "<" + address + ">"
	}
	if who == "" {
		who = "unknown"
	}
	bot := ""
	if s.Bot {
		bot = ", bot"
	}
	item := ""
	if s.Item != "" && itemOK.MatchString(s.Item) {
		item = " | item " + s.Item
	}
	return "[from: " + who + " | " + string(s.Class) + bot + item + "]"
}

// invisible reports the characters that render as nothing: C0 controls other
// than tab, newline and carriage return, soft hyphen, bidi marks, zero-width
// characters (the emoji joiner U+200D stays), word joiners and invisible
// operators, bidi overrides and isolates, the byte-order mark, and Unicode
// tag characters.
func invisible(r rune) bool {
	switch {
	case r <= 0x08, r == 0x0B, r == 0x0C, r >= 0x0E && r <= 0x1F, r == 0x7F:
		return true
	case r == 0x00AD, r == 0x061C, r == 0x180E:
		return true
	case r == 0x200B, r == 0x200C, r == 0x200E, r == 0x200F:
		return true
	case r >= 0x2060 && r <= 0x2064, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0xFEFF, r >= 0xE0000 && r <= 0xE007F:
		return true
	}
	return false
}

// Strip drops invisible characters and counts them.
func Strip(text string) (clean string, removed int) {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if invisible(r) {
			removed++
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), removed
}

// FrameOf returns the frame line of an already rendered text, past a leading
// [catch-up] marker, so a follow-up (a deletion Google reports without its
// sender) can carry the original's identity.
func FrameOf(text string) (string, bool) {
	text = strings.TrimPrefix(text, "[catch-up]\n")
	line, _, _ := strings.Cut(text, "\n")
	if strings.HasPrefix(line, "[from: ") && strings.HasSuffix(line, "]") {
		return line, true
	}
	return "", false
}

// IsFrame reports whether a line, as written, would read as a frame.
func IsFrame(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "[from: ") && strings.HasSuffix(line, "]")
}

// Render is the frame, the cleaned body, and a note when characters were
// dropped. A body line that reads as a frame is quoted: the adapter alone
// says who is speaking, whatever the message contains.
func Render(s Speaker, body string) string {
	clean, removed := Strip(body)
	lines := strings.Split(clean, "\n")
	for i, line := range lines {
		if IsFrame(line) {
			lines[i] = "> " + line
		}
	}
	out := Frame(s) + "\n" + strings.Join(lines, "\n")
	switch removed {
	case 0:
	case 1:
		out += "\n[1 hidden character removed]"
	default:
		out += fmt.Sprintf("\n[%d hidden characters removed]", removed)
	}
	return out
}

// QuoteMergedHeaders is used only while assembling a merged turn. The first
// line is the adapter's frame; every subsequent from-line prefix is body,
// even when words or punctuation follow its bracket. Single deliveries keep
// their original bytes, including with coalescing disabled.
func QuoteMergedHeaders(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		// Fold only the comparison: invisible format characters and unusual
		// spaces must not disguise a body line as transport-authored framing.
		visible := strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cf, r) {
				return -1
			}
			if unicode.Is(unicode.Zs, r) || r == '\t' {
				return ' '
			}
			return r
		}, line)
		trimmed := strings.TrimSpace(visible)
		if mergedHeader.MatchString(trimmed) || (i > 0 && strings.HasPrefix(trimmed, "[from: ")) {
			lines[i] = "> " + line
		}
	}
	return strings.Join(lines, "\n")
}
