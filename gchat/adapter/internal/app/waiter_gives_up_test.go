package app

// PR 109 round 3, Google Chat: a react call waiting for its turn ends with
// its caller.
//
// THE CLASS: with ACK_REACTION on, the session's calls on one reaction take
// turns (ack.Service.HoldReaction through chat.Client.ArrivalHold), so a call
// can wait behind a holder whose Google answer is slow. A waiting call whose
// caller has given up must leave the queue at once and never reach Google:
// no act and no mark for it, the holder and a call queued behind both go as
// if it had never been made, and the turn is not kept.
//
// ONE TEST, THREE WAYS THE CALLER GIVES UP: its request's context is
// cancelled, its request's context reaches its deadline, or its HTTP client
// times out through a reverse proxy in front of the adapter's unix socket
// (the gc API's proxy_process path, with app.go's server settings). Each row
// runs with ACK_REACTION=eyes: the session's eyes on A (s1); its removal
// (r1), applied by Google with the answer held; the waiter (w) and a third
// eyes (s3) queued behind r1; w's caller gives up; r1's answer is released;
// then a later eyes (s4) must go at once. The oracle is the same steps
// without w.
//
// THE LIMIT IT LEAVES: with ACK_REACTION unset the same call is made at once.
// TestReactVerbWaiterGivenUpIsAMissedCount shows which way that errs.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// waiterPrompt is how soon a waiting call must return once its caller has
// given up, and how soon a call on a free turn must return.
const waiterPrompt = 2 * time.Second

// waiterDeadline is the caller's own bound in the deadline and proxy rows.
const waiterDeadline = 600 * time.Millisecond

// sendThroughProxy is the session's call as it travels live: an HTTP client
// with its own timeout, the gc API's reverse proxy, and the adapter's server
// on a unix socket (app.go's settings). handled closes when the adapter's
// handler returns; gaveUp closes when the client has its answer or its error,
// which is then in *out.
func (r *heldChat) sendThroughProxy(message, emoji string, timeout time.Duration, out *string) (handled, gaveUp chan struct{}) {
	t := r.t
	t.Helper()
	dir, err := os.MkdirTemp("", "gp-react")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "adapter.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	serviceCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	s := r.s
	handled = make(chan struct{})
	var once sync.Once
	backend := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer once.Do(func() { close(handled) })
		s.ServeHTTP(w, req)
	}), BaseContext: func(net.Listener) context.Context { return serviceCtx }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 25 * time.Second, IdleTimeout: time.Minute}
	go func() { _ = backend.Serve(ln) }()
	t.Cleanup(func() { _ = backend.Close() })
	target := &url.URL{Scheme: "http", Host: "gc-service"}
	proxy := &httputil.ReverseProxy{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "service unavailable: "+err.Error(), http.StatusBadGateway)
		},
		Rewrite: func(req *httputil.ProxyRequest) {
			req.SetURL(target)
			req.Out.URL.Path = "/react"
			req.Out.URL.RawPath = "/react"
			req.Out.Host = ""
		},
	}
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)
	body, _ := json.Marshal(map[string]any{"message": message, "emoji": emoji, "session_id": "example-session"})
	gaveUp = make(chan struct{})
	go func() {
		defer close(gaveUp)
		resp, err := (&http.Client{Timeout: timeout}).Post(front.URL+"/v0/city/examplecity/svc/gchat/react", "application/json", strings.NewReader(string(body)))
		var timedOut net.Error
		switch {
		case errors.As(err, &timedOut) && timedOut.Timeout():
			*out = "client timed out"
			return
		case err != nil:
			*out = "client: " + err.Error()
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		*out = strconv.Itoa(resp.StatusCode)
	}()
	return handled, gaveUp
}

// waiterKinds names each state file by its kind (ack-session, ack-placed,
// ack-attempts): every file of the row is about eyes on A, and its name is a
// hash of A, which differs from rig to rig.
func waiterKinds(files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = filepath.Dir(f)
	}
	return out
}

