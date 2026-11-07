package fakegoogle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"runtime"
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
	create := &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/-", EventTypes: []string{"google.workspace.chat.message.v1.created"}, NotificationEndpoint: &events.NotificationEndpoint{PubsubTopic: "projects/example/topics/events"}, Ttl: "14400s"}
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
	page, err := api.Subscriptions.List().Filter(`event_types:"google.workspace.chat.message.v1.created"`).PageSize(1).Do()
	if err != nil || len(page.Subscriptions) != 1 || page.NextPageToken == "" {
		t.Fatalf("page: %+v, %v", page, err)
	}
	page2, err := api.Subscriptions.List().Filter(`event_types:"google.workspace.chat.message.v1.created"`).PageSize(1).PageToken(page.NextPageToken).Do()
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
	request(t, s, "POST", "/v1/subscriptions", &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/scripted", EventTypes: []string{"google.workspace.chat.message.v1.created"}, NotificationEndpoint: &events.NotificationEndpoint{PubsubTopic: "projects/example/topics/events"}}, 409)
	s.Configure(func(s *Server) {
		s.CreateReturnsAlreadyExists = false
		s.OperationError = &events.Status{Code: 7, Message: "permission denied"}
	})
	failed := request(t, s, "POST", "/v1/subscriptions", &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/new", EventTypes: []string{"google.workspace.chat.message.v1.created"}, NotificationEndpoint: &events.NotificationEndpoint{PubsubTopic: "projects/example/topics/events"}}, 200)
	if failed["done"] != true || failed["error"].(map[string]any)["code"] != float64(7) {
		t.Fatalf("failed op: %v", failed)
	}
	request(t, s, "GET", "/v1/subscriptions/missing", nil, 404)
	request(t, s, "GET", "/v1/operations/missing", nil, 404)
}

func TestLostSubscriptionResponseIsDiscoverable(t *testing.T) {
	s := New(t, nil)
	s.DropNextResponse = true
	_, err := http.Post(s.URL()+"/v1/subscriptions", "application/json", strings.NewReader(`{"targetResource":"//chat.googleapis.com/spaces/-","eventTypes":["google.workspace.chat.message.v1.created"],"notificationEndpoint":{"pubsubTopic":"projects/example/topics/events"}}`))
	if err == nil {
		t.Fatal("expected a dropped connection")
	}
	list := request(t, s, "GET", "/v1/subscriptions?filter="+url.QueryEscape(`event_types:"google.workspace.chat.message.v1.created"`), nil, 200)
	if len(list["subscriptions"].([]any)) != 1 {
		t.Fatalf("lost creation: %v", list)
	}
}

func TestDelayedOperationRevealsFailureOnlyWhenDone(t *testing.T) {
	s := New(t, nil)
	s.OperationDelay = time.Minute
	s.OperationError = &events.Status{Code: 7, Message: "permission denied"}
	op := request(t, s, "POST", "/v1/subscriptions", &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/-", EventTypes: []string{"google.workspace.chat.message.v1.created"}, NotificationEndpoint: &events.NotificationEndpoint{PubsubTopic: "projects/example/topics/events"}}, 200)
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
	list := request(t, s, "GET", "/v1/subscriptions?filter="+url.QueryEscape(`event_types:"google.workspace.chat.message.v1.created"`), nil, 200)
	if len(list["subscriptions"].([]any)) != 0 {
		t.Fatal("failed creation persisted a subscription")
	}
}

func TestPendingCreateReservesTarget(t *testing.T) {
	s := New(t, nil)
	s.OperationDelay = time.Minute
	body := &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/-", EventTypes: []string{"google.workspace.chat.message.v1.created"}, NotificationEndpoint: &events.NotificationEndpoint{PubsubTopic: "projects/example/topics/events"}}
	request(t, s, "POST", "/v1/subscriptions", body, 200)
	request(t, s, "POST", "/v1/subscriptions", body, 409)
	s.Advance(time.Minute)
	list := request(t, s, "GET", "/v1/subscriptions?filter="+url.QueryEscape(`event_types:"google.workspace.chat.message.v1.created"`), nil, 200)
	if len(list["subscriptions"].([]any)) != 1 {
		t.Fatalf("duplicate creation: %v", list)
	}
}

func TestLostRenewalAndDelayedReactivation(t *testing.T) {
	s := New(t, nil)
	op := request(t, s, "POST", "/v1/subscriptions", &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/-", EventTypes: []string{"google.workspace.chat.message.v1.created"}, NotificationEndpoint: &events.NotificationEndpoint{PubsubTopic: "projects/example/topics/events"}, Ttl: "120s"}, 200)
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

func TestExpiredSubscriptionCanBeRecreated(t *testing.T) {
	s := New(t, nil)
	s.Now = func() time.Time { return time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC) }
	body := &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/-", EventTypes: []string{"google.workspace.chat.message.v1.created"}, NotificationEndpoint: &events.NotificationEndpoint{PubsubTopic: "projects/example/topics/events"}, Ttl: "60s"}
	op := request(t, s, "POST", "/v1/subscriptions", body, 200)
	name := op["response"].(map[string]any)["name"].(string)
	s.Advance(59 * time.Second)
	request(t, s, "GET", "/v1/"+name, nil, 200)
	s.Advance(time.Second)
	for _, method := range []string{"GET", "PATCH", "POST"} {
		t.Run(method, func(t *testing.T) {
			path := "/v1/" + name
			if method == "POST" {
				path += ":reactivate"
			}
			got := request(t, s, method, path, &events.Subscription{Ttl: "60s"}, 404)
			if got["error"].(map[string]any)["status"] != "NOT_FOUND" {
				t.Fatalf("expired subscription: %v", got)
			}
		})
	}
	list := request(t, s, "GET", "/v1/subscriptions?filter="+url.QueryEscape(`event_types:"google.workspace.chat.message.v1.created"`), nil, 200)
	if len(list["subscriptions"].([]any)) != 0 {
		t.Errorf("expired subscription still discoverable: %v", list)
	}
	recreated := request(t, s, "POST", "/v1/subscriptions", body, 200)
	if recreated["response"].(map[string]any)["name"] == name {
		t.Fatal("recreation reused expired subscription")
	}
}

func TestLateSubscriptionOperationCannotResurrectExpiry(t *testing.T) {
	for _, action := range []string{"renew", "reactivate"} {
		for _, pollAtExpiry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/poll=%v", action, pollAtExpiry), func(t *testing.T) {
				s := New(t, nil)
				now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
				s.Now = func() time.Time { return now }
				s.Subscriptions["subscriptions/one"] = &events.Subscription{Name: "subscriptions/one", State: "SUSPENDED", ExpireTime: now.Add(time.Minute).Format(time.RFC3339Nano)}
				s.OperationDelay = 2 * time.Minute
				method, path := "PATCH", "/v1/subscriptions/one"
				if action == "reactivate" {
					method, path = "POST", path+":reactivate"
				}
				op := request(t, s, method, path, &events.Subscription{Ttl: "7200s"}, 200)
				if pollAtExpiry {
					s.Advance(time.Minute)
					request(t, s, "GET", "/v1/subscriptions/one", nil, 404)
					s.Advance(time.Minute)
				} else {
					s.Advance(2 * time.Minute)
				}
				done := request(t, s, "GET", "/v1/"+op["name"].(string), nil, 200)
				failure, ok := done["error"].(map[string]any)
				if done["done"] != true || !ok || failure["code"] != float64(5) || done["response"] != nil {
					t.Fatalf("late operation must fail NOT_FOUND: %v", done)
				}
				request(t, s, "GET", "/v1/subscriptions/one", nil, 404)
			})
		}
	}
}

