package markdown

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestToChat(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"bold stars", "**bold**", "*bold*"},
		{"bold underscores", "__bold__", "*bold*"},
		{"italic stars", "*italic*", "_italic_"},
		{"italic underscores", "_italic_", "_italic_"},
		{"strikethrough", "~~gone~~", "~gone~"},
		{"inline code", "`**bold** _italic_ ~~gone~~ [x](url)`", "`**bold** _italic_ ~~gone~~ [x](url)`"},
		{"code span containing backtick", "``a ` **b**`` and **c**", "``a ` **b**`` and *c*"},
		{"fenced code", "```go\n**b**\n- x\n# title\n[x](url)\n```\n**after**", "```go\n**b**\n- x\n# title\n[x](url)\n```\n*after*"},
		{"dash bullet", "- one\n  - two\n", "* one\n  * two\n"},
		{"star bullet", "* one\n* **two**", "* one\n* *two*"},
		{"link", "[text](https://example.com)", "<https://example.com|text>"},
		{"balanced link URL", "[text](https://example.com/a_(b))", "<https://example.com/a_(b)|text>"},
		{"format link label", "[**text**](https://example.com)", "<https://example.com|*text*>"},
		{"literal brackets before link", "array[0] then [text](url)", "array[0] then <url|text>"},
		{"nested link label", "[a [nested] label](url)", "<url|a [nested] label>"},
		{"image unchanged", "![**alt**](image.png) and **text**", "![**alt**](image.png) and *text*"},
		{"link with code bracket", "[`]` label](url)", "<url|`]` label>"},
		{"heading", "# Title\n## Subtitle\n###### Small\n", "*Title*\n*Subtitle*\n*Small*\n"},
		{"nested emphasis", "**bold _italic_** and *soft*", "*bold _italic_* and _soft_"},
		{"code in emphasis", "**bold `*code*`**", "*bold `*code*`*"},
		{"plain text", "日本語 👋\n> quote\n1. item\n+ item\n---\n", "日本語 👋\n> quote\n1. item\n+ item\n---\n"},
		{"literal underscores", "snake_case_name and file__name__part", "snake_case_name and file__name__part"},
		{"escaped markers", "\\**literal\\** and \\[text](url)", "\\**literal\\** and \\[text](url)"},
		{"unmatched markers", "**open _open ~~open `open [open](url", "**open _open ~~open `open [open](url"},
		{"whitespace", "  plain  \r\n\tend\n\n", "  plain  \r\n\tend\n\n"},
		{"not headings", "#tag\n####### too many", "#tag\n####### too many"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToChat(tt.input); got != tt.want {
				t.Fatalf("ToChat(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSplitParagraphs(t *testing.T) {
	// Exactly 70,000 bytes, with each paragraph fitting in one message.
	want := []string{strings.Repeat("a", 23000) + "\n\n", strings.Repeat("b", 23000) + "\n\n", strings.Repeat("c", 23996)}
	input := strings.Join(want, "")
	parts := Split(input, 32000)
	checkParts(t, parts, 32000)
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("paragraph boundaries lost; part lengths = %v", lengths(parts))
	}
}

func TestSplitLongParagraphAtSpace(t *testing.T) {
	input := strings.Repeat("a", 25000) + " " + strings.Repeat("b", 14999)
	want := []string{input[:25001], input[25001:]}
	parts := Split(input, 32000)
	checkParts(t, parts, 32000)
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("want last space boundary, got part lengths %v", lengths(parts))
	}
}

