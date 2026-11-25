package principal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDirectoryCause(t *testing.T) {
	var logs bytes.Buffer
	now := time.Now()
	var status, calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"emailAddresses":[]}`)
	}))
	defer srv.Close()
	read := DirectoryLookup(srv.Client(), srv.URL)
	var failure error
	r := New(Options{Logger: NewWarningLog(slog.New(slog.NewTextHandler(&logs, nil))), Now: func() time.Time { return now }, Lookup: func(ctx context.Context, id string) (string, error) {
		calls++
		if failure != nil {
			return "", failure
		}
		return read(ctx, id)
	}})
	status = http.StatusForbidden
	for i := range 30 {
		r.resolve(t.Context(), fmt.Sprintf("users/%d", 1+i%3))
		now = now.Add(11 * time.Minute)
	}
	if err := r.logger.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 30 || strings.Count(logs.String(), "level=WARN") != 1 {
		t.Errorf("lookups %d log %s", calls, logs.String())
	}
	status = http.StatusNotFound
	r.resolve(t.Context(), "users/4")
	status = http.StatusOK
	r.resolve(t.Context(), "users/5")
	r.resolve(t.Context(), "users/invalid")
	failure = context.DeadlineExceeded
	r.resolve(t.Context(), "users/6")
	failure = errors.New("example@example.com " + srv.URL + "/users/7")
	r.resolve(t.Context(), "users/7")
	if err := r.logger.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []string{"http_403", "not_found", "no_address", "timeout", "bad_id", "error"} {
		if strings.Count(logs.String(), "cause="+cause+"\n") != 1 {
			t.Errorf("cause %q: %s", cause, logs.String())
		}
	}
	if strings.Count(logs.String(), "level=WARN") != 6 {
		t.Errorf("log %s", logs.String())
	}
	for _, private := range []string{"users/", "example@example.com", srv.URL} {
		if strings.Contains(logs.String(), private) {
			t.Fatalf("private field %q in log", private)
		}
	}
}

func TestDirectoryCauseNoLogger(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	r := New(Options{Lookup: func(context.Context, string) (string, error) { return "", errors.New("fixture failure") }})
	if got := r.resolve(t.Context(), "users/1"); got != "" || buf.Len() != 0 {
		t.Fatalf("address=%q log=%q", got, buf.String())
	}
}
