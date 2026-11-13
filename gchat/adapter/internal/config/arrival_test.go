package config

import (
	"os"
	"testing"
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
