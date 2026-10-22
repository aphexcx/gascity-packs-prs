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
	"sync"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
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

func newFixture(t *testing.T, dialOptions ...grpc.DialOption) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	pstest.SetMinAckDeadline(time.Second)
	t.Cleanup(pstest.ResetMinAckDeadline)
	srv := pstest.NewServer()
	t.Cleanup(func() { _ = srv.Close() })
	conn, err := grpc.NewClient(srv.Addr, append(dialOptions, grpc.WithTransportCredentials(insecure.NewCredentials()))...)
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
			mediaDir := filepath.Join(quarantineTempDir(t), "inbound")
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
		MediaDir: filepath.Join(quarantineTempDir(t), "inbound"),
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

func TestRunExtendsLeaseUntilWholeBatchHandled(t *testing.T) {
	f := newFixture(t)
	f.sub.ReceiveSettings.MinDurationPerAckExtension = 10 * time.Second
	f.sub.ReceiveSettings.MaxDurationPerAckExtension = 10 * time.Second
	started := make(chan struct{})
	release := make(chan struct{})
	f.run(t, func(ctx context.Context, ev *Event) (Outcome, error) {
		if len(ev.Changes) != 3 {
			return Nack, errors.New("batch was not expanded")
		}
		close(started)
		// Simulate the final change waiting for its terminal delivery outcome.
		select {
		case <-release:
			return Ack, nil
		case <-ctx.Done():
			return Nack, ctx.Err()
		}
	})
	f.google.Emit("google.workspace.chat.message.v1.batchCreated", []*chat.Message{
		{Name: "spaces/room/messages/one"}, {Name: "spaces/room/messages/two"}, {Name: "spaces/room/messages/three"},
	}, "spaces/room")
	select {
	case <-started:
	case <-f.ctx.Done():
		t.Fatal("handler never started")
	}
	// Require both the receipt extension and a later renewal while blocked.
	waitFor(t, f.ctx, func() bool {
		m := f.server.Messages()[0]
		extensions := 0
		for _, modack := range m.Modacks {
			if modack.AckDeadline > 0 {
				extensions++
			}
		}
		return extensions >= 2
	})
	m := f.server.Messages()[0]
	if m.Acks != 0 || m.Deliveries != 1 {
		t.Fatalf("premature ack or expired lease: %+v", m)
	}
	close(release)
	waitFor(t, f.ctx, func() bool { return f.server.Messages()[0].Acks == 1 })
}

func TestRunQuarantineWriteFailureRedelivers(t *testing.T) {
	f := newFixture(t)
	parent := filepath.Join(quarantineTempDir(t), "not-a-directory")
	if err := os.WriteFile(parent, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls, records atomic.Int32
	f.run(t, func(context.Context, *Event) (Outcome, error) {
		if calls.Add(1) == 1 {
			return Quarantine, nil
		}
		return Ack, nil
	}, RunOptions{MediaDir: filepath.Join(parent, "inbound"), RecordQuarantine: func(context.Context, *Event, string, error) error {
		records.Add(1)
		return nil
	}})
	f.google.Emit("google.workspace.chat.message.v1.created", &chat.Message{Name: "spaces/room/messages/one"}, "spaces/room")
	waitFor(t, f.ctx, func() bool { m := f.server.Messages(); return len(m) == 1 && m[0].Acks > 0 })
	if calls.Load() != 2 || records.Load() != 0 {
		t.Fatalf("calls=%d records=%d", calls.Load(), records.Load())
	}
	data, err := os.ReadFile(parent)
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing file modified: %q, %v", data, err)
	}
}

func TestRunCancellationReleasesHandler(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	f.run(t, func(ctx context.Context, _ *Event) (Outcome, error) {
		close(started)
		<-ctx.Done()
		return Nack, ctx.Err()
	})
	f.google.Emit("google.workspace.chat.message.v1.created", &chat.Message{Name: "spaces/room/messages/one"}, "spaces/room")
	select {
	case <-started:
	case <-f.ctx.Done():
		t.Fatal("handler never started")
	}
	// fixture cleanup cancels Run and checks that it exits before closing pstest.
}

func TestQuarantineRejectsSymlinkDirectory(t *testing.T) {
	parent, target := quarantineTempDir(t), quarantineTempDir(t)
	if err := os.Symlink(target, filepath.Join(parent, "quarantine")); err != nil {
		t.Fatal(err)
	}
	if _, err := quarantine(filepath.Join(parent, "inbound"), &Event{PubSubID: "one", Raw: []byte("payload")}); err == nil {
		t.Fatal("quarantine followed symlink outside its directory")
	}
	files, err := os.ReadDir(target)
	if err != nil || len(files) != 0 {
		t.Fatalf("wrote through symlink: %v, %v", files, err)
	}
}

