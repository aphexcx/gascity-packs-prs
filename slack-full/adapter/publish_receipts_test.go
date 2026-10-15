package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPublishReceiptStore(t *testing.T, dir string) *publishReceiptStore {
	t.Helper()
	store, err := newPublishReceiptStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestPublishReceiptSurvivesRestartAndReadbackUpdates(t *testing.T) {
	dir := t.TempDir()
	store := testPublishReceiptStore(t, dir)
	postedAt := time.Now().UTC().Truncate(time.Second)
	store.now = func() time.Time { return postedAt }
	key := "../../caller key?&="
	receipt := publishReceipt{Conversation: conversationRef{ConversationID: "C1"}, MessageID: "1700.002"}
	post := slackPostMessageReq{Text: "hello", ThreadTS: "1700.001"}
	if err := store.RecordPosted(key, receipt, post); err != nil {
		t.Fatal(err)
	}
	store = testPublishReceiptStore(t, dir) // simulate a crash before readback
	got, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Delivered || got.Readback != "pending" || got.ConversationID != "C1" || got.MessageID != "1700.002" || got.ThreadTS != "1700.001" || got.ReplyToMessageID != got.ThreadTS || !got.PostedAt.Equal(postedAt) {
		t.Fatalf("pending receipt = %+v", got)
	}
	if got.PostedTextSHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("posted text digest = %q", got.PostedTextSHA256)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 || len(files[0].Name()) != 64+len(".json") {
		t.Fatalf("private digest-keyed files = %v, %v", files, err)
	}
	info, err := os.Stat(filepath.Join(dir, files[0].Name()))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt file permissions: %v, %v", info, err)
	}
	receipt.Delivered = true
	if err := store.RecordReadback(key, receipt); err != nil {
		t.Fatal(err)
	}
	store = testPublishReceiptStore(t, dir)
	got, err = store.Get(key)
	if err != nil || !got.Delivered || got.Readback != "confirmed" || !got.PostedAt.Equal(postedAt) {
		t.Fatalf("confirmed receipt after restart = %+v, %v", got, err)
	}
	// A slow failed read cannot downgrade proof, nor overwrite a newer post.
	receipt.Delivered = false
	receipt.FailureKind = "transient"
	receipt.Metadata = map[string]string{receiptMetadataKeyReadback: slackReadbackUnavailable}
	if err := store.RecordReadback(key, receipt); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(key)
	if err != nil || !got.Delivered {
		t.Fatalf("slow read downgraded confirmation: %+v, %v", got, err)
	}
	newPost := receipt
	newPost.MessageID = "1700.003"
	if err := store.RecordPosted(key, newPost, slackPostMessageReq{Text: "next"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordReadback(key, receipt); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(key)
	if err != nil || got.MessageID != "1700.003" || got.Readback != "pending" {
		t.Fatalf("stale read changed replacement: %+v, %v", got, err)
	}
}

func TestPublishReceiptPruneByPostedAge(t *testing.T) {
	store := testPublishReceiptStore(t, t.TempDir())
	now := time.Now().UTC()
	store.now = func() time.Time { return now.Add(-31 * 24 * time.Hour) }
	receipt := publishReceipt{Conversation: conversationRef{ConversationID: "C1"}, MessageID: "1.2"}
	if err := store.RecordPosted("old", receipt, slackPostMessageReq{Text: "old"}); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	if err := store.RecordPosted("fresh", receipt, slackPostMessageReq{Text: "fresh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("old"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired lookup = %v, want unknown before sweep", err)
	}
	removed, err := store.Prune()
	if err != nil || removed != 1 {
		t.Fatalf("Prune = %d, %v", removed, err)
	}
	if _, err := os.Stat(store.path("old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old receipt file remains: %v", err)
	}
	if _, err := store.Get("fresh"); err != nil {
		t.Fatalf("fresh receipt lost: %v", err)
	}
}

func TestPublishReceiptGET(t *testing.T) {
	store := testPublishReceiptStore(t, t.TempDir())
	key := "key with ?&=/ reserved characters"
	receipt := publishReceipt{Conversation: conversationRef{ConversationID: "C1"}, MessageID: "1.2"}
	if err := store.RecordPosted(key, receipt, slackPostMessageReq{Text: "hello", ThreadTS: "1.1"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path("corrupt"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		key, method string
		status      int
	}{
		{key, http.MethodGet, http.StatusOK},
		{"unknown", http.MethodGet, http.StatusNotFound},
		{"", http.MethodGet, http.StatusBadRequest},
		{"corrupt", http.MethodGet, http.StatusInternalServerError},
		{key, http.MethodPost, http.StatusMethodNotAllowed},
	} {
		t.Run(test.key+test.method, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "/publish-receipt?idempotency_key="+url.QueryEscape(test.key), nil)
			w := httptest.NewRecorder()
			handlePublishReceipt(store)(w, r)
			if w.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, test.status, w.Body)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("receipt lookup can be cached across pending/confirmed transitions")
			}
			if test.status == http.StatusOK {
				var got map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got["delivered"] != false || got["conversation_id"] != "C1" || got["message_id"] != "1.2" || got["thread_ts"] != "1.1" || got["reply_to_message_id"] != "1.1" || got["readback"] != "pending" {
					t.Fatalf("flat receipt shape = %v", got)
				}
			}
		})
	}
}

func TestPublishReceiptRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "store")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newPublishReceiptStore(link); err == nil {
		t.Fatal("opened symlinked store")
	}
	store := testPublishReceiptStore(t, dir)
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), store.path("link")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("link"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink lookup = %v, want a storage error", err)
	}
}

func TestHandlePublishPersistsAcceptanceBeforeReadbackAndRetry(t *testing.T) {
	fastReadbackTimings(t)
	store := testPublishReceiptStore(t, t.TempDir())
	const key = "integration-key"
	const posted = "hello <@U123> & welcome"
	var captured slackPostMessageReq
	posts := 0
	readable := false
	original := slackAPIBase
	t.Cleanup(func() { slackAPIBase = original })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/chat.postMessage" {
			posts++
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"ok":true,"ts":"1700.002"}`))
			return
		}
		if r.URL.Path != "/conversations.replies" || r.URL.Query().Get("ts") != "1700.001" {
			t.Errorf("readback request = %s", r.URL)
		}
		got, err := store.Get(key)
		if err != nil || got.MessageID != "1700.002" || got.PostedTextSHA256 != publishReceiptDigest(posted) {
			t.Errorf("readback began before persisted acceptance: %+v, %v", got, err)
		}
		if !readable {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]string{{"ts": "1700.002", "text": slackCanonicalizeForTest(posted)}}})
	}))
	defer server.Close()
	slackAPIBase = server.URL
	cfg := config{slackBotToken: "xoxb-test", publishReceipts: store}
	aliases := &userAliasMap{byHandle: map[string]string{"bob": "<@U123>"}}
	handler := handlePublish(cfg, nil, aliases, newPublishDedupCache(publishDedupTTL))
	body := `{"session_id":"s1","conversation":{"conversation_id":"C1"},"reply_to_message_id":"1700.001","text":"hello @bob & welcome","idempotency_key":"integration-key"}`
	for _, confirmed := range []bool{false, true} {
		readable = confirmed
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(body)))
		var receipt publishReceipt
		if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Delivered != confirmed || captured.Text != posted || posts != 1 {
			t.Fatalf("receipt=%+v text=%q posts=%d", receipt, captured.Text, posts)
		}
		reopened := testPublishReceiptStore(t, store.dir)
		got, err := reopened.Get(key)
		wantState := slackReadbackUnavailable
		if confirmed {
			wantState = "confirmed"
		}
		if err != nil || got.Delivered != confirmed || got.Readback != wantState || got.FailureKind != receipt.FailureKind {
			t.Fatalf("persisted readback = %+v, %v", got, err)
		}
	}
}

func TestHandlePublishPersistenceFailureDoesNotRepost(t *testing.T) {
	store := testPublishReceiptStore(t, filepath.Join(t.TempDir(), "receipts"))
	if err := os.Remove(store.dir); err != nil {
		t.Fatal(err)
	}
	original := slackAPIBase
	t.Cleanup(func() { slackAPIBase = original })
	var captured slackPostMessageReq
	posts := 0
	server := httptest.NewServer(newFakeSlackPublishHandler(t, &captured, "1.2", func() { posts++ }))
	defer server.Close()
	slackAPIBase = server.URL
	handler := handlePublish(config{slackBotToken: "xoxb-test", publishReceipts: store}, nil, nil, newPublishDedupCache(publishDedupTTL))
	for range 2 {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(`{"session_id":"s1","conversation":{"conversation_id":"C1"},"text":"hello","idempotency_key":"disk-failure"}`)))
		var receipt publishReceipt
		if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if !receipt.Delivered || receipt.Metadata["receipt_store"] != "unavailable" || posts != 1 {
			t.Fatalf("disk failure receipt=%+v posts=%d", receipt, posts)
		}
	}
}
