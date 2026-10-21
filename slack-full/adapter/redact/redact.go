// Package redact removes secret-shaped text before inbound persistence or display.
package redact

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Patterns is also read by scripts/slack_redact.py. Keep the regex syntax in
// the Go/Python intersection; shared fixtures enforce identical output.
//
//go:embed patterns.json
var patterns []byte

type rule struct {
	kind    string
	pattern *regexp.Regexp
}

var rules = compile()

func compile() []rule {
	var specs []struct{ Kind, Pattern string }
	if err := json.Unmarshal(patterns, &specs); err != nil {
		panic(err)
	}
	parts := make([]rule, 0, len(specs))
	for _, r := range specs {
		parts = append(parts, rule{r.Kind, regexp.MustCompile(r.Pattern)})
	}
	return parts
}

// Text redacts before clipping. Merge overlapping matches, including a JWT
// inside a URL whose path also looks like base64. Keep this span algorithm in
// sync with slack_redact.py; shared fixtures cover both languages.
func Text(s string) string {
	// Every rule needs at least six consecutive ASCII token characters
	// (the shortest is "Bearer"). Skip the regexp engine for large prose
	// or UTF-8 spool records with no possible candidate. This keeps the
	// durable spool's multi-megabyte size checks inexpensive under -race.
	run := 0
	for i := 0; i < len(s) && run < 6; i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '+' || c == '/' {
			run++
		} else {
			run = 0
		}
	}
	if run < 6 {
		return s
	}
	type match struct{ start, end, priority int }
	var matches []match
	for priority, rule := range rules {
		token := rule.pattern.SubexpIndex("token")
		for _, loc := range rule.pattern.FindAllStringSubmatchIndex(s, -1) {
			start := loc[0]
			if token >= 0 {
				start = loc[2*token]
			}
			matches = append(matches, match{start, loc[1], priority})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.start != b.start {
			return a.start < b.start
		}
		if a.end != b.end {
			return a.end > b.end
		}
		return a.priority < b.priority
	})
	var out strings.Builder
	end := 0
	for i := 0; i < len(matches); {
		m := matches[i]
		i++
		for i < len(matches) && matches[i].start < m.end {
			m.end = max(m.end, matches[i].end)
			i++
		}
		out.WriteString(s[end:m.start])
		out.WriteString("[REDACTED:" + rules[m.priority].kind + ":..." + s[max(m.start, m.end-4):m.end] + "]")
		end = m.end
	}
	if end == 0 {
		return s
	}
	out.WriteString(s[end:])
	return out.String()
}
