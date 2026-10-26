package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"gchat-adapter/internal/ledger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type rejectingSubscriber struct {
	pubsubpb.UnimplementedSubscriberServer
	path       string
	calls      atomic.Int32
	identities chan string
}

func (s *rejectingSubscriber) GetSubscription(_ context.Context, r *pubsubpb.GetSubscriptionRequest) (*pubsubpb.Subscription, error) {
	return &pubsubpb.Subscription{Name: r.Subscription, Topic: "projects/example/topics/chat", AckDeadlineSeconds: 60}, nil
}

func (s *rejectingSubscriber) StreamingPull(stream pubsubpb.Subscriber_StreamingPullServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	b, _ := os.ReadFile(s.path)
	var l ledger.Ledger
	_ = json.Unmarshal(b, &l)
	s.identities <- l.IdentityUserID
	if s.calls.Add(1) == 1 {
		return status.Error(codes.Unauthenticated, "expired credential")
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func TestPubSubUnauthenticatedForcesRefreshAndIdentityPrecedesPull(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, "users/123456")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	stub := &rejectingSubscriber{path: f.cfg.Endpoints.StateFile, identities: make(chan string, 10)}
	pubsubpb.RegisterSubscriberServer(s, stub)
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(s.Stop)
	f.cfg.Endpoints.PubSubEndpoint = ln.Addr().String()
	f.cfg.Endpoints.OAuthTokenURL = f.cfg.Endpoints.ChatAPIBase + "/token"
	var refreshes atomic.Int32
	f.intercept = func(r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
		}
	}
	f.start(t)
	waitFor(t, 5*time.Second, func() bool { return stub.calls.Load() >= 2 })
	f.health(t, "ready", 5*time.Second)
	if refreshes.Load() != 1 {
		t.Fatalf("Pub/Sub rejection refreshes=%d", refreshes.Load())
	}
	for range 2 {
		select {
		case id := <-stub.identities:
			if id != "users/123456" {
				t.Fatalf("pull started before identity: %q", id)
			}
		default:
			t.Fatal("missing pull observation")
		}
	}
	var counters map[string]int64
	if err := json.Unmarshal(f.status(t)["counters"], &counters); err != nil {
		t.Fatal(err)
	}
	if counters["credential_refresh_restarts"] != 1 {
		t.Fatalf("no refresh restart: %v", counters)
	}
}
