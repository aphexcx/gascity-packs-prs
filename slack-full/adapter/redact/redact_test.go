package redact

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSharedFixtures(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/redaction/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct{ Name, Text, Want string }
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := Text(tc.Text); got != tc.Want {
				t.Fatalf("got %q; want %q", got, tc.Want)
			}
			if got := Text(tc.Want); got != tc.Want {
				t.Fatalf("not idempotent: %q", got)
			}
		})
	}
}

func TestRedactBeforeClip(t *testing.T) {
	text := strings.Repeat(". ", 205) + strings.Repeat("a", 100)
	if got := Text(text)[:420]; strings.Contains(got, "aaaa") {
		t.Fatal("partial secret survived clipping")
	}
}
