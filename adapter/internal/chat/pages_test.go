package chat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegoogle"
	api "google.golang.org/api/chat/v1"
)

func TestHistoryPages(t *testing.T) {
	for _, mode := range []string{"messages", "events", "retained-events"} {
		t.Run(mode, func(t *testing.T) {
			events := mode != "messages"
			s := fakegoogle.New(t, nil)
			upper := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			lower := upper.Add(-time.Hour)
			const space, thread = "spaces/room", "spaces/room/threads/topic"
			const typ = "google.workspace.chat.message.v1.updated"
			s.Now = func() time.Time { return upper }
			s.Spaces[space] = &api.Space{Name: space, SpaceType: "SPACE"}
			for i := 0; i < 1001; i++ {
				stamp := lower.Add(time.Duration(i+1) * time.Second).Format(time.RFC3339Nano)
				name := fmt.Sprintf("%s/messages/m%04d", space, i)
				s.Messages[name] = &api.Message{Name: name, CreateTime: stamp, Thread: &api.Thread{Name: thread}}
				s.SpaceEvents[space] = append(s.SpaceEvents[space], &api.SpaceEvent{Name: fmt.Sprintf("%s/spaceEvents/e%04d", space, i), EventTime: stamp, EventType: typ})
			}
			if mode == "retained-events" {
				lower = time.Time{}
			}
			calls := 0
			token := ""
			h := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				q := r.URL.Query()
				if q.Get("pageToken") != token || q.Get("pageSize") != "1000" {
					t.Errorf("page parameters: %v", q)
				}
				wantFilter := `create_time > "2026-10-22T11:00:00Z" AND create_time < "2026-10-22T12:00:00Z" AND thread.name = ` + thread
				if events {
					wantFilter = `start_time="2026-10-22T11:00:00Z" AND end_time="2026-10-22T12:00:00Z" AND (event_types:"` + typ + `")`
					if mode == "retained-events" {
						wantFilter = strings.TrimPrefix(wantFilter, `start_time="2026-10-22T11:00:00Z" AND `)
					}
				} else if q.Get("orderBy") != "create_time ASC" {
					t.Errorf("orderBy = %q", q.Get("orderBy"))
				}
				if q.Get("filter") != wantFilter {
					t.Errorf("filter = %q, want %q", q.Get("filter"), wantFilter)
				}
				return http.DefaultTransport.RoundTrip(r)
			})}
			c, err := New(context.Background(), h, config.Endpoints{ChatAPIBase: s.URL()})
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range []int{1000, 1} {
				var n int
				if events {
					var page []*api.SpaceEvent
					page, token, err = c.ListSpaceEventsPage(context.Background(), space, lower, upper, []string{typ}, token)
					n = len(page)
				} else {
					var page []*api.Message
					page, token, err = c.ListMessagesPage(context.Background(), space, lower, upper, thread, token)
					n = len(page)
				}
				if err != nil || n != want || (i == 0 && token == "") || (i == 1 && token != "") {
					t.Fatalf("page %d: n=%d token=%q err=%v", i, n, token, err)
				}
			}
			if calls != 2 {
				t.Fatalf("calls = %d", calls)
			}
		})
	}
}

func TestHistoryPageRetriesAndCredentials(t *testing.T) {
	for _, events := range []bool{false, true} {
		for _, status := range []int{429, 503, 401, 403} {
			t.Run(fmt.Sprintf("events=%v/status=%d", events, status), func(t *testing.T) {
				now := time.Now().UTC()
				calls := 0
				h := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.Query().Get("pageToken") != "opaque-resume-token" {
						t.Error("retry lost page token")
					}
					if calls == 1 {
						return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"2"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"code":%d,"message":"credential or quota"}}`, status))), Request: r}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"messages":[],"spaceEvents":[]}`)), Request: r}, nil
				})}
				c, err := New(context.Background(), h, config.Endpoints{ChatAPIBase: "https://chat.invalid"})
				if err != nil {
					t.Fatal(err)
				}
				clock := useTestClock(c)
				start := clock.Now()
				if events {
					_, _, err = c.ListSpaceEventsPage(context.Background(), "spaces/room", now.Add(-time.Hour), now, []string{"google.workspace.chat.message.v1.updated"}, "opaque-resume-token")
				} else {
					_, _, err = c.ListMessagesPage(context.Background(), "spaces/room", now.Add(-time.Hour), now, "", "opaque-resume-token")
				}
				if status == 401 || status == 403 {
					kind, _ := Classify(err)
					if kind != "auth" || calls != 1 {
						t.Fatalf("kind=%q calls=%d err=%v", kind, calls, err)
					}
				} else if err != nil || calls != 2 || (status == 429 && clock.Now().Sub(start) < 2*time.Second) {
					t.Fatalf("retry: calls=%d elapsed=%v err=%v", calls, clock.Now().Sub(start), err)
				}
			})
		}
	}
}

func TestHistoryPageValidation(t *testing.T) {
	c, err := New(context.Background(), http.DefaultClient, config.Endpoints{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, bounds := range [][2]time.Time{{{}, now}, {now, {}}, {now, now}, {now, now.Add(-time.Hour)}} {
		if _, _, err := c.ListMessagesPage(context.Background(), "spaces/room", bounds[0], bounds[1], "", ""); err == nil {
			t.Error("accepted invalid message interval")
		}
		if bounds[0].IsZero() {
			continue // Events may use Google's default retained lower bound.
		}
		if _, _, err := c.ListSpaceEventsPage(context.Background(), "spaces/room", bounds[0], bounds[1], []string{"type"}, ""); err == nil {
			t.Error("accepted invalid event interval")
		}
	}
	if _, _, err := c.ListMessagesPage(context.Background(), "spaces/room", now.Add(-time.Hour), now, "invalid", ""); err == nil {
		t.Error("accepted invalid thread")
	}
	if _, _, err := c.ListSpaceEventsPage(context.Background(), "spaces/room", now.Add(-time.Hour), now, nil, ""); err == nil {
		t.Error("accepted no event types")
	}
}
