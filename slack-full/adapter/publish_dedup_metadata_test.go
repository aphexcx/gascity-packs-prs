package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestPublishDedupCacheHandsOutIndependentReceipts pins gf-pu2 finding 3e
// (the gf-jr0 vs-main gate's Major at publish_readback.go:631): Get returned
// the cached receipt by value with its Metadata map shared with the cache
// entry, so a caller's write to the map was a write to the cache, outside
// its lock. A receipt that crosses the cache boundary — in or out — now owns
// its own map.
func TestPublishDedupCacheHandsOutIndependentReceipts(t *testing.T) {
	dedup := newPublishDedupCache(publishDedupTTL)
	original := publishReceipt{MessageID: "1700.001", Metadata: map[string]string{receiptMetadataKeyReadback: slackReadbackUnavailable}}
	dedup.Put("k-1", original)

	// A write to the receipt that was Put does not reach the cache.
	original.Metadata["truncated"] = "true"
	first, ok := dedup.Get("k-1")
	if !ok {
		t.Fatal("the posted-but-unconfirmed receipt must be remembered")
	}
	if _, leaked := first.Metadata["truncated"]; leaked {
		t.Fatalf("the cache shares the map it was given: %v", first.Metadata)
	}

	// A write to the receipt that was Got does not reach the cache either.
	first.Metadata[receiptMetadataKeyReadback] = slackReadbackUnconfirmed
	delete(first.Metadata, receiptMetadataKeyReadback)
	second, _ := dedup.Get("k-1")
	if second.Metadata[receiptMetadataKeyReadback] != slackReadbackUnavailable {
		t.Fatalf("the cache shares the map it hands out: %v", second.Metadata)
	}

	// A receipt without metadata comes back without one, not with an empty map.
	dedup.Put("k-2", publishReceipt{MessageID: "1700.002", Delivered: true})
	if got, _ := dedup.Get("k-2"); got.Metadata != nil {
		t.Fatalf("a nil Metadata must stay nil across the cache, got %v", got.Metadata)
	}
}

// TestReplayPublishReceiptConcurrentReVerifiesDoNotRace is the gate's
// scenario under the race detector: several retries of one idempotency key
// whose read leg is still down re-verify at once, and each
// confirmPublishReceipt writes the readback detail into its receipt's
// Metadata. Before the fix those were writes to one shared map without the
// cache's lock — a fatal concurrent map write in the adapter; `go test -race`
// reports it. After it, every retry works on its own copy and the cache ends
// with one coherent receipt.
func TestReplayPublishReceiptConcurrentReVerifiesDoNotRace(t *testing.T) {
	fastReadbackTimings(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // the read leg stays down: every re-verify records readback_unavailable
	}))
	t.Cleanup(server.Close)
	originalBase := slackAPIBase
	slackAPIBase = server.URL
	t.Cleanup(func() { slackAPIBase = originalBase })

	dedup := newPublishDedupCache(publishDedupTTL)
	dedup.Put("k-1", publishReceipt{MessageID: "1700.001", Metadata: map[string]string{receiptMetadataKeyReadback: slackReadbackUnavailable}})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replay, ok := replayPublishReceipt("xoxb-test", dedup, "k-1", readbackTarget("hello"))
			receipt := replay.receipt
			if !ok || !receipt.Delivered || publishReadbackConfirmed(receipt) {
				t.Errorf("replay = %+v, %v; want the accepted but unconfirmed receipt", receipt, ok)
			}
		}()
	}
	wg.Wait()

	cached, ok := dedup.Get("k-1")
	if !ok || !cached.Delivered || publishReadbackConfirmed(cached) || cached.MessageID != "1700.001" || cached.Metadata[receiptMetadataKeyReadback] != slackReadbackUnavailable {
		t.Fatalf("cached receipt after concurrent re-verifies = %+v, %v", cached, ok)
	}
}
