package token

import (
	"reflect"
	"testing"
)

// Pin: both consent lists must remain byte for byte unchanged.
func TestStandingScopesAreUnchanged(t *testing.T) {
	want := []string{"https://www.googleapis.com/auth/chat.messages", "https://www.googleapis.com/auth/chat.spaces.readonly", "https://www.googleapis.com/auth/chat.memberships.readonly", "https://www.googleapis.com/auth/pubsub", "openid"}
	if !reflect.DeepEqual(Scopes, want) {
		t.Fatalf("desktop standing scopes changed: %v", Scopes)
	}
	want[4] = "https://www.googleapis.com/auth/directory.readonly"
	if !reflect.DeepEqual(KeyholderScopes, want) {
		t.Fatalf("managed standing scopes changed: %v", KeyholderScopes)
	}
}