func TestSplitPlainTextRoundTrip(t *testing.T) {
	tests := []struct {
		name, text string
		limit      int
	}{
		{"no spaces", strings.Repeat("x", 40000), 32000},
		{"Japanese and emoji", strings.Repeat("日本語🙂🚀", 4000), 32000},
		{"small multibyte limit", "日🙂本🚀語", 4},
		{"whitespace", "\n\n a  b\n\n\n\t  c\n", 5},
		{"CRLF paragraphs", "one\r\n\r\ntwo\r\n\r\nthree", 12},
		{"exact limit", "1234567890", 10},
		{"one byte parts", "abc", 1},
		{"fits", "one\n\ntwo", 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts := Split(tt.text, tt.limit)
			checkParts(t, parts, tt.limit)
			if got := strings.Join(parts, ""); got != tt.text {
				t.Fatalf("round trip = %q, want %q", got, tt.text)
			}
			if len(tt.text) <= tt.limit && len(parts) != 1 {
				t.Fatalf("fitting text split into %d parts", len(parts))
			}
		})
	}
}

func TestSplitFencedCode(t *testing.T) {
	tests := []struct {
		name, opener, marker, body string
		limit                      int
	}{
		{"40000 bytes", "```go\n", "```", strings.Repeat("x", 39990), 32000},
		{"multibyte", "```日本語\n", "```", strings.Repeat("日本語🙂", 4000), 32000},
		{"many splits", "```text title\n", "```", strings.Repeat("abc def\n\n", 30), 40},
		{"no info string", "```\n", "```", strings.Repeat("x", 50), 20},
		{"longer fence", "````go\n", "````", "a\n```\n" + strings.Repeat("x", 60), 30},
		{"tilde fence", "~~~go\n", "~~~", strings.Repeat("x", 50), 20},
		{"indented fence", "  ```go\n", "  ```", strings.Repeat("x", 50), 24},
		{"exact fit", "```go\n", "```", "1234567890", 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := tt.opener + tt.body + "\n" + tt.marker
			parts := Split(input, tt.limit)
			checkParts(t, parts, tt.limit)
			var recovered strings.Builder
			for i, part := range parts {
				if !strings.HasPrefix(part, tt.opener) || !strings.HasSuffix(part, "\n"+tt.marker) {
					t.Fatalf("part %d missing original info string or closing fence", i)
				}
				if i > 0 {
					part = strings.TrimPrefix(part, tt.opener)
				}
				if i < len(parts)-1 {
					part = strings.TrimSuffix(part, "\n"+tt.marker)
				}
				recovered.WriteString(part)
			}
			if recovered.String() != input {
				t.Fatal("fence wrappers changed original bytes")
			}
			if len(input) <= tt.limit && len(parts) != 1 {
				t.Fatal("exactly fitting fence was split")
			}
		})
	}
}

func TestSplitParagraphBeforeCode(t *testing.T) {
	input := "intro\n\n```go\n1234567890\n```\n\nend"
	want := []string{"intro\n\n", "```go\n1234567890\n```\n\n", "end"}
	if parts := Split(input, 22); !reflect.DeepEqual(parts, want) {
		t.Fatalf("Split = %q, want %q", parts, want)
	}
}

func TestSplitEmpty(t *testing.T) {
	if parts := Split("", 32000); len(parts) != 0 {
		t.Fatalf("empty input = %q", parts)
	}
}

func TestSplitImpossibleLimit(t *testing.T) {
	for _, tt := range []struct {
		text  string
		limit int
	}{{"x", 0}, {"x", -1}, {"🙂", 3}, {"```very-long-info\ncontent\n```", 10}} {
		t.Run(tt.text, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("want a panic when the limit cannot preserve a rune or fence")
				}
			}()
			Split(tt.text, tt.limit)
		})
	}
}

func checkParts(t *testing.T, parts []string, limit int) {
	t.Helper()
	if len(parts) == 0 {
		t.Fatal("nonempty input returned no parts")
	}
	for i, part := range parts {
		if len(part) == 0 || len(part) > limit {
			t.Fatalf("part %d has %d bytes, limit %d", i, len(part), limit)
		}
		if !utf8.ValidString(part) {
			t.Fatalf("part %d splits a UTF-8 rune", i)
		}
	}
}

func lengths(parts []string) []int {
	result := make([]int, len(parts))
	for i, part := range parts {
		result[i] = len(part)
	}
	return result
}
