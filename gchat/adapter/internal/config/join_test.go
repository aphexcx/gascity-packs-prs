package config

import (
	"strings"
	"testing"
	"time"
)

func TestJoinReachbackValidation(t *testing.T) {
	env, settings, dir := configFixture(t)
	c, err := Load(env, settings, dir)
	if err != nil || c.JoinReachback() != 5*time.Minute {
		t.Fatalf("default: %v %v", c, err)
	}
	for _, value := range []string{"0s", "5m", "60m"} {
		t.Run(value, func(t *testing.T) {
			env, settings, dir := configFixture(t)
			writeFixture(t, settings, "join_reachback = \""+value+"\"\n")
			c, err := Load(env, settings, dir)
			if err != nil {
				t.Fatalf("valid margin refused: %v", err)
			}
			want, _ := time.ParseDuration(value)
			if c.JoinReachback() != want {
				t.Fatalf("margin=%v want %v", c.JoinReachback(), want)
			}
		})
	}
	for _, value := range []string{"", "-1s", "60m1s", "nope"} {
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