func TestLateSubscriptionOperationsMergeOnlyTheirFields(t *testing.T) {
	s := New(t, nil)
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	s.Subscriptions["subscriptions/one"] = &events.Subscription{Name: "subscriptions/one", State: "ACTIVE", ExpireTime: now.Add(time.Hour).Format(time.RFC3339Nano)}
	s.OperationDelay = time.Minute
	renew := request(t, s, "PATCH", "/v1/subscriptions/one", &events.Subscription{Ttl: "7200s"}, 200)
	s.SuspendSubscription("subscriptions/one")
	s.Configure(func(s *Server) { s.Subscriptions["subscriptions/one"].SuspensionReason = "USER_SCOPE_REVOKED" })
	s.Advance(time.Minute)
	done := request(t, s, "GET", "/v1/"+renew["name"].(string), nil, 200)["response"].(map[string]any)
	if done["state"] != "SUSPENDED" || done["suspensionReason"] != "USER_SCOPE_REVOKED" || done["expireTime"] != now.Add(2*time.Hour).Format(time.RFC3339Nano) {
		t.Errorf("renewal replaced current state: %v", done)
	}
	s.SuspendSubscription("subscriptions/one")
	reactivate := request(t, s, "POST", "/v1/subscriptions/one:reactivate", map[string]any{}, 200)
	s.Configure(func(s *Server) { s.OperationDelay = 0 })
	newer := request(t, s, "PATCH", "/v1/subscriptions/one", &events.Subscription{Ttl: "10800s"}, 200)["response"].(map[string]any)
	s.Advance(time.Minute)
	done = request(t, s, "GET", "/v1/"+reactivate["name"].(string), nil, 200)["response"].(map[string]any)
	if done["state"] != "ACTIVE" || done["suspensionReason"] != nil || done["expireTime"] != newer["expireTime"] {
		t.Errorf("reactivation replaced current expiry: %v", done)
	}
}

