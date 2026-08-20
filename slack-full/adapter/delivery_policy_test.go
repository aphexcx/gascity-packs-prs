package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDeliveryPolicyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "delivery_policy.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	return path
}

func TestDeliveryPolicyAbsentFileMeansImmediate(t *testing.T) {
	reg, err := newDeliveryPolicyRegistry(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("absent file must not error: %v", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("Len = %d, want 0", reg.Len())
	}
	if d, ok := reg.digestInterval("C1"); ok || d != 0 {
		t.Fatalf("digestInterval on empty registry = (%v, %v), want (0, false)", d, ok)
	}
}

func TestDeliveryPolicyNilRegistryIsImmediate(t *testing.T) {
	var reg *deliveryPolicyRegistry
	if d, ok := reg.digestInterval("C1"); ok || d != 0 {
		t.Fatalf("nil registry digestInterval = (%v, %v), want (0, false)", d, ok)
	}
	if reg.Len() != 0 {
		t.Fatalf("nil registry Len = %d, want 0", reg.Len())
	}
	if snap, err := reg.Stage(); snap != nil || err != nil {
		t.Fatalf("nil registry Stage = (%v, %v), want (nil, nil)", snap, err)
	}
	reg.Commit(nil) // must not panic
}

func TestDeliveryPolicyParsesDigestAndImmediate(t *testing.T) {
	path := writeDeliveryPolicyFile(t, `{
		"channels": {
			"CDIGEST": {"mode": "digest", "interval_minutes": 10},
			"CIMMED": {"mode": "immediate"}
		}
	}`)
	reg, err := newDeliveryPolicyRegistry(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if reg.Len() != 2 {
		t.Fatalf("Len = %d, want 2", reg.Len())
	}
	d, ok := reg.digestInterval("CDIGEST")
	if !ok || d != 10*time.Minute {
		t.Fatalf("digestInterval(CDIGEST) = (%v, %v), want (10m, true)", d, ok)
	}
	if _, ok := reg.digestInterval("CIMMED"); ok {
		t.Fatal("explicit immediate channel must not report digest")
	}
	if _, ok := reg.digestInterval("CUNLISTED"); ok {
		t.Fatal("unlisted channel must not report digest")
	}
}

func TestDeliveryPolicyRejectsInvalidEntries(t *testing.T) {
	cases := map[string]string{
		"unknown mode":            `{"channels": {"C1": {"mode": "batchy"}}}`,
		"digest without interval": `{"channels": {"C1": {"mode": "digest"}}}`,
		"interval too large":      `{"channels": {"C1": {"mode": "digest", "interval_minutes": 121}}}`,
		"negative interval":       `{"channels": {"C1": {"mode": "digest", "interval_minutes": -5}}}`,
		"immediate with interval": `{"channels": {"C1": {"mode": "immediate", "interval_minutes": 5}}}`,
		"unknown field":           `{"channels": {}, "surprise": true}`,
		"empty channel key":       `{"channels": {"": {"mode": "immediate"}}}`,
		"corrupt json":            `{"channels": `,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeDeliveryPolicyFile(t, content)
			if _, err := newDeliveryPolicyRegistry(path); err == nil {
				t.Fatalf("want parse error for %s, got nil", name)
			}
		})
	}
}

// TestDeliveryPolicyStagePreservesLiveStateOnAbsentFile pins the shared
// registry contract: operators clear with `{}`, a deleted file preserves
// live state across SIGHUP.
func TestDeliveryPolicyStagePreservesLiveStateOnAbsentFile(t *testing.T) {
	path := writeDeliveryPolicyFile(t, `{"channels": {"C1": {"mode": "digest", "interval_minutes": 3}}}`)
	reg, err := newDeliveryPolicyRegistry(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	snap, err := reg.Stage()
	if err != nil {
		t.Fatalf("Stage after rm: %v", err)
	}
	if snap != nil {
		t.Fatal("Stage after rm must return nil snapshot (preserve live state)")
	}
	reg.Commit(snap)
	if d, ok := reg.digestInterval("C1"); !ok || d != 3*time.Minute {
		t.Fatalf("live state lost after nil commit: (%v, %v)", d, ok)
	}
}

func TestDeliveryPolicySIGHUPReloadSwapsPolicy(t *testing.T) {
	path := writeDeliveryPolicyFile(t, `{"channels": {"C1": {"mode": "digest", "interval_minutes": 3}}}`)
	reg, err := newDeliveryPolicyRegistry(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"channels": {}}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := reloadAllRegistries(nil, nil, nil, nil, nil, nil, nil, reg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := reg.digestInterval("C1"); ok {
		t.Fatal("reload did not clear the digest policy")
	}
}

// TestDeliveryPolicyReloadFailureAborts pins all-or-nothing: a corrupt
// policy file fails the whole reload with live state untouched.
func TestDeliveryPolicyReloadFailureAborts(t *testing.T) {
	path := writeDeliveryPolicyFile(t, `{"channels": {"C1": {"mode": "digest", "interval_minutes": 3}}}`)
	reg, err := newDeliveryPolicyRegistry(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"channels": {"C1": {"mode": "nope"}}}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	err = reloadAllRegistries(nil, nil, nil, nil, nil, nil, nil, reg)
	if err == nil || !strings.Contains(err.Error(), "delivery policy") {
		t.Fatalf("want delivery-policy reload error, got %v", err)
	}
	if d, ok := reg.digestInterval("C1"); !ok || d != 3*time.Minute {
		t.Fatalf("live state must survive failed reload: (%v, %v)", d, ok)
	}
}
