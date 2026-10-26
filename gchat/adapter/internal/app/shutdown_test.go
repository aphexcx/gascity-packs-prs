package app

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	api "google.golang.org/api/chat/v1"
)

// A blocked request body ignores context cancellation until its connection is
// closed. This exercises the actual one-second HTTP drain bound.
func TestShutdownCheckpointsBeforeDrainingStalledRequestBody(t *testing.T) {
	for _, releaseBody := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded", true: "post-drain-save"}[releaseBody], func(t *testing.T) {
			f := newFixture(t, true)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(context.Context, string, string) error { return nil }})
			}()
			stopped := false
			t.Cleanup(func() {
				cancel()
				if !stopped {
					<-done
				}
			})
			f.health(t, "ready", 5*time.Second)
			conn, err := net.Dial("unix", f.cfg.Secrets.ServiceSocket)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
				t.Fatal(err)
			}
			_, err = io.WriteString(conn, "POST /publish HTTP/1.1\r\nHost: adapter\r\nContent-Length: 1000\r\nContent-Type: application/json\r\nExpect: 100-continue\r\n\r\n")
			if err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			line, err := reader.ReadString('\n')
			if err != nil || !strings.Contains(line, "100 Continue") {
				t.Fatalf("handler not reading body: %q %v", line, err)
			}
			before, err := os.Stat(f.cfg.Endpoints.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			cancel()
			waitFor(t, 500*time.Millisecond, func() bool {
				now, e := os.Stat(f.cfg.Endpoints.StateFile)
				return e == nil && !os.SameFile(before, now)
			})
			select {
			case err := <-done:
				stopped = true
				t.Fatalf("drain ended before body was released or timeout: %v", err)
			default:
			}
			checkpoint, err := os.Stat(f.cfg.Endpoints.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			if releaseBody {
				_ = conn.Close()
			}
			select {
			case err := <-done:
				stopped = true
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Until(start.Add(1500 * time.Millisecond))):
				t.Fatal("HTTP body prevented bounded drain")
			}
			if !releaseBody && time.Since(start) < 900*time.Millisecond {
				t.Fatal("request body did not hold the drain open")
			}
			if releaseBody {
				final, err := os.Stat(f.cfg.Endpoints.StateFile)
				if err != nil || os.SameFile(checkpoint, final) {
					t.Fatalf("post-drain checkpoint missing: %v", err)
				}
			}
		})
	}
}

func TestShutdownSavesBeforeDrainingStuckPublish(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, fakegoogle.UserID)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
	})
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	f.intercept = func(r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
			entered <- struct{}{}
			<-release
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(context.Context, string, string) error { return nil }})
	}()
	stopped := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		if !stopped {
			<-done
		}
	})
	f.health(t, "ready", 5*time.Second)
	client := *f.client
	client.Timeout = 10 * time.Second
	published := make(chan struct{})
	go func() {
		defer close(published)
		resp, err := client.Post("http://adapter/publish", "application/json", strings.NewReader(`{"idempotency_key":"shutdown-example","conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"default","conversation_id":"spaces/room","kind":"room"},"text":"hello"}`))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("publish did not start")
	}
	before, err := os.Stat(f.cfg.Endpoints.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cancel()
	waitFor(t, 500*time.Millisecond, func() bool {
		current, e := os.Stat(f.cfg.Endpoints.StateFile)
		return e == nil && !os.SameFile(before, current)
	})
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Until(start.Add(1500 * time.Millisecond))):
		t.Fatal("shutdown exceeded 1.5 seconds")
	}
	saved, err := ledger.Load(f.cfg.Endpoints.StateFile, true)
	if err != nil || saved.IdentityUserID != fakegoogle.UserID || saved.Sends["shutdown-example"] == nil {
		t.Fatalf("shutdown ledger lost intent/identity: %v %+v", err, saved)
	}
	releaseOnce.Do(func() { close(release) })
	<-published
}