// waiterRun plays the row with ACK_REACTION=eyes and the waiter given up the
// named way ("" leaves the waiter out: the oracle). It returns the row's trace
// and what the waiter's caller got.
func (r *heldChat) waiterRun(way string) ([]string, string) {
	t := r.t
	t.Helper()
	r.start("eyes")
	a := r.sessionFirstNext()
	var s1, rm, s3, s4, wCode int
	var w string
	if !waitDone(r.send(context.Background(), a, round4Eyes, false, &s1), 10*time.Second) {
		t.Fatal("the session's first eyes never returned")
	}
	hold := r.arm("delete")
	rmDone := r.send(context.Background(), a, round4Eyes, true, &rm)
	if !waitDone(hold.applied, 10*time.Second) {
		t.Fatalf("the removal never reached Google; Google applied %s", r.front.record())
	}
	acts, files := r.acts(), strings.Join(r.ackFiles(), ",")

	// wDone closes when the waiter's handler returns; giveUp returns once its
	// caller has given up.
	var wDone chan struct{}
	var giveUp func()
	switch way {
	case "":
	case "cancelled":
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		wDone = r.send(ctx, a, round4Eyes, false, &wCode)
		giveUp = cancel
	case "deadline":
		ctx, cancel := context.WithTimeout(context.Background(), waiterDeadline)
		t.Cleanup(cancel)
		wDone = r.send(ctx, a, round4Eyes, false, &wCode)
		giveUp = func() { <-ctx.Done() }
	case "client-gives-up-through-the-proxy":
		var gaveUp chan struct{}
		wDone, gaveUp = r.sendThroughProxy(a, round4Eyes, waiterDeadline, &w)
		giveUp = func() {
			if !waitDone(gaveUp, waiterDeadline+waiterPrompt) {
				t.Fatal("the client never gave up")
			}
		}
	default:
		t.Fatalf("unknown way %q", way)
	}
	if wDone != nil && waitDone(wDone, 150*time.Millisecond) {
		t.Fatalf("%s: the waiter did not wait for the turn r1 holds", way)
	}
	s3Done := r.send(context.Background(), a, round4Eyes, false, &s3)
	if waitDone(s3Done, 150*time.Millisecond) {
		t.Fatalf("the third eyes did not wait for the turn r1 holds: %d", s3)
	}
	if wDone != nil {
		giveUp()
		if !waitDone(wDone, waiterPrompt) {
			t.Errorf("%s: the waiting call had not returned %s after its caller gave up, with the holder's answer still held", way, waiterPrompt)
		} else {
			if got, want := r.front.record(), "create:200 | delete:200"; got != want {
				t.Errorf("%s: before the holder's answer, Google applied %s, want %s", way, got, want)
			}
			if got := r.acts(); got != acts {
				t.Errorf("%s: acts=%d after the waiter returned, want %d: an act was recorded for it", way, got, acts)
			}
			if got := strings.Join(r.ackFiles(), ","); got != files {
				t.Errorf("%s: state files [%s] after the waiter returned, want [%s]", way, got, files)
			}
		}
	}
	hold.free()
	for name, done := range map[string]chan struct{}{"r1": rmDone, "s3": s3Done, "w": wDone} {
		if done != nil && !waitDone(done, 15*time.Second) {
			t.Fatalf("%s never returned after the holder's answer; Google applied %s", name, r.front.record())
		}
	}
	r.service.Wait()
	// The turn is not kept: a later call on the same reaction goes at once.
	s4Done := r.send(context.Background(), a, round4Eyes, false, &s4)
	if !waitDone(s4Done, waiterPrompt) {
		t.Errorf("%s: a later call on the same reaction waited: the turn was kept", way)
		if !waitDone(s4Done, 15*time.Second) {
			t.Fatal("the later call never returned")
		}
	}
	if way == "cancelled" || way == "deadline" {
		w = strconv.Itoa(wCode)
	}
	return []string{
		"google=" + r.front.record(),
		"acts=" + strconv.Itoa(r.acts()),
		"s1=" + strconv.Itoa(s1), "r1=" + strconv.Itoa(rm), "s3=" + strconv.Itoa(s3), "s4=" + strconv.Itoa(s4),
		"state files=[" + strings.Join(waiterKinds(r.ackFiles()), ",") + "]",
	}, w
}

