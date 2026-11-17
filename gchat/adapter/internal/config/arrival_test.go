package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestArrivalEnvironmentPrecedence(t *testing.T) {
	env, _, dir := configFixture(t)
	t.Setenv("JEV_API_KEY", "inherited-canary")
	c, err := Load(env, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Acknowledgement.Key != "inherited-canary" {
		t.Fatal("missing inherited key")
	}
	body, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, env, string(body)+"JEV_API_KEY=file-canary\nACK_REACTION=jev\n")
	c, err = Load(env, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Acknowledgement.Key != "file-canary" {
		t.Fatal("file did not override process")
	}
	writeFixture(t, env, string(body)+"JEV_API_KEY=\n")
	c, err = Load(env, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Acknowledgement.Key != "" {
		t.Fatal("explicit empty file value lost")
	}
}

func TestArrivalAndJoinReachbackLoadTogether(t *testing.T) {
	env, settings, dir := configFixture(t)
	t.Setenv("ACK_REACTION", "eyes")
	writeFixture(t, settings, "join_reachback = \"10m\"\n")
	c, err := Load(env, settings, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.JoinReachback() != 10*time.Minute || c.Acknowledgement.Mode != "eyes" {
		t.Fatalf("reachback=%v acknowledgement=%q", c.JoinReachback(), c.Acknowledgement.Mode)
	}
	writeFixture(t, settings, "join_reachback = \"61m\"\n")
	if _, err := Load(env, settings, dir); err == nil || !strings.Contains(err.Error(), "join_reachback") {
		t.Fatalf("invalid reachback with acknowledgement on: %v", err)
	}
}
