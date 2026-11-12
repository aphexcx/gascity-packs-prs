package neversilent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHealthFileStatesAndUnknown(t *testing.T) {
	now := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, body, want string }{
		{"missing", "", "unknown"},
		{"malformed", "{", "unknown"},
		{"unknown state", `{"state":"busy","since":"2026-11-12T07:59:00Z","checked_at":"2026-11-12T08:00:00Z"}`, "unknown"},
		{"missing since", `{"state":"no_seat","checked_at":"2026-11-12T08:00:00Z"}`, "unknown"},
		{"invalid timestamp", `{"state":"no_seat","since":"yesterday","checked_at":"now"}`, "unknown"},
		{"stale", `{"state":"session_down","since":"2026-11-12T07:00:00Z","checked_at":"2026-11-12T07:54:59Z"}`, "unknown"},
		{"ok", `{"state":"ok","since":"2026-11-12T07:59:00Z","checked_at":"2026-11-12T08:00:00Z"}`, "ok"},
		{"no seat", `{"state":"no_seat","since":"2026-11-12T07:59:00Z","checked_at":"2026-11-12T08:00:00Z"}`, "no_seat"},
		{"cap", `{"state":"seat_at_cap","since":"2026-11-12T07:59:00Z","checked_at":"2026-11-12T08:00:00Z","resets_at":"2026-11-12T09:00:00Z"}`, "seat_at_cap"},
		{"down", `{"state":"session_down","since":"2026-11-12T07:59:00Z","checked_at":"2026-11-12T08:00:00Z"}`, "session_down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "health.json")
			if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got := ReadHealth(path, now, HealthMaxAge)
			if got.State != tc.want || (tc.want == "unknown" && got.Reason == "") {
				t.Fatalf("health=%+v, want %s", got, tc.want)
			}
		})
	}
}
