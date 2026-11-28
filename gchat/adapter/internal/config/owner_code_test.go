package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/ack"
)

// TestChatOwnerCodesNeverReachTheReactionProvider: a message whose text reads
// as a one-time code (the owner page's 12 characters, or the 10 of a join's
// or a removal's approval) is never sent to the reaction provider; any
// other text still is.
func TestChatOwnerCodesNeverReachTheReactionProvider(t *testing.T) {
	var asked atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	choice, err := ack.New(ack.Options{Mode: "jev", Key: "example-key", Base: srv.URL, Catalog: ack.Catalog, Redact: AckRedact})
	if err != nil {
		t.Fatal(err)
	}
	// The join's check makes letters upper case by Unicode, so a letter whose
	// upper case is one of the alphabet's reads as it does there.
	for _, text := range []string{"ABCD-EFGH-JKLM", "abcd efgh jklm", " ABCDEFGHJKLM\n", "ABCDEFGHJK", "abcdefghjk", "abcdefghj\u017f"} {
		before := asked.Load()
		if row, _ := choice.Choose(context.Background(), text, time.Now()); row != ack.Default || asked.Load() != before {
			t.Fatalf("%q reached the provider (row %v)", text, row)
		}
	}
	before := asked.Load()
	choice.Choose(context.Background(), "status of the order?", time.Now())
	if asked.Load() != before+1 {
		t.Fatal("ordinary text did not reach the provider: the fake proves nothing")
	}
}