func TestUnpolledRenewalCompletesBeforeOriginalExpiry(t *testing.T) {
	s := New(t, nil)
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	s.Subscriptions["subscriptions/one"] = &events.Subscription{Name: "subscriptions/one", State: "ACTIVE", ExpireTime: now.Add(time.Minute).Format(time.RFC3339Nano)}
	s.OperationDelay = 30 * time.Second
	op := request(t, s, "PATCH", "/v1/subscriptions/one", &events.Subscription{Ttl: "7200s"}, 200)
	s.Advance(2 * time.Minute)
	done := request(t, s, "GET", "/v1/"+op["name"].(string), nil, 200)
	if done["done"] != true || done["error"] != nil {
		t.Fatalf("timely renewal failed because it was not polled: %v", done)
	}
	request(t, s, "GET", "/v1/subscriptions/one", nil, 200)
}

func TestSubscriptionCreateRequiresEventTypesAndTopic(t *testing.T) {
	for _, missing := range []string{"eventTypes", "notificationEndpoint", "pubsubTopic"} {
		t.Run(missing, func(t *testing.T) {
			s := New(t, nil)
			sub := &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/-", EventTypes: []string{"google.workspace.chat.message.v1.created"}, NotificationEndpoint: &events.NotificationEndpoint{PubsubTopic: "projects/example/topics/events"}}
			switch missing {
			case "eventTypes":
				sub.EventTypes = nil
			case "notificationEndpoint":
				sub.NotificationEndpoint = nil
			case "pubsubTopic":
				sub.NotificationEndpoint.PubsubTopic = ""
			}
			got := request(t, s, "POST", "/v1/subscriptions", sub, 400)
			if got["error"].(map[string]any)["status"] != "INVALID_ARGUMENT" {
				t.Fatalf("invalid create: %v", got)
			}
		})
	}
}

func TestSubscriptionOperationResponseType(t *testing.T) {
	s := New(t, nil)
	body := &events.Subscription{TargetResource: "//chat.googleapis.com/spaces/-", EventTypes: []string{"google.workspace.chat.message.v1.created"}, NotificationEndpoint: &events.NotificationEndpoint{PubsubTopic: "projects/example/topics/events"}}
	created := request(t, s, "POST", "/v1/subscriptions", body, 200)
	name := created["response"].(map[string]any)["name"].(string)
	patched := request(t, s, "PATCH", "/v1/"+name, &events.Subscription{Ttl: "7200s"}, 200)
	s.SuspendSubscription(name)
	reactivated := request(t, s, "POST", "/v1/"+name+":reactivate", map[string]any{}, 200)
	for _, op := range []map[string]any{created, patched, reactivated} {
		if op["response"].(map[string]any)["@type"] != "type.googleapis.com/google.apps.events.subscriptions.v1.Subscription" {
			t.Errorf("operation response missing Subscription type: %v", op)
		}
	}
}

type emitReporter struct {
	testing.TB
	errors []string
}

func (r *emitReporter) Error(args ...any) { r.errors = append(r.errors, fmt.Sprint(args...)) }
func (r *emitReporter) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
func (r *emitReporter) Fatal(args ...any) { r.Error(args...); runtime.Goexit() }
func (r *emitReporter) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}

