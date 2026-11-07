package main

import "testing"

func TestAckKeywordPriority(t *testing.T) {
	for _, tc := range []struct {
		text  string
		files bool
		want  string
	}{
		{"Thanks for fixing the bug!", false, "pray"},
		{"Great news, the invoice is paid", false, "tada"},
		{"Approved!", false, "white_check_mark"},
		{"The site is broken?", false, "wrench"},
		{"Why?", false, "thinking_face"},
		{"", true, "eyes"},
		{"Hello", false, "eyes"},
	} {
		if got := pickAckEmoji(tc.text, tc.files); got != tc.want {
			t.Errorf("pickAckEmoji(%q, %v) = %q, want %q", tc.text, tc.files, got, tc.want)
		}
	}
}
