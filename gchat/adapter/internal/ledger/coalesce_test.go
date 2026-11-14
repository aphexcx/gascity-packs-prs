package ledger

import "testing"

func TestCoalescedReaderLoadsReleasedLedger(t *testing.T) {
	l, err := Load("testdata/ledger-0.5.0.json", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range l.Changes {
		if c.OriginalEvent() != c.Event || c.OriginalKind() != c.Kind || len(c.Members) != 0 || c.MemberOf != "" || !c.HeldUntil.IsZero() {
			t.Fatal("released state changed")
		}
	}
}

func TestCoalescedMembersCloneIndependently(t *testing.T) {
	l := New()
	c := &ChangeState{ID: "group", Members: []string{"first", "last"}}
	l.Changes[c.ID], l.Unresolved[c.ID] = c, c
	l.Changes["nil"], l.Unresolved["nil"] = nil, nil
	copy := l.Clone()
	copy.Changes[c.ID].Members[0] = "changed"
	copy.Unresolved[c.ID].Members[1] = "changed"
	if c.Members[0] != "first" || c.Members[1] != "last" {
		t.Fatal("snapshot shares group members")
	}
}