func TestReactVerbWaiterEndsWithItsCaller(t *testing.T) {
	for _, row := range []struct{ way, caller string }{
		{"cancelled", "502"},
		{"deadline", "502"},
		{"client-gives-up-through-the-proxy", "client timed out"},
	} {
		t.Run(row.way, func(t *testing.T) {
			oracle, _ := newHeldChat(t).waiterRun("")
			got, caller := newHeldChat(t).waiterRun(row.way)
			t.Logf("without the waiter: %s", strings.Join(oracle, " ; "))
			t.Logf("waiter %-34s %s ; its caller got %s", row.way+":", strings.Join(got, " ; "), caller)
			if want := "google=create:200 | delete:200 | create:200 | create:409"; oracle[0] != want {
				t.Errorf("the oracle did not reach its order: %s, want %s", oracle[0], want)
			}
			if strings.Join(got, "\n") != strings.Join(oracle, "\n") {
				t.Errorf("the holder and the calls around the given-up waiter differ from the same steps without it:\n got: %s\nwant: %s", strings.Join(got, " ; "), strings.Join(oracle, " ; "))
			}
			if caller != row.caller {
				t.Errorf("the waiter's caller got %s, want %s", caller, row.caller)
			}
		})
	}
}

// waiterLimitRun is the limit's row with ACK_REACTION set to mode: the
// session's eyes on A (s1), its removal held, A admitted (with the switch set,
// the acknowledgement's eyes), another owner message B, then the waiter (w),
// whose caller gives up while r1's answer is held.
func (r *heldChat) waiterLimitRun(mode string) []string {
	t := r.t
	t.Helper()
	r.start(mode)
	a := r.sessionFirstNext()
	var s1, rm, w int
	if !waitDone(r.send(context.Background(), a, round4Eyes, false, &s1), 10*time.Second) {
		t.Fatal("the session's first eyes never returned")
	}
	hold := r.arm("delete")
	rmDone := r.send(context.Background(), a, round4Eyes, true, &rm)
	if !waitDone(hold.applied, 10*time.Second) {
		t.Fatalf("the removal never reached Google; Google applied %s", r.front.record())
	}
	if got := r.human(); got != a {
		t.Errorf("admitted %q, want %q", got, a)
	}
	// B arrives after s1's act: bring the rig's clock up to the wall clock
	// the react verb's acts are on, as round 2's "message" step does.
	r.clock.mu.Lock()
	r.clock.now = time.Now().UTC()
	r.clock.mu.Unlock()
	r.human()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wDone := r.send(ctx, a, round4Eyes, false, &w)
	// With ACK_REACTION unset it has returned; set, it waits for r1's turn.
	waitDone(wDone, heldPending)
	cancel()
	if !waitDone(wDone, waiterPrompt) {
		t.Errorf("ACK_REACTION=%q: the waiting call had not returned %s after its caller gave up", mode, waiterPrompt)
	}
	hold.free()
	if !waitDone(rmDone, 15*time.Second) || !waitDone(wDone, 15*time.Second) {
		t.Fatalf("a call never returned; Google applied %s", r.front.record())
	}
	r.service.Wait()
	return []string{
		"google=" + r.front.record(),
		"acts=" + strconv.Itoa(r.acts()),
		"s1=" + strconv.Itoa(s1), "r1=" + strconv.Itoa(rm), "w=" + strconv.Itoa(w),
		"owner lines=" + strconv.Itoa(r.due()),
	}
}

// THE PARITY LIMIT, from a test: a call whose caller gives up while it waits
// is never made, where with ACK_REACTION unset it is made at once. The error
// is a missed count, so never-silent posts a notice the unset run does not
// (an extra notice, the false-alarm side), never a hidden failure; the waiter
// is answered 502, which reaches no one live (its caller has gone).
func TestReactVerbWaiterGivenUpIsAMissedCount(t *testing.T) {
	off := newHeldChat(t).waiterLimitRun("")
	on := newHeldChat(t).waiterLimitRun("eyes")
	t.Logf("ACK_REACTION unset: %s", strings.Join(off, " ; "))
	t.Logf("ACK_REACTION=eyes:  %s", strings.Join(on, " ; "))
	want := map[string][]string{
		"off": {"google=create:200 | delete:200 | create:200", "acts=2", "s1=200", "r1=200", "w=200", "owner lines=0"},
		"on":  {"google=create:200 | delete:200 | create:200 | create:200", "acts=1", "s1=200", "r1=200", "w=502", "owner lines=1"},
	}
	if strings.Join(off, "\n") != strings.Join(want["off"], "\n") {
		t.Errorf("ACK_REACTION unset:\n got: %s\nwant: %s", strings.Join(off, " ; "), strings.Join(want["off"], " ; "))
	}
	if strings.Join(on, "\n") != strings.Join(want["on"], "\n") {
		t.Errorf("ACK_REACTION=eyes:\n got: %s\nwant: %s", strings.Join(on, " ; "), strings.Join(want["on"], " ; "))
	}
}
