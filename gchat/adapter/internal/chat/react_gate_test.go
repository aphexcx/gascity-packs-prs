package chat

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gchat-adapter/internal/config"
)

// With the acknowledgement off, the mayor's react verb takes the base's
// identity-wide gate (five a second, no burst) and no arrival queue exists.
func TestReactVerbUsesBaseGateWhileOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var mu sync.Mutex
		var times []time.Duration
		client, err := New(context.Background(), &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			times = append(times, time.Since(start))
			mu.Unlock()
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"name":"spaces/example/messages/example/reactions/example"}`))}, nil
		})}, config.Endpoints{ChatAPIBase: "http://127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if _, err := client.CreateReaction(context.Background(), "spaces/example/messages/example", "👍"); err != nil {
				t.Fatal(err)
			}
		}
		if len(times) != 3 || times[0] != 0 || times[1] != 200*time.Millisecond || times[2] != 400*time.Millisecond {
			t.Fatalf("react verb slots = %v, want [0s 200ms 400ms]", times)
		}
		client.limits.mu.Lock()
		created := client.limits.reactionChoices != nil
		client.limits.mu.Unlock()
		if created {
			t.Fatal("the arrival queue was created while the acknowledgement is off")
		}
	})
}
