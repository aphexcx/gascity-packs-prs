package puller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"gchat-adapter/internal/fakes/fakegoogle"
	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"
	events "google.golang.org/api/workspaceevents/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestDecodeSingularAndBatch(t *testing.T) {
	for _, resource := range []struct{ kind, name string }{
		{"message", "spaces/room/messages/one"},
		{"reaction", "spaces/room/messages/one/reactions/two"},
		{"membership", "spaces/room/members/three"},
	} {
		for _, action := range []string{"created", "updated", "deleted"} {
			if resource.kind == "reaction" && action == "updated" {
				continue
			}
			for _, batch := range []bool{false, true} {
				verb, count := action, 1
				var data any = map[string]any{resource.kind: map[string]any{"name": resource.name}}
				if batch {
					verb, count = "batch"+strings.ToUpper(action[:1])+action[1:], 3
					data = map[string]any{resource.kind + "s": []any{data, data, data}}
				}
				t.Run(resource.kind+"."+verb, func(t *testing.T) {
					raw, err := json.Marshal(data)
					if err != nil {
						t.Fatal(err)
					}
					msg := &pubsub.Message{ID: "transport", Data: raw, Attributes: map[string]string{
						"ce-id": "event", "ce-type": "google.workspace.chat." + resource.kind + ".v1." + verb,
						"ce-subject": "//chat.googleapis.com/" + resource.name,
					}}
					ev, err := Decode(msg)
					if err != nil {
						t.Fatal(err)
					}
					if ev.PubSubID != msg.ID || ev.CloudEventID != "event" || ev.Type != msg.Attributes["ce-type"] || ev.Subject != msg.Attributes["ce-subject"] || !reflect.DeepEqual(ev.Raw, raw) || len(ev.Changes) != count {
						t.Fatalf("event: %+v", ev)
					}
					for _, change := range ev.Changes {
						if change.Kind != resource.kind+"."+action || change.Space != "spaces/room" {
							t.Fatalf("change: %+v", change)
						}
						var name string
						switch resource.kind {
						case "message":
							if change.Message != nil {
								name = change.Message.Name
							}
						case "reaction":
							if change.Reaction != nil {
								name = change.Reaction.Name
							}
						case "membership":
							if change.Membership != nil {
								name = change.Membership.Name
							}
						}
						if name != resource.name {
							t.Fatalf("resource lost: %+v", change)
						}
					}
					msg.Data[0] = '!'
					if ev.Raw[0] == '!' {
						t.Fatal("Raw aliases the transport buffer")
					}
				})
			}
		}
	}
}

func TestDecodeLifecycle(t *testing.T) {
	for _, action := range []string{"expirationReminder", "expired", "suspended"} {
		ev, err := Decode(&pubsub.Message{Data: []byte(`{"subscription":{"name":"subscriptions/example","state":"ACTIVE"}}`), Attributes: map[string]string{
			"ce-type": "google.workspace.events.subscription.v1." + action, "ce-subject": "//workspaceevents.googleapis.com/subscriptions/example",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(ev.Changes) != 1 || ev.Changes[0].Kind != "subscription."+action || ev.Changes[0].Subscription == nil || ev.Changes[0].Subscription.Name != "subscriptions/example" || ev.Changes[0].Space != "" {
			t.Fatalf("lifecycle: %+v", ev)
		}
	}
}

func TestDecodeUnknownAndMalformed(t *testing.T) {
	for _, typ := range []string{"future", "google.workspace.chat.message.v2.created", "google.workspace.chat.message.v1.future", "google.workspace.chat.reaction.v1.updated"} {
		ev, err := Decode(&pubsub.Message{Data: []byte("opaque future payload"), Attributes: map[string]string{"ce-type": typ}})
		if err != nil || ev.Type != typ || len(ev.Changes) != 0 {
			t.Fatalf("unknown: %+v, %v", ev, err)
		}
	}
	for _, tc := range []struct{ typ, body string }{
		{"", `{}`}, {"created", `{`}, {"created", `null`}, {"created", `{}`},
		{"created", `{"message":null}`}, {"created", `{"message":{}}`},
		{"created", `{"message":{"name":"invalid"}}`}, {"created", `{"message":{"name":"spaces/room/members/person"}}`},
		{"batchCreated", `{"messages":null}`}, {"batchCreated", `{"messages":[]}`},
		{"batchCreated", `{"messages":[{"name":"spaces/room/messages/one"}]}`},
		{"batchCreated", `{"messages":[{"message":{"name":"spaces/room/messages/one"}},null]}`},
	} {
		t.Run(tc.typ+tc.body, func(t *testing.T) {
			typ := tc.typ
			if typ != "" {
				typ = "google.workspace.chat.message.v1." + typ
			}
			ev, err := Decode(&pubsub.Message{ID: "broken", Data: []byte(tc.body), Attributes: map[string]string{"ce-type": typ}})
			if err == nil {
				t.Fatal("malformed payload accepted")
			}
			if ev == nil || string(ev.Raw) != tc.body || len(ev.Changes) != 0 {
				t.Fatalf("lost raw data or exposed partial batch: %+v", ev)
			}
		})
	}
	if _, err := Decode(nil); err == nil {
		t.Fatal("nil message accepted")
	}
}

type fixture struct {
	ctx       context.Context
	server    *pstest.Server
	client    *pubsub.Client
	sub       *pubsub.Subscriber
	publisher *pubsub.Publisher
	google    *fakegoogle.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	pstest.SetMinAckDeadline(time.Second)
	t.Cleanup(pstest.ResetMinAckDeadline)
	srv := pstest.NewServer()
	t.Cleanup(func() { _ = srv.Close() })
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client, err := pubsub.NewClient(ctx, "example", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	topic, sub := "projects/example/topics/events", "projects/example/subscriptions/events"
	if _, err := client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic, AckDeadlineSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	publisher := client.Publisher(topic)
	t.Cleanup(publisher.Stop)
	f := &fixture{ctx: ctx, server: srv, client: client, sub: client.Subscriber(sub), publisher: publisher, google: fakegoogle.New(t, publisher)}
	return f
}

func (f *fixture) run(t *testing.T, h Handler, opts ...RunOptions) {
	t.Helper()
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, f.sub, h, nil, opts...) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not stop after cancellation")
		}
	})
}

