package markdown

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
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
		{"backslash before code closer", "`**a**\\` **b**", "`**a**\\` *b*"},
		{"multiline code", "`a\n**b**`", "`a\n**b**`"},
		{"multiline code search resets after paragraph", "a \\``\n\nb `x\n**y**` **z**", "a \\``\n\nb `x\n**y**` *z*"},
		{"multiline code search resets after fence", "a \\``\n```\nfence\n```\nb `x\n**y**` **z**", "a \\``\n```\nfence\n```\nb `x\n**y**` *z*"},
		{"multiline code block syntax", "before ``a `\n# **title**\n- item\n```\nend`` **after**\n# Next", "before ``a `\n*title*\n* item\n```\nend`` **after**\n# Next"},
		{"unclosed code before heading", "`open\n# Title", "`open\n*Title*"},
		{"code span containing backtick", "``a ` **b**`` and **c**", "``a ` **b**`` and *c*"},
		{"fenced code", "```go\n**b**\n- x\n# title\n[x](url)\n```\n**after**", "```go\n**b**\n- x\n# title\n[x](url)\n```\n*after*"},
		{"dash bullet", "- one\n  - two\n", "* one\n  * two\n"},
		{"empty dash bullet", "- one\n- \n- three", "* one\n* \n* three"},
		{"star bullet", "* one\n* **two**", "* one\n* *two*"},
		{"spaced thematic break", "- - -\n* * *", "- - -\n* * *"},
		{"two markers are not a thematic break", "- -", "* -"},
		{"link", "[text](https://example.com)", "<https://example.com|text>"},
		{"link destination surrounding spaces", "[text]( https://example.com )", "<https://example.com|text>"},
		{"link destination leading space", "[text]( https://example.com)", "<https://example.com|text>"},
		{"link destination leading tabs and title", "[text](\t \thttps://example.com \"title\")", "<https://example.com|text>"},
		{"angle link destination leading whitespace", "[text]( \t<u v> \"title\")", "<u v|text>"},
		{"link destination pipe", "[t](https://example.com/a|b)", "<https://example.com/a%7Cb|t>"},
		{"link destination angle", "[t](https://example.com/a>b)", "<https://example.com/a%3Eb|t>"},
		{"link title", "[t](url \"title\")", "<url|t>"},

		{"invalid plain link title", "[a](u xandy) **b**", "[a](u xandy) *b*"},
		{"invalid link preserves label and destination", "[**a**](u **x**) **b**", "[**a**](u **x**) *b*"},
		{"invalid angle link title", "[**a**](<u v> nope) **b**", "[**a**](<u v> nope) *b*"},
		{"empty link destination", "[**text**]() **b**", "[**text**]() *b*"},
		{"blank link destination", "[**text**]( \t ) **b**", "[**text**]( \t ) *b*"},
		{"empty angle destination", "[**text**](<> \"title\") **b**", "[**text**](<> \"title\") *b*"},
		{"newline inside destination code", "[**x**](`0\n0`) **b**", "[**x**](`0\n0`) *b*"},
		{"newline inside angle destination code", "[**x**](<`0\n0`>) **b**", "[**x**](<`0\n0`>) *b*"},
		{"newline inside title code", "[**x**](u \"`0\n0`\") **b**", "[**x**](u \"`0\n0`\") *b*"},
		{"single quoted link title", "[t](u 'title')", "<u|t>"},
		{"parenthesized link title", "[t](u (title))", "<u|t>"},
		{"escaped quote in link title", `[t](u "a\"b")`, "<u|t>"},
		{"unclosed link title", `[**t**](u "title) **b**`, `[**t**](u "title) *b*`},
		{"extra text after link title", `[**t**](u "title" extra) **b**`, `[**t**](u "title" extra) *b*`},
		{"multiple link titles", `[**t**](u "first" "second") **b**`, `[**t**](u "first" "second") *b*`},
		// Angle-delimited destinations preserve spaces, without percent-encoding.
		{"angle link destination", "[t](<u v>)", "<u v|t>"},
		{"angle link destination with title", "[t](<u v> \"title\")", "<u v|t>"},
		{"angle link destination with pipe", "[t](<a|b>)", "<a%7Cb|t>"},
		{"angle link destination trailing text", "[t](<a>b>)", "[t](<a>b>)"},
		{"angle link destination open angle", "[t](<a<b>)", "<a%3Cb|t>"},
		{"plain link destination open angle", "[t](a<b)", "<a%3Cb|t>"},
		{"unclosed angle destination", "[t](<u v)", "[t](<u v)"},
		{"unclosed angle destination title", "[t](<u \"title\")", "<%3Cu|t>"},
		{"unclosed angle destination escaped space", `[t](<u\ v "title")`, `<%3Cu\ v|t>`},
		{"angle destination escaped closer", `[t](<a\>b>)`, `<a\%3Eb|t>`},
		{"link escaped space", "[t](url\\ path \"title\")", "<url\\ path|t>"},
		{"balanced link URL", "[text](https://example.com/a_(b))", "<https://example.com/a_(b)|text>"},
		{"escaped link parentheses", `[text](a\(b\)) **after**`, `<a\(b\)|text> *after*`},
		{"escaped link backslash", `[text](a\\) **after**`, `<a\\|text> *after*`},
		{"unclosed nested destination", "[text](a(b) **after**", "[text](a(b) *after*"},
		{"format link label", "[**text**](https://example.com)", "<https://example.com|*text*>"},
		{"link label closing angle", "[a>b](u)", "[a>b](u)"},
		{"formatted link label closing angle", "[**a>b**](u)", "[**a>b**](u)"},
		{"code link label closing angle", "[`Vec<u8>`](https://example.com)", "[`Vec<u8>`](https://example.com)"},
		{"breadcrumb link label", "[Settings > Billing](url)", "[Settings > Billing](url)"},
		{"literal link preserves surrounding formatting", "**before** [**a>b**](u) **after**", "*before* [**a>b**](u) *after*"},
		{"literal brackets before link", "array[0] then [text](url)", "array[0] then <url|text>"},
		{"nested link label", "[a [nested] label](url)", "<url|a [nested] label>"},
		{"links inside label stay literal", "[outer [inner](v) **bold**](u)", "<u|outer [inner](v) *bold*>"},
		{"links inside emphasized label stay literal", "[*outer [inner](v)*](u)", "<u|_outer [inner](v)_>"},
		{"image inside label stays literal", "[![**B**](x)](u)", "<u|![**B**](x)>"},
		{"nested destination inside label stays literal", "[t [x](a*b*c)](u)", "<u|t [x](a*b*c)>"},
		{"image unchanged", "![**alt**](image.png) and **text**", "![**alt**](image.png) and *text*"},
		{"escaped image marker becomes link", `\![a](u)`, `\!<u|a>`},
		{"escaped image marker formats label", `\![**a**](u)`, `\!<u|*a*>`},
		{"escaped backslash before image", `\\![a](u)`, `\\![a](u)`},
		{"escaped backslash and image marker", `\\\![a](u)`, `\\\!<u|a>`},
		{"link with code bracket", "[`]` label](url)", "<url|`]` label>"},
		{"heading", "# Title\n## Subtitle\n###### Small\n", "*Title*\n*Subtitle*\n*Small*\n"},
		{"tabbed heading", "#\tTitle\n##\t**Subtitle** #\t", "*Title*\n*Subtitle*"},
		{"heading trailing space", "# Title  ", "*Title*"},
		{"heading closing hashes", "# Title #", "*Title*"},
		{"heading bold", "# **Bold** title", "*Bold title*"},
		{"heading empty", "# ", ""},
		{"heading only hashes", "# ###  \n", "\n"},
		{"heading underline bold", "## __Bold__ title", "*Bold title*"},
		{"heading bold italic", "# ***Bold italic*** title", "*_Bold italic_ title*"},
		{"heading code", "# `**code**` **title**", "*`**code**` title*"},
		{"heading literal hashes", "# Title#", "*Title#*"},
		{"heading escaped hash", "# Title \\#", "*Title \\#*"},
		{"nested emphasis", "**bold _italic_** and *soft*", "*bold _italic_* and _soft_"},
		{"emphasis after multiplication", "2*3 and *note*", "2*3 and _note_"},
		{"punctuation cannot close emphasis", "(2*3)*4", "(2*3)*4"},
		{"punctuation cannot open emphasis", "a*(*b", "a*(*b"},
		{"unicode punctuation cannot close emphasis", "（2*3）*4", "（2*3）*4"},
		{"unicode punctuation cannot open emphasis", "a*（*b", "a*（*b"},
		{"punctuation surrounds emphasis", "(*word*) and **bold**!", "(_word_) and *bold*!"},
		{"ascii symbols count as punctuation", "(2*3+$*4", "(2*3+$*4"},
		{"spaced marker does not reopen emphasis", "*5 * 3 is 15*", "_5 * 3 is 15_"},
		// The subset leaves an abandoned opener literal and pairs the later one.
		{"nearest emphasis opener", "*a *b* c*", "*a _b_ c*"},
		{"emphasis after bare URL", "https://x/a_b*c and *x*", "https://x/a_b*c and _x_"},
		{"code in emphasis", "**bold `*code*`**", "*bold `*code*`*"},
		{"emphasis around emphasized link label", "*see [*the*](u) now*", "_see <u|_the_> now_"},
		{"bold around bold link label", "**see [**the**](u) now**", "*see <u|*the*> now*"},
		{"emphasis skips destination markers", "*see [the](u*path) now*", "_see <u*path|the> now_"},
		{"emphasis skips malformed link markers", "*see [**a**](u *bad*) now*", "_see [**a**](u *bad*) now_"},
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