func TestQuarantineCreatesParentsAndEscapesID(t *testing.T) {
	parent := filepath.Join(quarantineTempDir(t), "nested", "media")
	ev := &Event{PubSubID: "../outside", Raw: []byte("preserve exact bytes")}
	path, err := quarantine(filepath.Join(parent, "inbound"), ev)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(parent, "quarantine") {
		t.Fatalf("path escaped: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(ev.Raw) {
		t.Fatalf("payload: %q, %v", data, err)
	}
}

func TestQuarantineRejectsSymlinkAncestorOnFirstAndLaterWrites(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("quarantine_exists=%t", existing), func(t *testing.T) {
			parent, outside := quarantineTempDir(t), quarantineTempDir(t)
			if existing {
				if err := os.Mkdir(filepath.Join(outside, "quarantine"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			// Refuse a symlinked media ancestor, whether quarantine exists yet
			// or not. Neither case may write raw payloads into the target tree.
			if err := os.Symlink(outside, filepath.Join(parent, "media")); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				_, err := quarantine(filepath.Join(parent, "media", "inbound"), &Event{PubSubID: "one", Raw: []byte("private")})
				if err == nil {
					t.Errorf("write %d accepted symlinked media ancestor", i+1)
				}
			}
			if _, err := os.Stat(filepath.Join(outside, "quarantine", "one.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("raw payload escaped: %v", err)
			}
		})
	}
}

type logRecord struct {
	level, message string
	attrs          map[string]any
}

type recordingLogger struct {
	mu      sync.Mutex
	records []logRecord
}

func (l *recordingLogger) record(level, message string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := logRecord{level: level, message: message, attrs: map[string]any{}}
	for i := 0; i < len(args); i += 2 {
		r.attrs[args[i].(string)] = args[i+1]
	}
	l.records = append(l.records, r)
}

func (l *recordingLogger) Info(message string, args ...any)  { l.record("info", message, args...) }
func (l *recordingLogger) Error(message string, args ...any) { l.record("error", message, args...) }
func (l *recordingLogger) Warn(message string, args ...any)  { l.record("warn", message, args...) }

func (l *recordingLogger) snapshot() []logRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]logRecord(nil), l.records...)
}

type failingReceiveStream struct {
	grpc.ClientStream
	err error
}

func (s *failingReceiveStream) RecvMsg(any) error { return s.err }

// pstest v2.8.0 only consults its reactors for unary RPCs, not StreamingPull.
// Bridge its error reactor into RecvMsg; successful streams still use pstest.
func injectPullErrors(code codes.Code, failures int, attempts chan<- time.Time) grpc.DialOption {
	var calls atomic.Int32
	reactor := pstest.WithErrorInjection("StreamingPull", code, "injected failure").Reactor
	return grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		stream, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil || method != "/google.pubsub.v1.Subscriber/StreamingPull" {
			return stream, err
		}
		attempts <- time.Now()
		if int(calls.Add(1)) <= failures {
			_, _, err := reactor.React(nil)
			return &failingReceiveStream{ClientStream: stream, err: err}, nil
		}
		return stream, nil
	})
}

func TestRunReconnectsAfterQuotaFailures(t *testing.T) {
	attempts := make(chan time.Time, 10)
	f := newFixture(t, injectPullErrors(codes.ResourceExhausted, 2, attempts))
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	log := &recordingLogger{}
	done := make(chan error, 1)
	delivered := make(chan struct{}, 1)
	go func() {
		done <- Run(ctx, f.sub, func(context.Context, *Event) (Outcome, error) {
			delivered <- struct{}{}
			return Ack, nil
		}, log, RunOptions{InitialBackoff: 20 * time.Millisecond, MaxBackoff: 40 * time.Millisecond})
	}()
	var started []time.Time
	for len(started) < 3 {
		select {
		case at := <-attempts:
			started = append(started, at)
		case err := <-done:
			t.Fatalf("Run stopped before reconnecting: %v", err)
		case <-ctx.Done():
			t.Fatal("Run did not reconnect")
		}
	}
	f.google.Emit("google.workspace.chat.message.v1.created", &chat.Message{Name: "spaces/room/messages/one"}, "spaces/room")
	select {
	case <-delivered:
	case err := <-done:
		t.Fatalf("Run stopped before delivery: %v", err)
	case <-ctx.Done():
		t.Fatal("no delivery after reconnecting")
	}
	waitFor(t, ctx, func() bool { m := f.server.Messages(); return len(m) == 1 && m[0].Acks > 0 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run cancellation: %v", err)
	}
	retries := 0
	for _, record := range log.snapshot() {
		if record.message != "pubsub receive retry" {
			continue
		}
		delay, ok := record.attrs["delay"].(time.Duration)
		if !ok || delay < 0 || delay >= time.Duration(20<<retries)*time.Millisecond || record.attrs["code"] != codes.ResourceExhausted.String() {
			t.Fatalf("retry must log its delay and status code: %+v", record)
		}
		if elapsed := started[retries+1].Sub(started[retries]); elapsed < delay {
			t.Errorf("reconnected after %v, before backoff %v", elapsed, delay)
		}
		retries++
	}
	if retries != 2 || len(attempts) != 0 {
		t.Fatalf("retries=%d extra attempts=%d", retries, len(attempts))
	}
}

func TestRunReturnsPermissionDenied(t *testing.T) {
	attempts := make(chan time.Time, 10)
	f := newFixture(t, injectPullErrors(codes.PermissionDenied, 10, attempts))
	err := Run(f.ctx, f.sub, func(context.Context, *Event) (Outcome, error) { return Ack, nil }, nil)
	if status.Code(err) != codes.PermissionDenied || len(attempts) != 1 {
		t.Fatalf("Run: %v, attempts=%d", err, len(attempts))
	}
}

func TestReceiveErrorClassification(t *testing.T) {
	for _, code := range []codes.Code{codes.ResourceExhausted, codes.Unavailable, codes.Internal, codes.DeadlineExceeded, codes.Aborted, codes.Unknown} {
		if !recoverableReceiveError(status.Error(code, "retry")) {
			t.Errorf("%s must reconnect", code)
		}
	}
	if !recoverableReceiveError(errors.New("transport disconnected")) {
		t.Error("non-status transport errors must reconnect")
	}
	for _, code := range []codes.Code{codes.PermissionDenied, codes.NotFound, codes.Unauthenticated, codes.FailedPrecondition, codes.InvalidArgument} {
		if recoverableReceiveError(status.Error(code, "fatal")) {
			t.Errorf("%s must return to the application", code)
		}
	}
}

func TestReceiveBackoffGrowthCapAndReset(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		b := receiveBackoff{initial: time.Second, max: time.Minute}
		for _, bound := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60} {
			if delay := b.pause(0, false); delay < 0 || delay >= bound*time.Second {
				t.Fatalf("delay %v exceeds bound %v", delay, bound*time.Second)
			}
		}
		stretch := time.Duration(0)
		if !delivered {
			stretch = time.Minute
		}
		if delay := b.pause(stretch, delivered); delay < 0 || delay >= time.Second {
			t.Errorf("backoff did not reset: %v", delay)
		}
	}
}

