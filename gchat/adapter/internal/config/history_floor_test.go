package config

import (
	"strings"
	"testing"
)

func TestFirstStartHistorySettingBounds(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{{"", true}, {`"0s"`, true}, {`"24h"`, true}, {`"168h"`, true}, {`"-1h"`, false}, {`"169h"`, false}, {`"8760h"`, false}, {"24", false}, {`"1d"`, false}, {`""`, false}} {
		t.Run(tc.value, func(t *testing.T) {
			env, settings, dir := configFixture(t)
			body := ""
			if tc.value != "" {
				body = "first_start_history = " + tc.value
			}
			writeFixture(t, settings, body)
			_, err := Load(env, settings, dir)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "first_start_history") || !strings.Contains(err.Error(), settings) {
				t.Fatalf("invalid setting error=%v", err)
			}
			if tc.value == `"1d"` && !strings.Contains(err.Error(), "24h") {
				t.Fatalf("duration guidance missing: %v", err)
			}
		})
	}
}
