package fakegoogle

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"
	events "google.golang.org/api/workspaceevents/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func request(t *testing.T, s *Server, method, path string, body any, status int) map[string]any {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, s.URL()+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, status, data)
	}
	var result map[string]any
	if len(data) != 0 {
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestToken(t *testing.T) {
	s := New(t, nil)
	for _, bad := range []bool{false, true} {
		if bad {
			s.Configure(func(s *Server) { s.TokenError = "invalid_grant" })
		}
		r, err := http.PostForm(s.URL()+"/token", url.Values{"grant_type": {"refresh_token"}, "scope": {"chat.messages chat.spaces"}})
		if err != nil {
			t.Fatal(err)
		}
		var b map[string]any
		err = json.NewDecoder(r.Body).Decode(&b)
		r.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if bad {
			if r.StatusCode != 400 || b["error"] != "invalid_grant" {
				t.Fatalf("error response: %v", b)
			}
		} else if r.StatusCode != 200 || b["access_token"] != "fake" || b["expires_in"] != float64(3600) || b["scope"] != "chat.messages chat.spaces" || b["token_type"] != "Bearer" {
			t.Fatalf("token: %v", b)
		}
	}
}

func TestSubscriptionLifecycleAndDiscovery(t *testing.T) {
	s := New(t, nil)
	s.OperationDelay = time.Minute
	api, err := events.NewService(context.Background(), option.WithEndpoint(s.URL()+"/"), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	create := &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/-", EventTypes: []string{"google.workspace.chat.message.v1.created"}, Ttl: "14400s"}
	op, err := api.Subscriptions.Create(create).Do()
	if err != nil || op.Done || op.Name == "" {
		t.Fatalf("create: %+v, %v", op, err)
	}
	pending, err := api.Operations.Get(op.Name).Do()
	if err != nil || pending.Done {
		t.Fatalf("pending: %+v, %v", pending, err)
	}
	s.Advance(time.Minute)
	done, err := api.Operations.Get(op.Name).Do()
	if err != nil || !done.Done || done.Error != nil {
		t.Fatalf("done: %+v, %v", done, err)
	}
	var sub events.Subscription
	if err := json.Unmarshal(done.Response, &sub); err != nil {
		t.Fatal(err)
	}
	if sub.State != "ACTIVE" || sub.ExpireTime == "" || sub.TargetResource != create.TargetResource {
		t.Fatalf("subscription: %+v", sub)
	}
	got, err := api.Subscriptions.Get(sub.Name).Do()
	if err != nil || got.Name != sub.Name {
		t.Fatalf("get: %+v, %v", got, err)
	}
	s.Configure(func(s *Server) {
		s.OperationDelay = 0
		s.Subscriptions["subscriptions/other"] = &events.Subscription{Name: "subscriptions/other", TargetResource: "//chat.googleapis.com/spaces/other", EventTypes: create.EventTypes, State: "ACTIVE"}
	})
	list, err := api.Subscriptions.List().Filter(`target_resource = "//chat.googleapis.com/spaces/-" AND event_types:"google.workspace.chat.message.v1.created"`).Do()
	if err != nil || len(list.Subscriptions) != 1 || list.Subscriptions[0].Name != sub.Name {
		t.Fatalf("filter: %+v, %v", list, err)
	}
	page, err := api.Subscriptions.List().PageSize(1).Do()
	if err != nil || len(page.Subscriptions) != 1 || page.NextPageToken == "" {
		t.Fatalf("page: %+v, %v", page, err)
	}
	page2, err := api.Subscriptions.List().PageSize(1).PageToken(page.NextPageToken).Do()
	if err != nil || len(page2.Subscriptions) != 1 || page2.NextPageToken != "" || page.Subscriptions[0].Name == page2.Subscriptions[0].Name {
		t.Fatalf("page 2: %+v, %v", page2, err)
	}
	patch, err := api.Subscriptions.Patch(sub.Name, &events.Subscription{Ttl: "7200s"}).UpdateMask("ttl").Do()
	if err != nil || !patch.Done {
		t.Fatalf("patch: %+v, %v", patch, err)
	}
	got, err = api.Subscriptions.Get(sub.Name).Do()
	if err != nil || got.ExpireTime == sub.ExpireTime {
		t.Fatalf("renewed: %+v, %v", got, err)
	}
	s.SuspendSubscription(sub.Name)
	got, err = api.Subscriptions.Get(sub.Name).Do()
	if err != nil || got.State != "SUSPENDED" {
		t.Fatalf("suspend: %+v, %v", got, err)
	}
	reactivate := request(t, s, "POST", "/v1/"+sub.Name+":reactivate", map[string]any{}, 200)
	if reactivate["done"] != true {
		t.Fatalf("reactivate: %v", reactivate)
	}
	got, err = api.Subscriptions.Get(sub.Name).Do()
	if err != nil || got.State != "ACTIVE" {
		t.Fatalf("reactivated: %+v, %v", got, err)
	}
	s.Configure(func(s *Server) { s.CreateReturnsAlreadyExists = true })
	request(t, s, "POST", "/v1/subscriptions", &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/scripted"}, 409)
	s.Configure(func(s *Server) {
		s.CreateReturnsAlreadyExists = false
		s.OperationError = &events.Status{Code: 7, Message: "permission denied"}
	})
	failed := request(t, s, "POST", "/v1/subscriptions", &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/new"}, 200)
	if failed["done"] != true || failed["error"].(map[string]any)["code"] != float64(7) {
		t.Fatalf("failed op: %v", failed)
	}
	request(t, s, "GET", "/v1/subscriptions/missing", nil, 404)
	request(t, s, "GET", "/v1/operations/missing", nil, 404)
}

func TestLostSubscriptionResponseIsDiscoverable(t *testing.T) {
	s := New(t, nil)
	s.DropNextResponse = true
	_, err := http.Post(s.URL()+"/v1/subscriptions", "application/json", strings.NewReader(`{"targetResource":"//chat.googleapis.com/spaces/-"}`))
	if err == nil {
		t.Fatal("expected a dropped connection")
	}
	list := request(t, s, "GET", "/v1/subscriptions", nil, 200)
	if len(list["subscriptions"].([]any)) != 1 {
		t.Fatalf("lost creation: %v", list)
	}
}

func TestDelayedOperationRevealsFailureOnlyWhenDone(t *testing.T) {
	s := New(t, nil)
	s.OperationDelay = time.Minute
	s.OperationError = &events.Status{Code: 7, Message: "permission denied"}
	op := request(t, s, "POST", "/v1/subscriptions", &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/-"}, 200)
	if op["done"] != false || op["error"] != nil || op["response"] != nil {
		t.Fatalf("initial operation: %v", op)
	}
	pending := request(t, s, "GET", "/v1/"+op["name"].(string), nil, 200)
	if pending["done"] != false || pending["error"] != nil || pending["response"] != nil {
		t.Fatalf("pending failure was revealed: %v", pending)
	}
	s.Advance(time.Minute)
	done := request(t, s, "GET", "/v1/"+op["name"].(string), nil, 200)
	if done["done"] != true || done["error"] == nil || done["response"] != nil {
		t.Fatalf("failed operation: %v", done)
	}
	list := request(t, s, "GET", "/v1/subscriptions", nil, 200)
	if len(list["subscriptions"].([]any)) != 0 {
		t.Fatal("failed creation persisted a subscription")
	}
}

func TestPendingCreateReservesTarget(t *testing.T) {
	s := New(t, nil)
	s.OperationDelay = time.Minute
	body := &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/-"}
	request(t, s, "POST", "/v1/subscriptions", body, 200)
	request(t, s, "POST", "/v1/subscriptions", body, 409)
	s.Advance(time.Minute)
	list := request(t, s, "GET", "/v1/subscriptions", nil, 200)
	if len(list["subscriptions"].([]any)) != 1 {
		t.Fatalf("duplicate creation: %v", list)
	}
}

func TestLostRenewalAndDelayedReactivation(t *testing.T) {
	s := New(t, nil)
	op := request(t, s, "POST", "/v1/subscriptions", &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/-", Ttl: "60s"}, 200)
	sub := op["response"].(map[string]any)
	name := sub["name"].(string)
	s.Configure(func(s *Server) { s.DropNextResponse = true; s.OperationDelay = time.Minute })
	r, err := http.NewRequest("PATCH", s.URL()+"/v1/"+name, strings.NewReader(`{"ttl":"7200s"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(r)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected lost renewal response")
	}
	before := request(t, s, "GET", "/v1/"+name, nil, 200)
	if before["expireTime"] != sub["expireTime"] {
		t.Fatal("renewal applied before operation completed")
	}
	s.Advance(time.Minute)
	after := request(t, s, "GET", "/v1/"+name, nil, 200)
	if after["expireTime"] == sub["expireTime"] {
		t.Fatal("accepted renewal was lost")
	}
	s.SuspendSubscription()
	reactivate := request(t, s, "POST", "/v1/"+name+":reactivate", map[string]any{}, 200)
	if reactivate["done"] != false {
		t.Fatal("reactivation ignored operation delay")
	}
	s.Advance(time.Minute)
	done := request(t, s, "GET", "/v1/"+reactivate["name"].(string), nil, 200)
	if done["done"] != true || done["response"].(map[string]any)["state"] != "ACTIVE" {
		t.Fatalf("reactivation: %v", done)
	}
}

func TestMessagesPaginationThreadsAndRate(t *testing.T) {
	s := New(t, nil)
	s.Spaces["spaces/room"] = &chat.Space{Name: "spaces/room", SpaceType: "SPACE"}
	s.Spaces["spaces/dm"] = &chat.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
	s.Spaces["spaces/group"] = &chat.Space{Name: "spaces/group", SpaceType: "GROUP_CHAT"}
	page := request(t, s, "GET", "/v1/spaces?pageSize=2", nil, 200)
	if len(page["spaces"].([]any)) != 2 || page["nextPageToken"] == nil {
		t.Fatalf("spaces: %v", page)
	}
	page2 := request(t, s, "GET", "/v1/spaces?pageSize=2&pageToken="+page["nextPageToken"].(string), nil, 200)
	if len(page2["spaces"].([]any)) != 1 || page2["nextPageToken"] != nil {
		t.Fatalf("spaces page 2: %v", page2)
	}
	s.Configure(func(s *Server) { s.EnforceRate = true })
	one := request(t, s, "POST", "/v1/spaces/room/messages?requestId=first", map[string]any{"text": "hello"}, 200)
	retry := request(t, s, "POST", "/v1/spaces/room/messages?requestId=first", map[string]any{"text": "changed"}, 200)
	if retry["name"] != one["name"] || retry["text"] != "hello" {
		t.Fatalf("not idempotent: %v", retry)
	}
	resp, err := http.Post(s.URL()+"/v1/spaces/room/messages", "application/json", strings.NewReader(`{"text":"too soon"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("rate: %v", resp)
	}
	request(t, s, "POST", "/v1/spaces/dm/messages?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD", map[string]any{"text": "dm"}, 400)
	request(t, s, "POST", "/v1/spaces/group/messages?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD", map[string]any{"text": "group"}, 400)
	request(t, s, "POST", "/v1/spaces/dm/messages", map[string]any{"text": "dm", "thread": map[string]string{"name": "spaces/dm/threads/existing"}}, 200)
	s.Advance(time.Second)
	two := request(t, s, "POST", "/v1/spaces/room/messages?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD", map[string]any{"text": "reply", "thread": one["thread"]}, 200)
	if two["thread"].(map[string]any)["name"] != one["thread"].(map[string]any)["name"] {
		t.Fatal("reply changed thread")
	}
	got := request(t, s, "GET", "/v1/"+two["name"].(string), nil, 200)
	if got["text"] != "reply" {
		t.Fatalf("get: %v", got)
	}
	filter := `createTime > "` + one["createTime"].(string) + `" AND thread.name = "` + one["thread"].(map[string]any)["name"].(string) + `"`
	listed := request(t, s, "GET", "/v1/spaces/room/messages?filter="+url.QueryEscape(filter), nil, 200)
	if len(listed["messages"].([]any)) != 1 || listed["messages"].([]any)[0].(map[string]any)["name"] != two["name"] {
		t.Fatalf("filtered: %v", listed)
	}
	page = request(t, s, "GET", "/v1/spaces/room/messages?pageSize=1", nil, 200)
	page2 = request(t, s, "GET", "/v1/spaces/room/messages?pageSize=1&pageToken="+page["nextPageToken"].(string), nil, 200)
	if page2["messages"].([]any)[0].(map[string]any)["name"] != two["name"] {
		t.Fatalf("message pagination: %v", page2)
	}
	s.Advance(time.Second)
	fallback := request(t, s, "POST", "/v1/spaces/room/messages?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD", map[string]any{"text": "fallback", "thread": map[string]string{"name": "spaces/room/threads/gone"}}, 200)
	if fallback["thread"].(map[string]any)["name"] == "spaces/room/threads/gone" {
		t.Fatal("no fallback for vanished thread")
	}
	s.Advance(time.Second)
	s.Configure(func(s *Server) { s.DropNextResponse = true })
	_, err = http.Post(s.URL()+"/v1/spaces/room/messages?requestId=lost", "application/json", strings.NewReader(`{"text":"accepted"}`))
	if err == nil {
		t.Fatal("expected dropped message response")
	}
	recovered := request(t, s, "POST", "/v1/spaces/room/messages?requestId=lost", map[string]string{"text": "accepted"}, 200)
	if recovered["text"] != "accepted" {
		t.Fatalf("recovered: %v", recovered)
	}
	all := request(t, s, "GET", "/v1/spaces/room/messages", nil, 200)
	if len(all["messages"].([]any)) != 4 {
		t.Fatalf("retry duplicated a message: %v", all)
	}
	request(t, s, "GET", "/v1/spaces/room/messages/missing", nil, 404)
}

func TestReactionsAndMedia(t *testing.T) {
	s := New(t, nil)
	s.Spaces["spaces/room"] = &chat.Space{Name: "spaces/room", SpaceType: "SPACE"}
	msg := request(t, s, "POST", "/v1/spaces/room/messages", map[string]string{"text": "hello"}, 200)
	path := "/v1/" + msg["name"].(string) + "/reactions"
	reaction := request(t, s, "POST", path, map[string]any{"emoji": map[string]string{"unicode": "👍"}}, 200)
	list := request(t, s, "GET", path, nil, 200)
	if len(list["reactions"].([]any)) != 1 || reaction["name"] == "" {
		t.Fatalf("reactions: %v", list)
	}
	request(t, s, "DELETE", "/v1/"+reaction["name"].(string), nil, 200)
	list = request(t, s, "GET", path, nil, 200)
	if len(list["reactions"].([]any)) != 0 {
		t.Fatalf("delete: %v", list)
	}
	api, err := chat.NewService(context.Background(), option.WithEndpoint(s.URL()+"/"), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := api.Media.Upload("spaces/room", &chat.UploadAttachmentRequest{Filename: "example.txt"}).Media(strings.NewReader("file contents")).Do()
	if err != nil || attachment.AttachmentDataRef == nil {
		t.Fatalf("upload: %+v, %v", attachment, err)
	}
	media, err := api.Media.Download(attachment.AttachmentDataRef.ResourceName).Download()
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(media.Body)
	media.Body.Close()
	if err != nil || string(b) != "file contents" {
		t.Fatalf("download: %q, %v", b, err)
	}
	request(t, s, "GET", "/v1/media/missing?alt=media", nil, 404)
}

func TestSpaceEventsFiltersPaginationAndWindow(t *testing.T) {
	s := New(t, nil)
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	const created = "google.workspace.chat.message.v1.created"
	s.SpaceEvents["spaces/room"] = []*chat.SpaceEvent{
		{Name: "spaces/room/spaceEvents/old", EventTime: now.Add(-29 * 24 * time.Hour).Format(time.RFC3339Nano), EventType: created},
		{Name: "spaces/room/spaceEvents/1", EventTime: now.Add(-time.Hour).Format(time.RFC3339Nano), EventType: created},
		{Name: "spaces/room/spaceEvents/2", EventTime: now.Add(-time.Minute).Format(time.RFC3339Nano), EventType: "google.workspace.chat.message.v1.batchCreated"},
		{Name: "spaces/room/spaceEvents/3", EventTime: now.Format(time.RFC3339Nano), EventType: "google.workspace.chat.reaction.v1.created"},
	}
	filter := `eventTypes:"` + created + `" AND startTime="` + now.Add(-2*time.Hour).Format(time.RFC3339Nano) + `"`
	path := "/v1/spaces/room/spaceEvents?filter=" + url.QueryEscape(filter) + "&pageSize=1"
	first := request(t, s, "GET", path, nil, 200)
	second := request(t, s, "GET", path+"&pageToken="+first["nextPageToken"].(string), nil, 200)
	if first["spaceEvents"].([]any)[0].(map[string]any)["name"] != "spaces/room/spaceEvents/1" || second["spaceEvents"].([]any)[0].(map[string]any)["name"] != "spaces/room/spaceEvents/2" || second["nextPageToken"] != nil {
		t.Fatalf("events: %v %v", first, second)
	}
	filter = `eventTypes:"` + created + `" AND startTime="` + now.Add(-time.Hour).Format(time.RFC3339Nano) + `"`
	exclusive := request(t, s, "GET", "/v1/spaces/room/spaceEvents?filter="+url.QueryEscape(filter), nil, 200)
	if len(exclusive["spaceEvents"].([]any)) != 1 {
		t.Fatalf("startTime not exclusive: %v", exclusive)
	}
	filter = `eventTypes:"` + created + `" AND startTime="` + now.Add(-29*24*time.Hour).Format(time.RFC3339Nano) + `"`
	request(t, s, "GET", "/v1/spaces/room/spaceEvents?filter="+url.QueryEscape(filter), nil, 400)
}

func TestEmitOverStreamingPull(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ps := pstest.NewServer()
	defer ps.Close()
	conn, err := grpc.NewClient(ps.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client, err := pubsub.NewClient(ctx, "example", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	topic := "projects/example/topics/events"
	if _, err := client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	sub := "projects/example/subscriptions/events"
	if _, err := client.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	publisher := client.Publisher(topic)
	defer publisher.Stop()
	s := New(t, publisher)
	cases := []struct {
		typ   string
		value any
		key   string
	}{
		{"google.workspace.chat.message.v1.created", &chat.Message{Name: "spaces/room/messages/1"}, "message"},
		{"google.workspace.chat.reaction.v1.created", &chat.Reaction{Name: "spaces/room/messages/1/reactions/1"}, "reaction"},
		{"google.workspace.chat.membership.v1.created", &chat.Membership{Name: "spaces/room/members/1"}, "membership"},
		{"google.workspace.chat.message.v1.batchCreated", []*chat.Message{{Name: "spaces/room/messages/2"}, {Name: "spaces/room/messages/3"}}, "messages"},
	}
	for _, tc := range cases {
		s.Emit(tc.typ, tc.value, "spaces/room")
	}
	received := make(chan *pubsub.Message, len(cases))
	receiveDone := make(chan error, 1)
	go func() {
		receiveDone <- client.Subscriber(sub).Receive(ctx, func(_ context.Context, m *pubsub.Message) { m.Ack(); received <- m })
	}()
	seen := map[string]bool{}
	for range cases {
		select {
		case m := <-received:
			if m.Attributes["ce-subject"] != "//chat.googleapis.com/spaces/room" || m.Attributes["ce-id"] == "" || seen[m.Attributes["ce-id"]] {
				t.Fatalf("attributes: %v", m.Attributes)
			}
			seen[m.Attributes["ce-id"]] = true
			var body map[string]json.RawMessage
			if err := json.Unmarshal(m.Data, &body); err != nil {
				t.Fatal(err)
			}
			matched := false
			for _, tc := range cases {
				if tc.typ == m.Attributes["ce-type"] {
					matched = true
					if body[tc.key] == nil {
						t.Fatalf("payload: %s", m.Data)
					}
					if tc.key == "messages" {
						var wrappers []map[string]any
						if err := json.Unmarshal(body[tc.key], &wrappers); err != nil || len(wrappers) != 2 || wrappers[0]["message"] == nil {
							t.Fatalf("batch wrappers: %s", m.Data)
						}
					}
				}
			}
			if !matched {
				t.Fatalf("unexpected event: %v", m.Attributes)
			}
		case <-ctx.Done():
			t.Fatal("stream timed out")
		}
	}
	cancel()
	if err := <-receiveDone; err != nil && err != context.Canceled {
		t.Fatal(err)
	}
}
