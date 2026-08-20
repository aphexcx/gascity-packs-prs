package main

import (
	"fmt"
	"testing"
)

func TestDeliveredIDsRecordAndSeen(t *testing.T) {
	d := newDeliveredIDs()
	d.record("", "C1", "1.0", "2.0")
	d.record("mayor", "C1", "3.0")

	if !d.seen("", "C1", "1.0") || !d.seen("", "C1", "2.0") {
		t.Fatal("channel-audience ids not recorded")
	}
	if !d.seen("mayor", "C1", "3.0") {
		t.Fatal("handle-audience id not recorded")
	}
	// Exact-key semantics: audiences never bleed into each other.
	if d.seen("mayor", "C1", "1.0") {
		t.Fatal("channel-audience delivery must not mark the handle audience")
	}
	if d.seen("", "C1", "3.0") {
		t.Fatal("handle-audience delivery must not mark the channel audience")
	}
	if d.seen("", "C2", "1.0") {
		t.Fatal("channel C2 must not inherit C1 history")
	}
}

func TestDeliveredIDsNilSafety(t *testing.T) {
	var d *deliveredIDs
	d.record("", "C1", "1.0") // must not panic
	if d.seen("", "C1", "1.0") {
		t.Fatal("nil tracker must report never-seen")
	}
}

func TestDeliveredIDsEmptyInputsIgnored(t *testing.T) {
	d := newDeliveredIDs()
	d.record("", "", "1.0")
	d.record("", "C1", "")
	if d.seen("", "C1", "") || d.seen("", "", "1.0") {
		t.Fatal("empty channel/ts must never register")
	}
}

func TestDeliveredIDsPerKeyCapEvictsOldest(t *testing.T) {
	d := newDeliveredIDs()
	for i := 0; i < deliveredIDsPerKeyCap+10; i++ {
		d.record("", "C1", fmt.Sprintf("%d.0", i))
	}
	if d.seen("", "C1", "0.0") {
		t.Fatal("oldest ts must be evicted past the cap")
	}
	if !d.seen("", "C1", fmt.Sprintf("%d.0", deliveredIDsPerKeyCap+9)) {
		t.Fatal("newest ts must survive")
	}
}

func TestDeliveredIDsKeyCapEvictsOldestAudience(t *testing.T) {
	d := newDeliveredIDs()
	for i := 0; i < deliveredIDsMaxKeys+1; i++ {
		d.record("", fmt.Sprintf("C%d", i), "1.0")
	}
	if d.seen("", "C0", "1.0") {
		t.Fatal("oldest audience must be evicted past the key cap")
	}
	if !d.seen("", fmt.Sprintf("C%d", deliveredIDsMaxKeys), "1.0") {
		t.Fatal("newest audience must survive")
	}
}

func TestDeliveredIDsDuplicateRecordIsIdempotent(t *testing.T) {
	d := newDeliveredIDs()
	for i := 0; i < deliveredIDsPerKeyCap; i++ {
		d.record("", "C1", "1.0") // same ts repeatedly
	}
	d.record("", "C1", "2.0")
	if !d.seen("", "C1", "1.0") || !d.seen("", "C1", "2.0") {
		t.Fatal("duplicate records must not consume cap slots")
	}
}
