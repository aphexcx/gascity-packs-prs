package config

import (
	"strings"
	"testing"
)

func TestCoalesceWindowSettingBounds(t *testing.T) {
	for _, row := range []struct {
		value string
		valid bool
	}{
		{"", true}, {`"0s"`, true}, {`"8s"`, true}, {`"60s"`, true},
		{`"-1s"`, false}, {`"61s"`, false}, {"8", false}, {`"8"`, false}, {`""`, false},
	} {
		t.Run(row.value, func(t *testing.T) {
			env, settings, dir := configFixture(t)
			body := ""
			if row.value != "" {
				body = "coalesce_window = " + row.value
			}
			writeFixture(t, settings, body)
			_, err := Load(env, settings, dir)
			if row.valid && err != nil {
				t.Fatal(err)
			}
			if !row.valid && (err == nil || !strings.Contains(err.Error(), "coalesce_window") || !strings.Contains(err.Error(), settings)) {
				t.Fatalf("invalid setting: %v", err)
			}
		})
	}
}