func TestEmitReportsErrorsWithoutTerminatingCaller(t *testing.T) {
	for _, tc := range []struct {
		name, typ string
		publisher *pubsub.Publisher
		value     any
	}{
		{"publisher", "google.workspace.chat.message.v1.created", nil, &chat.Message{}},
		{"batch", "google.workspace.chat.message.v1.batchCreated", &pubsub.Publisher{}, &chat.Message{}},
		{"marshal", "google.workspace.chat.message.v1.created", &pubsub.Publisher{}, make(chan int)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reporter := &emitReporter{TB: t}
			s := New(reporter, tc.publisher)
			done := make(chan bool, 1)
			go func() {
				returned := false
				defer func() { done <- returned }()
				s.Emit(tc.typ, tc.value, "spaces/room")
				returned = true
			}()
			if returned := <-done; !returned || len(reporter.errors) != 1 {
				t.Fatalf("Emit returned normally = %v, reported errors = %v", returned, reporter.errors)
			}
		})
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
	filter := `create_time > "` + one["createTime"].(string) + `" AND thread.name = ` + one["thread"].(map[string]any)["name"].(string)
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
	sender := msg["sender"].(map[string]any)["name"].(string)
	owner := reaction["user"].(map[string]any)["name"].(string)
	if !regexp.MustCompile(`^users/[0-9]+$`).MatchString(sender) || owner != sender {
		t.Errorf("want the same canonical user for sender and reaction owner: %q, %q", sender, owner)
	}
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

func TestResumableUploadRoundTripAboveDefaultChunk(t *testing.T) {
	s := New(t, nil)
	s.Spaces["spaces/room"] = &chat.Space{Name: "spaces/room", SpaceType: "SPACE"}
	api, err := chat.NewService(context.Background(), option.WithEndpoint(s.URL()+"/"), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("0123456789abcdef"), (17<<20)/16)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	attachment, err := api.Media.Upload("spaces/room", &chat.UploadAttachmentRequest{Filename: "example.bin"}).Media(bytes.NewReader(data)).Context(ctx).Do()
	if err != nil {
		t.Fatalf("resumable upload: %v", err)
	}
	if attachment.AttachmentDataRef == nil {
		t.Fatal("upload omitted attachment data reference")
	}
	media, err := api.Media.Download(attachment.AttachmentDataRef.ResourceName).Download()
	if err != nil {
		t.Fatal(err)
	}
	defer media.Body.Close()
	got, err := io.ReadAll(media.Body)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("download = %d bytes, want %d identical bytes: %v", len(got), len(data), err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestSimpleUploadRejectsOversizeMedia(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		t.Run(fmt.Sprintf("multipart=%v", multipart), func(t *testing.T) {
			s := New(t, nil)
			s.Spaces["spaces/room"] = &chat.Space{Name: "spaces/room"}
			s.MaxUploadBytes = 1024
			var body io.Reader = io.LimitReader(zeroReader{}, s.MaxUploadBytes+1)
			contentType := "application/octet-stream"
			if multipart {
				contentType = "multipart/related; boundary=upload-boundary"
				body = io.MultiReader(
					strings.NewReader("--upload-boundary\r\nContent-Type: application/json\r\n\r\n{}\r\n--upload-boundary\r\nContent-Type: application/octet-stream\r\n\r\n"),
					body, strings.NewReader("\r\n--upload-boundary--\r\n"))
			}
			r := httptest.NewRequest(http.MethodPost, "/upload/v1/spaces/room/attachments:upload", body)
			r.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			s.upload(w, r)
			if w.Code != http.StatusBadRequest || len(s.Media) != 0 {
				t.Fatalf("oversize upload: status=%d, stored=%d", w.Code, len(s.Media))
			}
		})
	}
}

func TestResumableUploadPUTChunks(t *testing.T) {
	s := New(t, nil)
	s.Spaces["spaces/room"] = &chat.Space{Name: "spaces/room", SpaceType: "SPACE"}
	init, err := http.Post(s.URL()+"/upload/v1/spaces/room/attachments:upload?uploadType=resumable", "application/json", strings.NewReader(`{"filename":"example.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	init.Body.Close()
	location := init.Header.Get("Location")
	if init.StatusCode != 200 || location == "" {
		t.Fatalf("resumable initialization: status %d, Location %q", init.StatusCode, location)
	}
	put := func(contentRange, data string, status int) *http.Response {
		t.Helper()
		req, err := http.NewRequest("PUT", location, strings.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Range", contentRange)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != status {
			t.Fatalf("chunk %s: status %d, want %d", contentRange, resp.StatusCode, status)
		}
		return resp
	}
	put("bytes 3-5/6", "def", 400) // gaps cannot silently corrupt the file
	first := put("bytes 0-2/*", "abc", 308)
	if first.Header.Get("Range") != "bytes=0-2" {
		t.Fatalf("chunk range: %v", first.Header)
	}
	put("bytes 0-2/*", "abc", 308) // retry does not append twice
	put("bytes 0-1/2", "ab", 400)  // total cannot shrink below bytes already accepted
	put("bytes 3-5/6", "de", 400)  // mismatched byte count
	final := put("bytes 3-5/6", "def", 200)
	var attachment chat.UploadAttachmentResponse
	if err := json.NewDecoder(final.Body).Decode(&attachment); err != nil {
		t.Fatal(err)
	}
	if attachment.AttachmentDataRef == nil {
		t.Fatal("missing final attachment reference")
	}
	media, err := http.Get(s.URL() + "/v1/media/" + attachment.AttachmentDataRef.ResourceName + "?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	defer media.Body.Close()
	got, err := io.ReadAll(media.Body)
	if err != nil || string(got) != "abcdef" {
		t.Fatalf("download: %q, %v", got, err)
	}
	probe := put("bytes */6", "", 200)
	var repeated chat.UploadAttachmentResponse
	if err := json.NewDecoder(probe.Body).Decode(&repeated); err != nil || repeated.AttachmentDataRef == nil || repeated.AttachmentDataRef.ResourceName != attachment.AttachmentDataRef.ResourceName {
		t.Fatalf("completed session probe: %+v, %v", repeated, err)
	}
}

func TestNamedSpaceReplyOptions(t *testing.T) {
	for _, option := range []string{"", "MESSAGE_REPLY_OPTION_UNSPECIFIED", "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD", "REPLY_MESSAGE_OR_FAIL"} {
		for _, exists := range []bool{false, true} {
			t.Run(option+map[bool]string{false: "/missing", true: "/existing"}[exists], func(t *testing.T) {
				s := New(t, nil)
				s.Spaces["spaces/room"] = &chat.Space{Name: "spaces/room", SpaceType: "SPACE"}
				const thread = "spaces/room/threads/target"
				if exists {
					s.Messages["spaces/room/messages/seed"] = &chat.Message{Name: "spaces/room/messages/seed", Thread: &chat.Thread{Name: thread}}
				}
				status := 200
				if option == "REPLY_MESSAGE_OR_FAIL" && !exists {
					status = 404
				}
				got := request(t, s, "POST", "/v1/spaces/room/messages?messageReplyOption="+option, &chat.Message{Text: "reply", Thread: &chat.Thread{Name: thread, ThreadKey: "ignored-key"}}, status)
				if status == 404 {
					if got["error"].(map[string]any)["status"] != "NOT_FOUND" {
						t.Fatalf("missing thread: %v", got)
					}
					return
				}
				wantReply := exists && (option == "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD" || option == "REPLY_MESSAGE_OR_FAIL")
				if replied := got["thread"].(map[string]any)["name"] == thread; replied != wantReply {
					t.Fatalf("thread = %v; want existing reply %v", got["thread"], wantReply)
				}
			})
		}
	}
}

func TestUnknownReplyOptionRejected(t *testing.T) {
	s := New(t, nil)
	s.Spaces["spaces/room"] = &chat.Space{Name: "spaces/room", SpaceType: "SPACE"}
	got := request(t, s, "POST", "/v1/spaces/room/messages?messageReplyOption=REPLY_MESSAGE_TYPO", &chat.Message{Text: "reply"}, 400)
	if got["error"].(map[string]any)["status"] != "INVALID_ARGUMENT" {
		t.Fatalf("invalid reply option: %v", got)
	}
	listed := request(t, s, "GET", "/v1/spaces/room/messages", nil, 200)
	if len(listed["messages"].([]any)) != 0 {
		t.Fatal("invalid reply option created a message")
	}
	request(t, s, "POST", "/v1/spaces/room/messages?requestId=existing", &chat.Message{Text: "accepted"}, 200)
	request(t, s, "POST", "/v1/spaces/room/messages?requestId=existing&messageReplyOption=REPLY_MESSAGE_TYPO", &chat.Message{Text: "reply"}, 400)
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
	filter := `event_types:"` + created + `" AND start_time="` + now.Add(-2*time.Hour).Format(time.RFC3339Nano) + `"`
	path := "/v1/spaces/room/spaceEvents?filter=" + url.QueryEscape(filter) + "&pageSize=1"
	first := request(t, s, "GET", path, nil, 200)
	second := request(t, s, "GET", path+"&pageToken="+first["nextPageToken"].(string), nil, 200)
	if first["spaceEvents"].([]any)[0].(map[string]any)["name"] != "spaces/room/spaceEvents/1" || second["spaceEvents"].([]any)[0].(map[string]any)["name"] != "spaces/room/spaceEvents/2" || second["nextPageToken"] != nil {
		t.Fatalf("events: %v %v", first, second)
	}
	filter = `event_types:"` + created + `" AND start_time="` + now.Add(-time.Hour).Format(time.RFC3339Nano) + `"`
	exclusive := request(t, s, "GET", "/v1/spaces/room/spaceEvents?filter="+url.QueryEscape(filter), nil, 200)
	if len(exclusive["spaceEvents"].([]any)) != 1 {
		t.Fatalf("startTime not exclusive: %v", exclusive)
	}
	filter = `event_types:"` + created + `" AND start_time="` + now.Add(-29*24*time.Hour).Format(time.RFC3339Nano) + `"`
	request(t, s, "GET", "/v1/spaces/room/spaceEvents?filter="+url.QueryEscape(filter), nil, 400)
}

func TestDocumentedMessageFilters(t *testing.T) {
	s := New(t, nil)
	const thread = "spaces/room/threads/target"
	for i, fixture := range []struct{ at, thread string }{
		{"2026-01-30T10:00:00Z", thread},
		{"2026-01-30T11:00:00Z", thread},
		{"2026-01-30T11:00:00Z", "spaces/room/threads/other"},
		{"2026-01-30T12:00:00Z", thread},
	} {
		name := fmt.Sprintf("spaces/room/messages/%d", i)
		s.Messages[name] = &chat.Message{Name: name, CreateTime: fixture.at, Thread: &chat.Thread{Name: fixture.thread}}
	}
	for _, field := range []string{"create_time"} {
		t.Run(field, func(t *testing.T) {
			filter := field + ` > "2026-01-30T10:00:00Z" AND ` + field + ` < "2026-01-30T12:00:00Z" AND thread.name = ` + thread
			got := request(t, s, "GET", "/v1/spaces/room/messages?filter="+url.QueryEscape(filter), nil, 200)["messages"].([]any)
			if len(got) != 1 || got[0].(map[string]any)["name"] != "spaces/room/messages/1" {
				t.Fatalf("filtered messages: %v", got)
			}
		})
	}
}

func TestDocumentedSpaceEventFilters(t *testing.T) {
	s := New(t, nil)
	s.Now = func() time.Time { return time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC) }
	const created = "google.workspace.chat.message.v1.created"
	const reaction = "google.workspace.chat.reaction.v1.created"
	for i, fixture := range []struct{ at, typ string }{
		{"2026-01-30T10:00:00Z", created},
		{"2026-01-30T10:30:00Z", "google.workspace.chat.message.v1.batchCreated"},
		{"2026-01-30T11:00:00Z", reaction},
		{"2026-01-30T11:00:00Z", "google.workspace.chat.message.v1.deleted"},
		{"2026-01-30T11:30:00Z", created},
	} {
		s.SpaceEvents["spaces/room"] = append(s.SpaceEvents["spaces/room"], &chat.SpaceEvent{Name: fmt.Sprintf("spaces/room/spaceEvents/%d", i), EventTime: fixture.at, EventType: fixture.typ})
	}
	for _, fields := range [][3]string{{"event_types", "start_time", "end_time"}} {
		t.Run(fields[0], func(t *testing.T) {
			filter := fmt.Sprintf(`(%s:"%s" OR %s:"%s") AND %s="2026-01-30T10:00:00Z" AND %s="2026-01-30T11:00:00Z"`, fields[0], created, fields[0], reaction, fields[1], fields[2])
			got := request(t, s, "GET", "/v1/spaces/room/spaceEvents?filter="+url.QueryEscape(filter), nil, 200)["spaceEvents"].([]any)
			if len(got) != 2 || got[0].(map[string]any)["name"] != "spaces/room/spaceEvents/1" || got[1].(map[string]any)["name"] != "spaces/room/spaceEvents/2" {
				t.Fatalf("filtered events: %v", got)
			}
		})
	}
}

func TestEventTypeFilterRequired(t *testing.T) {
	for _, path := range []string{"/v1/subscriptions", "/v1/spaces/room/spaceEvents"} {
		for _, filter := range []string{"", `target_resource="//chat.googleapis.com/spaces/room"`, `event_types:""`} {
			t.Run(path+"/"+filter, func(t *testing.T) {
				s := New(t, nil)
				got := request(t, s, "GET", path+"?filter="+url.QueryEscape(filter), nil, 400)
				if got["error"].(map[string]any)["status"] != "INVALID_ARGUMENT" {
					t.Fatalf("missing event types: %v", got)
				}
			})
		}
	}
}

func TestFiltersRejectUndocumentedGrammar(t *testing.T) {
	for _, tc := range []struct {
		path    string
		filters []string
	}{
		{"/v1/spaces/room/spaceEvents", []string{
			`event_types:"A" AND event_types:"B"`,
			`(event_types:"A") AND (event_types:"B")`,
			`event_types:"A" AND start_time > "2026-01-30T10:00:00Z"`,
			`event_types:"A" AND start_time="2026-01-30T10:00:00Z" OR end_time="2026-01-30T11:00:00Z"`,
			`event_types:"A" AND unknown="value"`,
			`event_types:"A" trailing`,
			`(event_types:"A" OR event_types:"B"`,
			`event_types:"A")`,
			`event_types:"A" AND`,
			`event_types:"A" OR OR event_types:"B"`,
			`event_types:"A" AND start_time="bad"`,
		}},
		{"/v1/spaces/room/messages", []string{
			`create_time >= "2026-01-30T10:00:00Z"`,
			`create_time <= "2026-01-30T10:00:00Z"`,
			`create_time = "2026-01-30T10:00:00Z"`,
			`create_time > "2026-01-30T10:00:00Z" OR create_time < "2026-01-30T11:00:00Z"`,
			`create_time > 2026-01-30T10:00:00Z`,
			`thread.name = "spaces/room/threads/one"`,
			`thread.name = spaces/room/threads/one AND thread.name = spaces/room/threads/two`,
			`thread.name = nope`,
			`text="ignored"`,
			`create_time > "2026-01-30T10:00:00Z" garbage`,
		}},
		{"/v1/subscriptions", []string{
			`event_types:"A" AND event_types:"B"`,
			`event_types:"A" OR target_resource="//chat.googleapis.com/spaces/-"`,
			`event_types:"A" AND target_resource:"//chat.googleapis.com/spaces/-"`,
			`event_types:"A" AND state="ACTIVE"`,
			`event_types:"A" AND target_resource="one" AND target_resource="two"`,
			`event_types:"A" garbage`,
		}},
	} {
		for _, filter := range tc.filters {
			t.Run(tc.path+"/"+filter, func(t *testing.T) {
				s := New(t, nil)
				s.Now = func() time.Time { return time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC) }
				got := request(t, s, "GET", tc.path+"?filter="+url.QueryEscape(filter), nil, 400)
				if got["error"].(map[string]any)["status"] != "INVALID_ARGUMENT" {
					t.Fatalf("invalid filter response: %v", got)
				}
			})
		}
	}
}

func TestFilterCamelCaseAliases(t *testing.T) {
	s := New(t, nil)
	s.Now = func() time.Time { return time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC) }
	s.Messages["spaces/room/messages/1"] = &chat.Message{Name: "spaces/room/messages/1", CreateTime: "2026-01-30T11:00:00Z"}
	s.SpaceEvents["spaces/room"] = []*chat.SpaceEvent{{Name: "spaces/room/spaceEvents/1", EventTime: "2026-01-30T11:00:00Z", EventType: "A"}}
	s.Subscriptions["subscriptions/1"] = &events.Subscription{Name: "subscriptions/1", TargetResource: "//chat.googleapis.com/spaces/room", EventTypes: []string{"A"}, State: "ACTIVE"}
	for _, tc := range []struct{ path, key, filter string }{
		{"/v1/spaces/room/messages", "messages", `createTime > "2026-01-30T10:00:00Z" AND createTime < "2026-01-30T12:00:00Z"`},
		{"/v1/spaces/room/spaceEvents", "spaceEvents", `eventTypes:"A" AND startTime="2026-01-30T10:00:00Z" AND endTime="2026-01-30T11:00:00Z"`},
		{"/v1/subscriptions", "subscriptions", `(eventTypes:"A" OR eventTypes:"B") AND targetResource="//chat.googleapis.com/spaces/room"`},
	} {
		t.Run(tc.key, func(t *testing.T) {
			got := request(t, s, "GET", tc.path+"?filter="+url.QueryEscape(tc.filter), nil, 200)
			if len(got[tc.key].([]any)) != 1 {
				t.Fatalf("alias filter: %v", got)
			}
		})
	}
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
		{"google.workspace.events.subscription.v1.expirationReminder", &events.Subscription{Name: "subscriptions/lifecycle"}, "subscription"},
		{"google.workspace.events.subscription.v1.expired", &events.Subscription{Name: "subscriptions/lifecycle"}, "subscription"},
		{"google.workspace.events.subscription.v1.suspended", &events.Subscription{Name: "subscriptions/lifecycle"}, "subscription"},
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
			subject := "//chat.googleapis.com/spaces/room"
			if strings.HasPrefix(m.Attributes["ce-type"], "google.workspace.events.subscription.v1.") {
				subject = "//workspaceevents.googleapis.com/subscriptions/lifecycle"
			}
			if m.Attributes["ce-subject"] != subject || m.Attributes["ce-id"] == "" || seen[m.Attributes["ce-id"]] {
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

func TestEmitCanRespectSubscriptions(t *testing.T) {
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
	const topic = "projects/example/topics/events"
	if _, err := client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	publisher := client.Publisher(topic)
	defer publisher.Stop()
	s := New(t, publisher)
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	const created = "google.workspace.chat.message.v1.created"
	for _, tc := range []struct {
		name, state, target, eventType string
		respect, expired, want         bool
	}{
		{name: "default without subscription", want: true},
		{name: "none", respect: true},
		{name: "suspended", state: "SUSPENDED", respect: true},
		{name: "expired", state: "ACTIVE", respect: true, expired: true},
		{name: "active", state: "ACTIVE", respect: true, want: true},
		{name: "other space", state: "ACTIVE", target: "//chat.googleapis.com/spaces/other", respect: true},
		{name: "all spaces", state: "ACTIVE", target: "//chat.googleapis.com/spaces/-", respect: true, want: true},
		{name: "other type", state: "ACTIVE", eventType: "google.workspace.chat.reaction.v1.created", respect: true},
		{name: "batch", state: "ACTIVE", eventType: "google.workspace.chat.message.v1.batchCreated", respect: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.Configure(func(s *Server) {
				s.EmitRespectsSubscriptions = tc.respect
				s.Subscriptions = map[string]*events.Subscription{}
				if tc.state != "" {
					target := tc.target
					if target == "" {
						target = "//chat.googleapis.com/spaces/room"
					}
					expiry := now.Add(time.Hour)
					if tc.expired {
						expiry = now
					}
					s.Subscriptions["subscriptions/one"] = &events.Subscription{Name: "subscriptions/one", TargetResource: target, EventTypes: []string{created}, State: tc.state, ExpireTime: expiry.Format(time.RFC3339Nano)}
				}
			})
			typ := tc.eventType
			if typ == "" {
				typ = created
			}
			var resource any = &chat.Message{Name: "spaces/room/messages/1"}
			if strings.Contains(typ, ".batch") {
				resource = []*chat.Message{{Name: "spaces/room/messages/1"}}
			}
			before := len(ps.Messages())
			s.Emit(typ, resource, "spaces/room")
			want := 0
			if tc.want {
				want = 1
			}
			if got := len(ps.Messages()) - before; got != want {
				t.Fatalf("published %d messages, want %d", got, want)
			}
		})
	}
}

func TestSpaceEventsPaginationSurvivesSlidingWindow(t *testing.T) {
	s := New(t, nil)
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	const created = "google.workspace.chat.message.v1.created"
	for i, age := range []time.Duration{28*24*time.Hour - time.Second, time.Hour, time.Hour} {
		s.SpaceEvents["spaces/room"] = append(s.SpaceEvents["spaces/room"], &chat.SpaceEvent{Name: fmt.Sprintf("spaces/room/spaceEvents/%d", i), EventTime: now.Add(-age).Format(time.RFC3339Nano), EventType: created})
	}
	path := "/v1/spaces/room/spaceEvents?filter=" + url.QueryEscape(`event_types:"`+created+`" AND end_time="`+now.Format(time.RFC3339Nano)+`"`) + "&pageSize=1"
	first := request(t, s, "GET", path, nil, 200)
	s.Advance(2 * time.Second)
	second := request(t, s, "GET", path+"&pageToken="+url.QueryEscape(first["nextPageToken"].(string)), nil, 200)
	if got := second["spaceEvents"].([]any)[0].(map[string]any)["name"]; got != "spaces/room/spaceEvents/1" {
		t.Fatalf("sliding window skipped event: %v", second)
	}
	third := request(t, s, "GET", path+"&pageToken="+url.QueryEscape(second["nextPageToken"].(string)), nil, 200)
	if got := third["spaceEvents"].([]any)[0].(map[string]any)["name"]; got != "spaces/room/spaceEvents/2" || third["nextPageToken"] != nil {
		t.Fatalf("equal-time event lost: %v", third)
	}
}

func TestSubscriptionSnakeCaseOrFilter(t *testing.T) {
	s := New(t, nil)
	for name, target := range map[string]string{"a": "spaces/-", "b": "spaces/-", "other": "spaces/other"} {
		typ := "google.workspace.chat.message.v1.created"
		if name == "b" {
			typ = "google.workspace.chat.reaction.v1.created"
		}
		s.Subscriptions["subscriptions/"+name] = &events.Subscription{Name: "subscriptions/" + name, TargetResource: "//chat.googleapis.com/" + target, EventTypes: []string{typ}, State: "ACTIVE"}
	}
	filter := `(event_types:"google.workspace.chat.message.v1.created" OR event_types:"google.workspace.chat.reaction.v1.created") AND target_resource="//chat.googleapis.com/spaces/-"`
	result := request(t, s, "GET", "/v1/subscriptions?filter="+url.QueryEscape(filter), nil, 200)
	if got := result["subscriptions"].([]any); len(got) != 2 || got[0].(map[string]any)["name"] != "subscriptions/a" || got[1].(map[string]any)["name"] != "subscriptions/b" {
		t.Fatalf("filter result: %v", result)
	}
}
