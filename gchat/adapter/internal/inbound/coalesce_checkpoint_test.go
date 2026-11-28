package inbound

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func refusedCheckpointTurn(t *testing.T) (*windowFixture, string, string) {
	t.Helper()
	f := windowSetup(t, "8s")
	member := f.send(t, 1, "message.created", nil)
	f.now = f.now.Add(time.Second)
	anchor := f.send(t, 2, "message.created", nil)
	f.gc.Script = []wire.Delivery{{Status: "failed"}, {Status: "failed"}, {Status: "failed"}, {Status: "failed"}}
	f.advance(t, 8*time.Second)
	owner := f.l.Changes[anchor.Name]
	if owner.Status != "intent" || owner.Reason != reasonRetry || len(owner.Members) != 2 {
		t.Fatal("expected a refused two-member turn")
	}
	return f, member.Name, anchor.Name
}

func TestMergedCheckpointWithoutRecordedEditRaisesEndNotice(t *testing.T) {
	f, member, anchor := refusedCheckpointTurn(t)
	mediaState(f.l, member).Revision = f.l.Messages[member].Revision.Add(time.Second)
	posted := len(f.gc.Inbound())
	for i := 0; i < 2; i++ {
		if err := f.p.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	ended := f.l.Unresolved[anchor]
	if ended == nil || ended.Reason != "stored turn is malformed" {
		t.Fatal("expected the stored turn to end unresolved")
	}
	if got := endNotices(f, anchor); got != 1 {
		t.Fatalf("end notices for the turn = %d, want 1", got)
	}
	if len(f.gc.Inbound()) != posted {
		t.Fatal("posted the turn after its checkpoint advanced")
	}
}

// Install a recorded revision directly in the fake ledger. Both the history
// record and message state represent that revision; the media checkpoint is
// independent and may already represent the next one.
func recordCheckpointEdit(t *testing.T, f *windowFixture, member string, revision time.Time, body string) {
	t.Helper()
	var event wire.InboundMessage
	if err := json.Unmarshal([]byte(f.l.Changes[member].OriginalEvent()), &event); err != nil {
		t.Fatal(err)
	}
	frame, _, ok := strings.Cut(event.Text, "\n")
	if !ok {
		t.Fatal("missing member frame")
	}
	id := member + "#updated#" + revision.Format(time.RFC3339Nano)
	event.Text, event.ProviderMessageID = frame+"\nedited: "+body, id
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	f.l.Changes[id] = &ledger.ChangeState{ID: id, Space: space, Message: member,
		Kind: "message.updated", Status: "committed", Event: string(raw), Reason: "delivered"}
	f.l.Messages[member].Revision = revision
}

// Reject the first attempted write to the held turn, before the fake store
// accepts it. The pass may save an independent delivery's attempt and result.
func passWithCheckpoint(t *testing.T, f *windowFixture, member, anchor string, maxSaves int) {
	t.Helper()
	frozen := f.l.Clone()
	save := f.p.opts.Save
	defer func() { f.p.opts.Save = save }()
	saves := 0
	f.p.opts.Save = func(l *ledger.Ledger) error {
		saves++
		if !reflect.DeepEqual(l.Changes[anchor], frozen.Changes[anchor]) || !reflect.DeepEqual(l.Changes[member], frozen.Changes[member]) {
			t.Fatal("attempted to rewrite a held turn")
		}
		if saves > maxSaves {
			t.Fatalf("pass exceeded its bound of %d saves", maxSaves)
		}
		return save(l)
	}
	if err := f.p.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMergedCheckpointHoldLetsAnotherRoomPost(t *testing.T) {
	f, member, anchor := refusedCheckpointTurn(t)
	f.now = f.now.Add(time.Minute)
	other := f.send(t, 3, "message.created", func(m *api.Message) {
		m.Space.Name = "spaces/second"
		m.Name = m.Space.Name + "/messages/other"
	})
	// A later ready intent in the pass's ordered queue, outside the window.
	f.l.Changes[other.Name].HeldUntil = time.Time{}
	recordCheckpointEdit(t, f, member, f.now, "previous revision")
	mediaState(f.l, member).Revision = f.now.Add(time.Second)
	posted := len(f.gc.Inbound())
	passWithCheckpoint(t, f, member, anchor, 2) // One attempt and one outcome in the other room.
	newPosts := f.gc.Inbound()[posted:]
	if len(newPosts) != 1 || newPosts[0].ProviderMessageID != other.Name || newPosts[0].Conversation.ConversationID != other.Space.Name {
		t.Fatalf("expected only the other room's message, got %d posts", len(newPosts))
	}
}

func TestMergedCheckpointReleasesCurrentTurnOnce(t *testing.T) {
	f, member, anchor := refusedCheckpointTurn(t)
	f.now = f.now.Add(time.Minute)
	recordCheckpointEdit(t, f, member, f.now, "previous revision")
	checkpoint := f.now.Add(time.Second)
	mediaState(f.l, member).Revision = checkpoint
	posted := len(f.gc.Inbound())
	passWithCheckpoint(t, f, member, anchor, 0)
	if len(f.gc.Inbound()) != posted {
		t.Fatal("posted while waiting for the checkpoint")
	}
	f.now = checkpoint
	recordCheckpointEdit(t, f, member, checkpoint, "current revision")
	for i := 0; i < 2; i++ {
		if err := f.p.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	newPosts := f.gc.Inbound()[posted:]
	if len(newPosts) != 1 || newPosts[0].ProviderMessageID != anchor ||
		!strings.Contains(newPosts[0].Text, "current revision") || strings.Contains(newPosts[0].Text, "previous revision") {
		t.Fatalf("expected one updated turn on release, got %d posts", len(newPosts))
	}
}
