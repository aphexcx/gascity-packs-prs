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
	kind        string
	pattern     *regexp.Regexp
	allowlist   []*regexp.Regexp
	minDistinct int
}

var rules = compile()

func compile() []rule {
	var specs []struct {
		Kind, Pattern string
		Allowlist     []string
		MinDistinct   int `json:"min_distinct"`
	}
	if err := json.Unmarshal(patterns, &specs); err != nil {
		panic(err)
	}
	parts := make([]rule, 0, len(specs))
	for _, r := range specs {
		compiled := rule{kind: r.Kind, pattern: regexp.MustCompile(r.Pattern), minDistinct: r.MinDistinct}
		for _, allowed := range r.Allowlist {
			compiled.allowlist = append(compiled.allowlist, regexp.MustCompile(allowed))
		}
		parts = append(parts, compiled)
	}
	return parts
}

// Only the generic base64 rule sets a diversity floor. Padding is not entropy;
// numeric identifiers are excluded even when they contain many different digits.
func enoughDiversity(s string, minimum int) bool {
	if minimum == 0 {
		return true
	}
	s = strings.TrimRight(s, "=")
	var seen [128]bool // The base64 pattern only matches ASCII.
	distinct, digitsOnly := 0, true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !seen[c] {
			seen[c] = true
			distinct++
		}
		if c < '0' || c > '9' {
			digitsOnly = false
		}
	}
	return !digitsOnly && distinct >= minimum
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
		for offset := 0; offset < len(s); {
			loc := rule.pattern.FindStringSubmatchIndex(s[offset:])
			if loc == nil {
				break
			}
			start, stop := offset+loc[0], offset+loc[1]
			if token >= 0 {
				start = offset + loc[2*token]
			}
			if enoughDiversity(s[start:stop], rule.minDistinct) {
				matches = append(matches, match{start, stop, priority})
			}
			offset = stop
			if rule.kind == "bearer" {
				// A token can itself be "Bearer", starting another
				// credential. Keep the separating whitespace in view.
				offset = start + len("Bearer")
			} else if token >= 0 {
				// JWTs can overlap when a later segment is another eyJ
				// header. Resume at the first separator so no candidate
				// is skipped.
				if dot := strings.IndexByte(s[start:stop], '.'); dot >= 0 {
					offset = start + dot
				}
			}
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
	// Apply each rule's exceptions only to complete matches. Any other rule,
	// or a same-rule match crossing the allowed span, invalidates the exception
	// so redacting a URL prefix cannot leave a partial permalink on the next pass.
	exempt := make([]bool, len(matches))
	for priority, rule := range rules {
		if len(rule.allowlist) == 0 {
			continue
		}
		for _, allowed := range rule.allowlist {
			b := 0
			for _, span := range allowed.FindAllStringIndex(s, -1) {
				for b < len(matches) && matches[b].end <= span[0] {
					b++
				}
				last, blocked := b, false
				for last < len(matches) && matches[last].start < span[1] {
					m := matches[last]
					if m.priority != priority || m.start < span[0] || m.end > span[1] {
						blocked = true
						break
					}
					last++
				}
				if blocked {
					continue
				}
				for i := b; i < last; i++ {
					exempt[i] = true
				}
			}
		}
	}
	kept := matches[:0]
	for i, m := range matches {
		if !exempt[i] {
			kept = append(kept, m)
		}
	}
	matches = kept
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