func TestToChatCodeSpanBlockBoundaries(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"fence after paragraph break", "Reply in a ``` block:\n\n```go\nfunc f() { return a**b }\n# not a heading\n```\n\n**Next** step", "Reply in a ``` block:\n\n```go\nfunc f() { return a**b }\n# not a heading\n```\n\n*Next* step"},
		{"fence containing code span", "Use `x\n```sh\necho `date`\n**not bold in code**\n- not a bullet\n```\n**Done**", "Use `x\n```sh\necho `date`\n**not bold in code**\n- not a bullet\n```\n*Done*"},
		{"paragraph break", "Run `ls\n\n**Note**: use `grep` here.", "Run `ls\n\n*Note*: use `grep` here."},
		{"fence containing closer", "before `open\n```go\nx` **inside**\n```\n**after**", "before `open\n```go\nx` **inside**\n```\n*after*"},
		{"heading interrupts span", "before `open\n# **Title** `\n**after**", "before `open\n*Title `*\n*after*"},
		{"heading cannot start multiline span", "# `open\n**after** `", "*`open*\n*after* `"},
		{"whitespace paragraph break", "before `open\r\n \t\r\n**after** `", "before `open\r\n \t\r\n*after* `"},
		{"tilde fence interrupts span", "before `open\n~~~text\n` **inside**\n~~~\n**after**", "before `open\n~~~text\n` **inside**\n~~~\n*after*"},
		{"same paragraph continuation", "first line\nbefore `open\n**inside**` **after**", "first line\nbefore `open\n**inside**` *after*"},
		{"blockquote interrupts span", "Use `x\n> **q** `\n**after**", "Use `x\n> *q* `\n*after*"},
		{"blockquote cannot start multiline span", "> `open\n**after** `", "> `open\n*after* `"},
		{"consecutive blockquote code span", "> `open\n> **after** `", "> `open\n> **after** `"},
		{"quote marker without optional space", ">`open\n>**inside** ` **after**", ">`open\n>**inside** ` *after*"},
		{"quote paragraph without optional space", ">`open\n>\n>**after** `", ">`open\n>\n>*after* `"},
		{"quote with three spaces indent", "   >`open\n >**inside** ` **after**", "   >`open\n >**inside** ` *after*"},
		{"four spaces are not a quote prefix", "   >`open\n    >**after** `", "   >`open\n    >*after* `"},

		{"blockquote paragraph break", "> Run `ls\n>\n> **Important**: use `grep` here.", "> Run `ls\n>\n> *Important*: use `grep` here."},
		{"blockquote list interrupts span", "> a `b\n> - item\n> **c** `", "> a `b\n> - item\n> *c* `"},
		{"blockquote heading interrupts span", "> a `b\n> # Head\n> **c** `", "> a `b\n> # Head\n> *c* `"},
		{"blockquote thematic break interrupts span", "> a `b\n> ---\n> **c** `", "> a `b\n> ---\n> *c* `"},
		{"blockquote whitespace paragraph break", "> a `b\n> \n> **c** `", "> a `b\n> \n> *c* `"},
		{"blockquote fence interrupts span", "> a `b\n> ~~~\n> **c** `", "> a `b\n> ~~~\n> *c* `"},
		{"nested blockquote code span", "> > `open\n> > **after** `", "> > `open\n> > **after** `"},
		{"deeper blockquote interrupts span", "> `open\n> > **after** `", "> `open\n> > *after* `"},
		{"shallower blockquote interrupts span", "> > `open\n> **after** `", "> > `open\n> *after* `"},
		{"nested blockquote paragraph break", " >\t > `open\n   > >\t\n> > **after** `", " >\t > `open\n   > >\t\n> > *after* `"},
		{"quoted heading cannot start multiline span", "> # `open\n> **after** `", "> # `open\n> *after* `"},
		{"indented blockquote code span", " > `open\n   > **inside** `\n**after**", " > `open\n   > **inside** `\n*after*"},
		{"code search resets between paragraphs", "before \\``\n\n`**code**` **after**", "before \\``\n\n`**code**` *after*"},
		{"spaced equals keeps code span", "a `b\n = = = \n**c** `", "a `b\n = = = \n**c** `"},
		{"tabbed equals keeps code span", "a `b\n=\t=\n**c** `", "a `b\n=\t=\n**c** `"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToChat(tt.input); got != tt.want {
				t.Fatalf("ToChat(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
	for _, boundary := range []string{"-", "--", "---", "=", "==", "===", " == \t", "***", "___", " - - - ", " * * * ", " _ _ _ ", " > **quote**", "  > **quote**", "   > **quote**"} {
		t.Run("block boundary "+boundary, func(t *testing.T) {
			input := "a `b\n" + boundary + "\n**c** `"
			renderedBoundary := strings.ReplaceAll(boundary, "**quote**", "*quote*")
			want := "a `b\n" + renderedBoundary + "\n*c* `"
			if got := ToChat(input); got != want {
				t.Fatalf("ToChat(%q) = %q, want %q", input, got, want)
			}
		})
	}
	for _, marker := range []string{"- ", "* ", "+ ", "1. ", "1) "} {
		t.Run("list "+marker, func(t *testing.T) {
			input := "before `open\n" + marker + "**item** `\n**after**"
			wantMarker := marker
			if marker == "- " {
				wantMarker = "* "
			}
			want := "before `open\n" + wantMarker + "*item* `\n*after*"
			if got := ToChat(input); got != want {
				t.Fatalf("ToChat(%q) = %q, want %q", input, got, want)
			}
		})
	}
	for _, line := range []string{"2. **inside**", "2) **inside**", "0. **inside**", "12. **inside**", "1.", "1) \t"} {
		t.Run("noninterrupting ordered marker "+line, func(t *testing.T) {
			input := "x `a\n" + line + "\n**inside**` **after**"
			want := "x `a\n" + line + "\n**inside**` *after*"
			if got := ToChat(input); got != want {
				t.Fatalf("ToChat(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestToChatUnclosedOpenersLinearTime(t *testing.T) {
	tests := []struct{ name, input, want string }{}
	for _, pattern := range []string{"*x ", "[x ", "(_x ", "**a *b ", "[x]("} {
		size := 70000
		if pattern == "[x](" {
			size = 140000 // Leave a clear margin above 500ms on the quadratic implementation.
		}
		input := strings.Repeat(pattern, (size+len(pattern)-1)/len(pattern))
		tests = append(tests, struct{ name, input, want string }{pattern, input, input})
	}
	flat := strings.Repeat("\\``", (64000+2)/3)
	multiline := strings.Repeat("\\``\n", 128000/4)
	var runs strings.Builder
	for n := 1; n <= 400; n++ {
		runs.WriteString(strings.Repeat("`", n))
		runs.WriteByte(' ')
	}
	tests = append(tests,
		struct{ name, input, want string }{"escaped backticks", flat, flat},
		struct{ name, input, want string }{
			"nested emphasis and escaped backticks",
			"***a ___b **c __d *e _f ~~g " + flat + " g~~ f_ e* d__ c** b___ a***",
			"*_a *_b *c *d _e _f ~g " + flat + " g~ f_ e_ d* c* b_* a_*",
		},
		struct{ name, input, want string }{"multiline escaped backticks", multiline, multiline},
	)
	// Only the 1 MB case exceeds the bound before indexing on fast machines;
	// the 128 KB case also checks the requested input size.
	for _, size := range []int{128000, 1000000} {
		distinct := runs.String() + strings.Repeat("x", size-runs.Len())
		tests = append(tests, struct{ name, input, want string }{
			fmt.Sprintf("nested emphasis and distinct backtick runs %d", size),
			"***a ___b **c __d *e _f ~~g " + distinct + " g~~ f_ e* d__ c** b___ a***",
			"*_a *_b *c *d _e _f ~g " + distinct + " g~ f_ e_ d* c* b_* a_*",
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			got := ToChat(tt.input)
			elapsed := time.Since(start)
			if got != tt.want {
				t.Fatal("conversion changed literal code or failed to format surrounding emphasis")
			}
			t.Logf("converted %d bytes in %s", len(tt.input), elapsed)
			if elapsed > 500*time.Millisecond {
				t.Errorf("conversion took %s; limit 500ms", elapsed)
			}
		})
	}
}

func TestToChatNestedLinksLinearTime(t *testing.T) {
	const depth = 14000 // 70,001 bytes of nested link syntax.
	input := strings.Repeat("[", depth) + "a" + strings.Repeat("](u)", depth)
	want := "<u|" + strings.Repeat("[", depth-1) + "a" + strings.Repeat("](u)", depth-1) + ">"
	start := time.Now()
	got := ToChat(input)
	elapsed := time.Since(start)
	if got != want {
		t.Error("link label should retain nested links as literal text")
	}
	t.Logf("converted %d bytes in %s", len(input), elapsed)
	if elapsed > 500*time.Millisecond {
		t.Errorf("conversion took %s; limit 500ms", elapsed)
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
		{"indented fence", "  ```go\n", "  ```", strings.Repeat("x", 50), 32},
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

func TestSplitBeforeOpeningFence(t *testing.T) {
	intro := strings.Repeat("i", 19) + "\n"
	input := intro + "```go\n" + strings.Repeat("x", 40) + "\n```"
	parts := Split(input, 30)
	checkParts(t, parts, 30)
	if parts[0] != intro {
		t.Fatalf("first part = %q; want preceding line without an empty code block", parts[0])
	}
}

func TestSplitOpeningFenceBeforeHardCut(t *testing.T) {
	const limit = 32000
	for _, intro := range []string{"intro\n", ""} {
		t.Run(fmt.Sprintf("intro %q", intro), func(t *testing.T) {
			code := "```go\n" + strings.Repeat("`", 40000)
			parts := Split(intro+code, limit)
			checkParts(t, parts, limit)
			want := []string{code[:limit], code[limit:]}
			if intro != "" {
				want = append([]string{intro}, want...)
			}
			if !reflect.DeepEqual(parts, want) {
				t.Fatalf("want hard cuts without an empty fenced part or synthetic closer; part lengths = %v", lengths(parts))
			}
			if strings.Join(parts, "") != intro+code {
				t.Fatal("hard cut added or dropped bytes")
			}
		})
	}
}

func TestSplitFencedBodyBeforeHardCut(t *testing.T) {
	input := "intro\n```go\nx\n" + strings.Repeat("`", 40000)
	parts := Split(input, 32000)
	checkParts(t, parts, 32000)
	// A short code body can also precede a closing delimiter that needs hard
	// cuts. Do not leave a synthetic closer whose reopener was dropped.
	if strings.Join(parts, "") != input {
		t.Fatal("hard cut left an unpaired synthetic fence")
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
	}{{"x", 0}, {"x", -1}, {"🙂", 3}, {"abc", 1}} {
		t.Run(tt.text, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("want a panic for a limit smaller than utf8.UTFMax")
				}
			}()
			Split(tt.text, tt.limit)
		})
	}
}

func TestSplitOversizeFences(t *testing.T) {
	tests := []struct {
		name, text string
		limit      int
	}{
		{"closing whitespace", "```\nx\n```" + strings.Repeat(" ", 32000), 32000},
		{"small limit", "```go\n日本語🙂\n```", 4},
		{"long delimiter", strings.Repeat("`", 32000) + "\nx\n" + strings.Repeat("`", 32000), 32000},
	}
	for _, n := range []int{31990, 31992, 31996, 31997, 32000} {
		opener := "```" + strings.Repeat("a", n-4) + "\n"
		tests = append(tests, struct {
			name, text string
			limit      int
		}{
			fmt.Sprintf("opener %d bytes", n), opener + strings.Repeat("x", 1024) + "\n```", 32000,
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("Split panicked on valid input: %v", p)
				}
			}()
			parts := Split(tt.text, tt.limit)
			checkParts(t, parts, tt.limit)
			// Fences too costly to wrap must round-trip without synthetic bytes.
			if strings.Join(parts, "") != tt.text {
				t.Error("hard cut changed source bytes")
			}
			// These inputs need at most one part per full limit, plus one for a
			// possible UTF-8 boundary adjustment; long info cannot amplify output.
			bound := (len(tt.text)+tt.limit-1)/tt.limit + 1
			if len(parts) > bound {
				t.Errorf("got %d parts, bound %d", len(parts), bound)
			}
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
