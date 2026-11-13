package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigCarriesBothSettings(t *testing.T) {
	for _, mode := range []string{"on", "none", "off", "ON", "true", "1"} {
		t.Run(mode, func(t *testing.T) {
			env, settings, dir := configFixture(t)
			for _, key := range []string{"NEVER_SILENT", "MAYOR_HEALTH_FILE", "NEVER_SILENT_OPERATOR_CONTEXT", "NEVER_SILENT_OPERATOR_AGENT"} {
				old, present := os.LookupEnv(key)
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if present {
						_ = os.Setenv(key, old)
					} else {
						_ = os.Unsetenv(key)
					}
				})
			}
			if mode != "none" {
				body, err := os.ReadFile(env)
				if err != nil {
					t.Fatal(err)
				}
				body = append(body, []byte("NEVER_SILENT="+mode+"\nMAYOR_HEALTH_FILE=example-health.json\nNEVER_SILENT_OPERATOR_CONTEXT=example-operator\nNEVER_SILENT_OPERATOR_AGENT=example-agent\n")...)
				writeFixture(t, env, string(body))
				writeFixture(t, settings, "first_start_history = \"24h\"\n")
			}
			cfg, err := Load(env, settings, dir)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.NeverSilent.Enabled != (mode == "on") {
				t.Errorf("switch %q=%t", mode, cfg.NeverSilent.Enabled)
			}
			if mode == "none" {
				city := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(dir))))
				if cfg.FirstStartWindow() != 0 || cfg.NeverSilent.HealthFile != filepath.Join(city, ".gc", "mayor-health.json") || cfg.NeverSilent.OperatorContext != "" {
					t.Fatalf("defaults: %+v", cfg.NeverSilent)
				}
			} else if cfg.FirstStartWindow() != 24*time.Hour || cfg.NeverSilent.HealthFile != "example-health.json" || cfg.NeverSilent.OperatorContext != "example-operator" || cfg.NeverSilent.OperatorAgent != "example-agent" {
				t.Fatalf("settings lost: %+v", cfg.NeverSilent)
			}
			for _, value := range []string{"-1s", "169h"} {
				writeFixture(t, settings, "first_start_history = \""+value+"\"\n")
				if _, err := Load(env, settings, dir); err == nil || !strings.Contains(err.Error(), "first_start_history") {
					t.Errorf("%s accepted with switch %s: %v", value, mode, err)
				}
			}
		})
	}
}