func TestRunCancellationInterruptsBackoff(t *testing.T) {
	attempts := make(chan time.Time, 10)
	f := newFixture(t, injectPullErrors(codes.ResourceExhausted, 10, attempts))
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	log := &recordingLogger{}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, f.sub, func(context.Context, *Event) (Outcome, error) { return Ack, nil }, log,
			RunOptions{InitialBackoff: time.Hour, MaxBackoff: time.Hour})
	}()
	waitFor(t, f.ctx, func() bool {
		for _, record := range log.snapshot() {
			if record.message == "pubsub receive retry" {
				return true
			}
		}
		return false
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt backoff")
	}
}

// macOS exposes the test temp directory through /var, a system symlink. Resolve
// that trusted fixture base before testing rejection of configured symlinks.
func quarantineTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestQuarantineWriteStaysOnVerifiedDirectoryAfterAncestorSwap(t *testing.T) {
	parent, outside := quarantineTempDir(t), quarantineTempDir(t)
	media := filepath.Join(parent, "media")
	root, err := openDurableDirectory(filepath.Join(media, "quarantine"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	moved := filepath.Join(parent, "original-media")
	if err := os.Rename(media, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, media); err != nil {
		t.Fatal(err)
	}
	if err := writeQuarantine(root, "one.json", []byte("private")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(moved, "quarantine", "one.json"))
	if err != nil || string(data) != "private" {
		t.Fatalf("verified tree: %q, %v", data, err)
	}
	files, err := os.ReadDir(outside)
	if err != nil || len(files) != 0 {
		t.Fatalf("write escaped verified tree: %v, %v", files, err)
	}
}

func TestRunWarnsOnceForIncompleteQuarantineConfiguration(t *testing.T) {
	f := newFixture(t)
	record := func(context.Context, *Event, string, error) error { return nil }
	for _, tc := range []struct {
		name    string
		options []RunOptions
		want    []string
	}{
		{"four arguments", nil, []string{"MediaDir", "nacked", "RecordQuarantine", "not be counted"}},
		{"missing directory", []RunOptions{{RecordQuarantine: record}}, []string{"MediaDir", "nacked"}},
		{"missing recorder", []RunOptions{{MediaDir: quarantineTempDir(t)}}, []string{"RecordQuarantine", "not be counted"}},
		{"complete", []RunOptions{{MediaDir: quarantineTempDir(t), RecordQuarantine: record}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(f.ctx)
			cancel()
			log := &recordingLogger{}
			if err := Run(ctx, f.sub, func(context.Context, *Event) (Outcome, error) { return Ack, nil }, log, tc.options...); err != nil {
				t.Fatal(err)
			}
			records := log.snapshot()
			if len(tc.want) == 0 {
				if len(records) != 0 {
					t.Fatalf("unexpected warning: %+v", records)
				}
				return
			}
			if len(records) != 1 || records[0].level != "warn" {
				t.Fatalf("want one startup warning, got %+v", records)
			}
			for _, want := range tc.want {
				if !strings.Contains(fmt.Sprint(records[0]), want) {
					t.Errorf("warning does not name %q: %+v", want, records[0])
				}
			}
		})
	}
}
