package inbound

import (
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
)

func TestSupersededHoldsARefreshedMemberBehindAnEditCheckpoint(t *testing.T) {
	l := ledger.New()
	l.Media = make(map[string]*ledger.MediaState)
	const message = "spaces/room/messages/member"
	revision := time.Unix(100, 0).UTC()
	ahead := revision.Add(time.Second)
	l.Messages[message] = &ledger.MessageState{Revision: revision}
	for _, tc := range []struct {
		name       string
		member     time.Time
		checkpoint time.Time
		want       bool
	}{
		{"refreshed member behind checkpoint", revision, ahead, true},
		{"refreshed member at checkpoint", revision, revision, false},
		{"ordinary change behind checkpoint", time.Time{}, ahead, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l.Media[message] = &ledger.MediaState{Revision: tc.checkpoint}
			change := &ledger.ChangeState{ID: message, Message: message, Kind: "message.created", MemberRevision: tc.member}
			if got := superseded(l, change); got != tc.want {
				t.Errorf("superseded = %t, want %t", got, tc.want)
			}
		})
	}
}
