package main

import (
	"fmt"
	"testing"
	"time"
)

func TestAckRegistryBoundExpiryAndActiveProtection(t *testing.T) {
	r := newAckReceiptRegistry()
	now := time.Now()
	active := ackReceiptKey{"C1", "active"}
	if !r.claim(active, now) {
		t.Fatal("first claim refused")
	}
	for i := 0; i < ackReceiptMaxEntries; i++ {
		key := ackReceiptKey{"C1", fmt.Sprint(i)}
		if !r.claim(key, now) {
			t.Fatal("distinct claim refused")
		}
		r.finish(key, now.Add(time.Duration(i)*time.Nanosecond))
	}
	if len(r.entries) != ackReceiptMaxEntries || r.claim(active, now.Add(2*ackReceiptTTL)) {
		t.Fatal("cap exceeded or active pick expired/evicted")
	}
	// Identical timestamps in distinct conversations are distinct messages.
	if !r.claim(ackReceiptKey{"C2", "active"}, now) {
		t.Fatal("channel identity ignored")
	}
	r.finish(active, now)
	if r.claim(active, now.Add(ackReceiptTTL-time.Second)) || !r.claim(active, now.Add(ackReceiptTTL)) {
		t.Fatal("completed receipt did not respect the expiry window")
	}
}
