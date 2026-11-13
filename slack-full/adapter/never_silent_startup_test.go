package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
)

func TestNeverSilentStartsBeforeRegistration(t *testing.T) {
	dir := t.TempDir()
	var registrations, posts atomic.Int64
	gc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/extmsg/adapters") {
			registrations.Add(1)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer gc.Close()
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat.postMessage") {
			posts.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"ts":"123.456","user_id":"example-bot"}`)
	}))
	defer slack.Close()
	mail := filepath.Join(dir, "mail")
	bin := filepath.Join(dir, "fake-gc")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+mail+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-4 * time.Minute)
	e, err := neversilent.New(neversilent.Options{Enabled: true, Path: companyStateDirDefault(dir, "never-silent.json"),
		Now: func() time.Time { return old }, Post: func(context.Context, neversilent.Message, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Track(neversilent.Message{ID: "example-message", Conversation: "DEXAMPLE", Created: old}); err != nil {
		t.Fatal(err)
	}
	if err := e.Result("example-message", "unreachable", nil); err != nil {
		t.Fatal(err)
	}
	for _, restart := range []string{"first-restart", "second-restart"} {
		t.Run(restart, func(t *testing.T) {
			before := registrations.Load()
			p := startRegistrationProcess(t, gc.URL, "GC_CITY_PATH="+dir, "GC_BIN="+bin,
				"NEVER_SILENT=on", "NEVER_SILENT_OPERATOR_CONTEXT=example-operator", "SLACK_TEST_API_BASE="+slack.URL)
			p.waitHealth(t, "registering: attempt 1", 2*time.Second)
			time.Sleep(6 * time.Second)
			deadline := time.Now().Add(6 * time.Second)
			for time.Now().Before(deadline) {
				b, _ := os.ReadFile(mail)
				if posts.Load() == 1 && strings.Count(string(b), "--context example-operator") == 1 {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			b, _ := os.ReadFile(mail)
			mails := strings.Count(string(b), "--context example-operator")
			t.Logf("registration 503s=%d owner lines=%d operator mails=%d", registrations.Load()-before, posts.Load(), mails)
			if registrations.Load() == before {
				t.Error("registration was not attempted")
			}
			if posts.Load() != 1 || mails != 1 {
				t.Errorf("got owner lines=%d operator mails=%d; want 1, 1", posts.Load(), mails)
			}
			resp, err := p.client.Get("http://gc-service/healthz")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if !strings.HasPrefix(resp.Header.Get("X-GC-Health-Reason"), "registering:") {
				t.Error("adapter left registration barrier")
			}
		})
	}
}