func waitFor(t *testing.T, ctx context.Context, check func() bool) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatal("condition not met: ", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestRunSingularBatchAndUnknown(t *testing.T) {
	f := newFixture(t)
	got := make(chan *Event, 3)
	f.run(t, func(_ context.Context, ev *Event) (Outcome, error) { got <- ev; return Ack, nil })
	f.google.Emit("google.workspace.chat.message.v1.created", &chat.Message{Name: "spaces/room/messages/one", Text: "hello"}, "spaces/room")
	f.google.Emit("google.workspace.chat.message.v1.batchCreated", []*chat.Message{{Name: "spaces/room/messages/two"}, {Name: "spaces/room/messages/three"}, {Name: "spaces/room/messages/four"}}, "spaces/room")
	f.google.Emit("google.workspace.chat.future.v1.created", map[string]any{"future": true}, "spaces/room")
	waitFor(t, f.ctx, func() bool {
		messages := f.server.Messages()
		if len(messages) != 3 {
			return false
		}
		for _, msg := range messages {
			if msg.Acks == 0 {
				return false
			}
		}
		return true
	})
	counts := map[string]int{}
	for len(got) > 0 {
		ev := <-got
		counts[ev.Type] = len(ev.Changes)
	}
	if counts["google.workspace.chat.message.v1.created"] != 1 || counts["google.workspace.chat.message.v1.batchCreated"] != 3 {
		t.Fatalf("changes: %v", counts)
	}
	// Unknown events bypass the application handler, but Decode still exposes their type.
	if len(counts) != 2 {
		t.Fatalf("unknown event reached handler: %v", counts)
	}
}

func TestRunNackRedelivers(t *testing.T) {
	for _, result := range []struct {
		name    string
		outcome Outcome
		err     error
	}{
		{"nack", Nack, nil}, {"handler error", Ack, errors.New("temporary")}, {"invalid outcome", Outcome(99), nil},
	} {
		t.Run(result.name, func(t *testing.T) {
			f := newFixture(t)
			var calls atomic.Int32
			f.run(t, func(_ context.Context, ev *Event) (Outcome, error) {
				if calls.Add(1) == 1 {
					return result.outcome, result.err
				}
				return Ack, nil
			})
			f.google.Emit("google.workspace.chat.message.v1.created", &chat.Message{Name: "spaces/room/messages/one"}, "spaces/room")
			waitFor(t, f.ctx, func() bool { m := f.server.Messages(); return len(m) == 1 && m[0].Acks > 0 })
			m := f.server.Messages()[0]
			if calls.Load() != 2 || m.Deliveries < 2 {
				t.Fatalf("no redelivery: calls=%d, deliveries=%d", calls.Load(), m.Deliveries)
			}
		})
	}
}

func TestRunQuarantine(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			f := newFixture(t)
			mediaDir := filepath.Join(t.TempDir(), "inbound")
			var handled, recorded atomic.Int32
			f.run(t, func(_ context.Context, ev *Event) (Outcome, error) {
				handled.Add(1)
				return Quarantine, errors.New("permanent")
			}, RunOptions{MediaDir: mediaDir, RecordQuarantine: func(_ context.Context, ev *Event, path string, cause error) error {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != string(ev.Raw) || cause == nil {
					return fmt.Errorf("invalid quarantine: %s, %v", data, err)
				}
				recorded.Add(1)
				return nil
			}})
			raw := []byte(`{"message":{"name":"spaces/room/messages/one"}}`)
			if malformed {
				raw = []byte(`{"message":`)
			}
			id, err := f.publisher.Publish(f.ctx, &pubsub.Message{Data: raw, Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1.created"}}).Get(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, f.ctx, func() bool { return f.server.Message(id).Acks > 0 })
			path := filepath.Join(filepath.Dir(mediaDir), "quarantine", id+".json")
			data, err := os.ReadFile(path)
			if err != nil || string(data) != string(raw) {
				t.Fatalf("quarantine file: %s, %v", data, err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("private quarantine file: %v, %v", info, err)
			}
			if recorded.Load() != 1 || (malformed && handled.Load() != 0) {
				t.Fatalf("recorded=%d handled=%d", recorded.Load(), handled.Load())
			}
		})
	}
}

func TestRunQuarantineRecordFailureRedelivers(t *testing.T) {
	f := newFixture(t)
	var records atomic.Int32
	f.run(t, func(context.Context, *Event) (Outcome, error) { return Quarantine, nil }, RunOptions{
		MediaDir: filepath.Join(t.TempDir(), "inbound"),
		RecordQuarantine: func(context.Context, *Event, string, error) error {
			if records.Add(1) == 1 {
				return errors.New("ledger unavailable")
			}
			return nil
		},
	})
	f.google.Emit("google.workspace.events.subscription.v1.expired", &events.Subscription{Name: "subscriptions/example"}, "")
	waitFor(t, f.ctx, func() bool { m := f.server.Messages(); return len(m) == 1 && m[0].Acks > 0 })
	if records.Load() != 2 || f.server.Messages()[0].Deliveries < 2 {
		t.Fatal("quarantine record failure was acked")
	}
}
