package chat

import "testing"

func TestObservedIdentitySnapshotAndConflict(t *testing.T) {
	c := &Client{}
	if id, conflict := c.ObservedIdentityUserID(); id != "" || conflict {
		t.Fatalf("unseeded: %q %v", id, conflict)
	}
	if err := c.SetIdentityUserID("users/111"); err != nil {
		t.Fatal(err)
	}
	if id, conflict := c.ObservedIdentityUserID(); id != "users/111" || conflict {
		t.Fatalf("seeded: %q %v", id, conflict)
	}
	c.learnIdentityUserID("users/222")
	for range 2 {
		if id, conflict := c.ObservedIdentityUserID(); id != "users/222" || !conflict {
			t.Fatalf("observed conflict: %q %v", id, conflict)
		}
		if c.IdentityUserID() != "users/111" {
			t.Fatal("conflicted client changed canonical ownership")
		}
		if _, err := c.reactionIdentity(); err == nil {
			t.Fatal("conflict did not block reaction deletion")
		}
		c.learnIdentityUserID("users/222")
	}
	rebuilt := &Client{}
	if err := rebuilt.SetIdentityUserID("users/222"); err != nil {
		t.Fatal(err)
	}
	rebuilt.learnIdentityUserID("users/222")
	if id, conflict := rebuilt.ObservedIdentityUserID(); id != "users/222" || conflict {
		t.Fatalf("rebuilt: %q %v", id, conflict)
	}
}
