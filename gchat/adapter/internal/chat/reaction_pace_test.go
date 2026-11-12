package chat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gchat-adapter/internal/ack"
	"gchat-adapter/internal/config"
)

func TestOverflowKeepsEveryArrivalPastTenSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		calls := 0
		client, err := New(context.Background(), &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"name":"spaces/example/messages/example/reactions/example"}`))}, nil
		})}, config.Endpoints{ChatAPIBase: "http://127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		choice, err := ack.New(ack.Options{Mode: "eyes", Catalog: ack.Catalog})
		if err != nil {
			t.Fatal(err)
		}
		service := &ack.Service{Choice: choice, Dir: t.TempDir(), SendChoice: func(ctx context.Context, in ack.Input, choose func(context.Context) (ack.Row, string)) (string, error) {
			return client.CreateArrivalReaction(ctx, in.Message, in.Seen.Add(ack.ReactionBudget), ack.Default.Emoji, func(ctx context.Context) (string, string) { row, how := choose(ctx); return row.Emoji, how })
		}}
		start := time.Now()
		for i := 0; i < 60; i++ {
			service.Observe(ack.Input{Conversation: "spaces/example", Message: fmt.Sprintf("spaces/example/messages/item-%d", i), Human: true, Addressed: true, Time: start, Seen: start})
		}
		service.Wait()
		if calls != 60 {
			t.Fatalf("overflow dropped requests: got %d want 60", calls)
		}
	})
}

func TestArrivalPaceFakeRequests(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		delay time.Duration
	}{
		{"one timeout", 1, 1500 * time.Millisecond}, {"nine timeouts", 9, 1500 * time.Millisecond},
		{"nine choices", 9, 300 * time.Millisecond}, {"overflow", 12, 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var mu sync.Mutex
				var times []time.Duration
				var bodies []string
				client, err := New(context.Background(), &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(r.Body)
					mu.Lock()
					times = append(times, time.Since(start))
					bodies = append(bodies, string(body))
					mu.Unlock()
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"name":"spaces/example/messages/example/reactions/example"}`))}, nil
				})}, config.Endpoints{ChatAPIBase: "http://127.0.0.1"})
				if err != nil {
					t.Fatal(err)
				}
				var wg sync.WaitGroup
				for i := 0; i < tc.count; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, err := client.CreateArrivalReaction(context.Background(), "spaces/example/messages/example", start.Add(2*time.Second), "👀", func(ctx context.Context) (string, string) {
							select {
							case <-ctx.Done():
								return "👀", "default"
							case <-time.After(tc.delay):
							}
							if tc.delay == 300*time.Millisecond {
								return "❤️", "jev"
							}
							return "👀", "default"
						})
						if err != nil {
							t.Error(err)
						}
					}()
				}
				wg.Wait()
				if len(times) != tc.count {
					t.Fatal("dropped reaction")
				}
				for i, at := range times {
					if i > 0 && at-times[i-1] < 200*time.Millisecond {
						t.Fatal("pace exceeded")
					}
					if tc.count <= 11 && at > 2*time.Second {
						t.Fatalf("request %d at %v", i, at)
					}
					want := "👀"
					if tc.delay == 300*time.Millisecond {
						want = "❤️"
					}
					if !strings.Contains(bodies[i], want) {
						t.Fatal("incorrect choice")
					}
				}
				if tc.count == 1 && times[0] < 1500*time.Millisecond {
					t.Fatal("lone choice abandoned early")
				}
				if tc.count == 9 && tc.delay == 1500*time.Millisecond && times[0] != 400*time.Millisecond {
					t.Fatal("not latest safe slot")
				}
				if tc.count == 12 && times[11] != 2200*time.Millisecond {
					t.Fatal("overflow pace")
				}
			})
		})
	}
}

// The fake clock advances to the next send or choice completion. The fake
// channel records the permit at which each HTTP request is launched.
func TestArrivalLatestSafeSlots(t *testing.T) {
	for _, tc := range []struct {
		name   string
		count  int
		answer time.Duration
	}{
		{"one timeout", 1, 1500 * time.Millisecond},
		{"nine timeouts", 9, 1500 * time.Millisecond},
		{"nine choices at 300ms", 9, 300 * time.Millisecond},
		{"one beyond capacity", 12, 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(1700000000, 0)
			now := start
			q := &reactionQueue{}
			ready := make(chan struct{})
			for i := 0; i < tc.count; i++ {
				q.jobs = append(q.jobs, &reactionJob{ctx: context.Background(), deadline: start.Add(2 * time.Second), ready: ready})
			}
			answered := false
			var requested []time.Time
			for len(q.jobs) > 0 {
				index, at := q.plan(now)
				if !answered && !at.Before(start.Add(tc.answer)) {
					now = start.Add(tc.answer)
					close(ready)
					answered = true
					continue
				}
				now = at
				chosen := isReady(q.jobs[index].ready) && tc.answer < 1500*time.Millisecond && !now.After(q.jobs[index].deadline)
				if tc.answer == 300*time.Millisecond && now.After(start.Add(tc.answer)) && !chosen {
					t.Fatal("gave up a usable choice")
				}
				requested = append(requested, now)
				q.jobs = append(q.jobs[:index], q.jobs[index+1:]...)
				q.next = now.Add(reactionInterval)
			}
			for i, at := range requested {
				if i > 0 && at.Sub(requested[i-1]) < 200*time.Millisecond {
					t.Fatal("burst exceeded pace")
				}
				if tc.count <= 11 && at.Sub(start) > 2*time.Second {
					t.Fatal("missed deadline")
				}
			}
			if tc.count == 1 && requested[0].Sub(start) < 1500*time.Millisecond {
				t.Fatal("cut short lone choice")
			}
			if tc.count == 9 && tc.answer == 1500*time.Millisecond && requested[0].Sub(start) != 400*time.Millisecond {
				t.Fatal("did not use latest safe slot")
			}
			if tc.count == 12 && requested[11].Sub(start) != 2200*time.Millisecond {
				t.Fatal("overflow dropped or mispaced")
			}
		})
	}
}
