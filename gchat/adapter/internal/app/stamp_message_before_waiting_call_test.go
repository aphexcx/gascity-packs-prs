package app

import (
	"strings"
	"testing"
)

// PR 109 round 3 (the read r2's NOTE): the dating back from the false-notice
// side. Message B is admitted before the session's eyes (s2) asks, then s2
// waits behind the held removal. With ACK_REACTION unset s2 answers B, so a
// call that waited must not be dated back past the moment it asked: an act
// dated back by more than its wait (twice it, say) misses B and never-silent
// posts an owner line the unset run does not. Round 2's table pins the other
// side only (stamp/message-arrives-while-a-call-waits). One row, round 2's
// rig and oracle.
func TestHeldAnswerParityMessageBeforeTheWaitingCall(t *testing.T) {
	row := heldRow{
		name:      "stamp/message-before-the-waiting-call",
		steps:     "add s1; remove r1 held; ack; message; add s2; release r1; settle",
		googleAck: "create:200 | delete:200 | create:200 | create:200 | create:409",
		googleOff: "create:200 | delete:200 | create:200",
	}
	off := newHeldChat(t).run(row, "")
	on := newHeldChat(t).run(row, "eyes")
	t.Logf("ACK_REACTION unset: %s", strings.Join(off, " ; "))
	t.Logf("ACK_REACTION=eyes:  %s", strings.Join(on, " ; "))
	if strings.Join(on, "\n") != strings.Join(off, "\n") {
		t.Errorf("with ACK_REACTION=eyes the row's trace differs from the same row with it unset:\n on: %s\noff: %s", strings.Join(on, " ; "), strings.Join(off, " ; "))
	}
}
