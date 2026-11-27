package gcclient

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/wire"
)

func TestUnansweredPostRequestBounds(t *testing.T) {
	for name, call := range clientCalls() {
		t.Run(name, func(t *testing.T) {
			want := 35 * time.Second
			if name == "inbound" {
				want = 120 * time.Second
			}
			c := New("http://gc.invalid", "examplecity")
			c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				end, ok := r.Context().Deadline()
				if left := time.Until(end); !ok || left > want || left < want-time.Second {
					t.Errorf("request bound = %v, want %v", left, want)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			if err := call(t.Context(), c); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUnansweredPostReadsSlowAnswer(t *testing.T) {
	c := New("http://gc.invalid", "examplecity")
	c.http.Timeout = 35 * time.Millisecond
	c.inboundTimeout = time.Second
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case <-time.After(100 * time.Millisecond):
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"delivery":{"status":"delivered","receipt_id":"receipt-example"}}`))}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	result, err := c.Inbound(context.Background(), wire.InboundMessage{})
	if err != nil || result.Delivery.Status != "delivered" {
		t.Fatalf("slow answer = %q, %v", result.Delivery.Status, err)
	}
}
