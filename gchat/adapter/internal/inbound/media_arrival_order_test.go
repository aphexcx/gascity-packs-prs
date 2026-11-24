package inbound

import (
	"context"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ack"
	api "google.golang.org/api/chat/v1"
)

func TestArrivalObservationAndEditMediaStepOrder(t *testing.T) {
	t.Run("creation and live edit", func(t *testing.T) {
		f := setup(t)
		choice, err := ack.New(ack.Options{Mode: "eyes", Catalog: ack.Catalog})
		if err != nil {
			t.Fatal(err)
		}
		sent := make(chan ack.Input, 2)
		service := &ack.Service{Choice: choice, Dir: t.TempDir(), Send: func(_ context.Context, in ack.Input, _ ack.Row) error {
			sent <- in
			return nil
		}}
		defer service.Wait()
		f.opts.Ack = service
		f.p, err = New(f.opts)
		if err != nil {
			t.Fatal(err)
		}
		m := message()
		if f.l.Spaces[space] != nil {
			t.Fatal("expected an unknown room")
		}
		created := time.Now().UTC()
		m.CreateTime = created.Format(time.RFC3339Nano)
		m.Annotations = []*api.Annotation{{Type: "USER_MENTION", UserMention: &api.UserMentionMetadata{User: &api.User{Name: identity}}}}
		m.Attachment = []*api.Attachment{cleanupAttachment(m, "sample")}
		paths := saveCleanupMessage(t, f, m, "message.created", "create")
		if len(paths) != 1 {
			t.Fatalf("creation saved %d files", len(paths))
		}
		assertCleanupFile(t, paths[0])
		if state := f.l.Media[m.Name]; state == nil || !state.Revision.IsZero() {
			t.Fatal("expected a saved attachment without an edit checkpoint")
		}
		select {
		case in := <-sent:
			if in.Message != m.Name || in.Text != m.Text || !in.Addressed {
				t.Fatalf("unexpected creation acknowledgement: %+v", in)
			}
		case <-time.After(time.Second):
			t.Fatal("missing creation acknowledgement")
		}

		revision := created.Add(time.Second)
		edit := *m
		edit.Text = "attachment removed"
		edit.Attachment = nil
		edit.LastUpdateTime = revision.Format(time.RFC3339Nano)
		handle(t, f, event("edit", "message.updated", &edit))
		assertCleanupGone(t, paths[0])
		if state := f.l.Media[m.Name]; state == nil || !state.Revision.Equal(revision) {
			t.Fatal("missing edit checkpoint")
		}
		if turns := f.gc.Inbound(); len(turns) != 2 || !strings.Contains(turns[1].Text, edit.Text) {
			t.Fatal("missing edit turn")
		}
		service.Wait()
		select {
		case in := <-sent:
			t.Fatalf("unexpected second acknowledgement: %+v", in)
		default:
		}
	})

	t.Run("observation starts while media is held", func(t *testing.T) {
		f := setup(t)
		m := message()
		m.LastUpdateTime = time.Now().UTC().Format(time.RFC3339Nano)
		change := event("edit", "message.updated", m).Changes[0]
		observed := make(chan struct{})
		done := make(chan error, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mediaNormalizationGate <- struct{}{}
		go func() {
			done <- f.p.message(ctx, change, false, func() { close(observed) })
		}()
		select {
		case <-observed:
			select {
			case err := <-done:
				<-mediaNormalizationGate
				t.Fatalf("edit returned while media was held: %v", err)
			default:
			}
		case <-time.After(time.Second):
			t.Error("observation did not start while media was held")
		}
		<-mediaNormalizationGate
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}
