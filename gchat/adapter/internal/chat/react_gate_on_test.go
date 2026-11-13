package chat

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"gchat-adapter/internal/config"
)

// With the acknowledgement on, the react verb keeps sharing the arrival
// queue (RULING 2 of round 1); only the switch unset restores the base gate.
func TestReactVerbSharesArrivalQueueWhileOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, err := New(context.Background(), &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"name":"spaces/example/messages/example/reactions/example"}`))}, nil
		})}, config.Endpoints{ChatAPIBase: "http://127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		client.EnableArrivalPace()
		if _, err := client.CreateReaction(context.Background(), "spaces/example/messages/example", "👍"); err != nil {
			t.Fatal(err)
		}
		client.limits.mu.Lock()
		shared := client.limits.reactionChoices != nil && client.limits.reactions == nil
		client.limits.mu.Unlock()
		if !shared {
			t.Fatal("with the acknowledgement on, the react verb left the shared queue")
		}
	})
}
