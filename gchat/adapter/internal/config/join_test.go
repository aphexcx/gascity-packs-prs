package config

import (
	"strings"
	"testing"
)

func TestJoinReachbackValidation(t *testing.T) {
	for _, value := range []string{"0s", "5m", "60m"} {
		t.Run(value, func(t *testing.T) {
			env, settings, dir := configFixture(t)
			writeFixture(t, settings, "join_reachback = \""+value+"\"\n")
			if _, err := Load(env, settings, dir); err != nil {
				t.Fatalf("valid margin refused: %v", err)
			}
		})
	}
	for _, value := range []string{"-1s", "60m1s", "nope"} {
		t.Run(value, func(t *testing.T) {
			env, settings, dir := configFixture(t)
			writeFixture(t, settings, "join_reachback = \""+value+"\"\n")
			_, err := Load(env, settings, dir)
			if err == nil || !strings.Contains(err.Error(), "join_reachback") {
				t.Fatalf("invalid margin accepted: %v", err)
			}
		})
	}
}
