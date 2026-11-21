package main

// PR 109 round 3, Slack: a react call waiting for its turn ends with its
// caller.
//
// THE CLASS: with ACK_REACTION on, the session's calls on one reaction take
// turns (ack.Service.HoldReaction), so a call can wait behind a holder whose
// Slack answer is slow. A waiting call whose caller has given up must leave
// the queue at once and never reach Slack: no act, no journal row and no
// mark for it, the holder and a call queued behind both go as if it had
// never been made, and the turn is not kept.
//
// ONE TEST, THREE WAYS THE CALLER GIVES UP: its request's context is
// cancelled, its request's context reaches its deadline, or its HTTP client
// times out through a reverse proxy in front of the adapter's unix socket
// (the gc API's proxy_process path). Each row runs with ACK_REACTION=eyes:
// the session's eyes on A (s1); its removal (r1), applied by Slack with the
// answer held; the waiter (w) and a third eyes (s3) queued behind r1; w's
// caller gives up; r1's answer is released; then a later eyes (s4) must go
// at once. The oracle is the same steps without w.
//
// THE LIMIT IT LEAVES: with ACK_REACTION unset the same call is made at once.
// TestReactVerbWaiterGivenUpIsAMissedCount shows which way that errs.

import (
	"context"
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

func waiterBody(ts, emoji string, remove bool) string {
	body := `{"conversation":{"conversation_id":"DEXAMPLE"},"message_id":"` + ts + `","emoji":"` + emoji + `","session_id":"example-session"`
	if remove {
		body += `,"remove":true`
	}
	return body + "}"
}

// sendContext is the session's call through the react verb on ctx, in a
// goroutine; done closes when the handler returns, with its body in *out.
func (r *heldSlackRig) sendContext(ctx context.Context, ts, emoji string, remove bool, out *string) chan struct{} {
	body := waiterBody(ts, emoji, remove)
	cfg := r.cfg
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := httptest.NewRecorder()
		handleReact(cfg)(w, httptest.NewRequest(http.MethodPost, "/react", strings.NewReader(body)).WithContext(ctx))
		*out = strings.TrimSpace(w.Body.String())
	}()
	return done
}

// sendThroughProxy is the session's call as it travels live: an HTTP client
// with its own timeout, the gc API's reverse proxy, and the adapter's
// internal listener on a unix socket (main.go's internalSrv settings).
// handled closes when the adapter's handler returns; gaveUp closes when the
// client has its answer or its error, which is then in *out.
func (r *heldSlackRig) sendThroughProxy(ts, emoji string, timeout time.Duration, out *string) (handled, gaveUp chan struct{}) {
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
	cfg := r.cfg
	handled = make(chan struct{})
	var once sync.Once
	backend := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer once.Do(func() { close(handled) })
		handleReact(cfg)(w, req)
	})}
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
	gaveUp = make(chan struct{})
	go func() {
		defer close(gaveUp)
		resp, err := (&http.Client{Timeout: timeout}).Post(front.URL+"/v0/city/examplecity/svc/slack/react", "application/json", strings.NewReader(waiterBody(ts, emoji, false)))
		var timedOut net.Error
		switch {
		case errors.As(err, &timedOut) && timedOut.Timeout():
			*out = "client timed out"
			return
		case err != nil:
			*out = "client: " + err.Error()
			return
		}
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		*out = strconv.Itoa(resp.StatusCode) + " " + strings.TrimSpace(string(data))
	}()
	return handled, gaveUp
}

// waiterRun plays the row with ACK_REACTION=eyes and the waiter given up the
// named way ("" leaves the waiter out: the oracle). It returns the row's trace
// and what the waiter's caller got.
func (r *heldSlackRig) waiterRun(way string) ([]string, string) {
	t := r.t
	t.Helper()
	r.start("eyes")
	a := slackTimestamp(r.now)
	var s1, rm, s3, s4, w string
	if !waitDone(r.send(a, "eyes", false, &s1), 10*time.Second) {
		t.Fatal("the session's first eyes never returned")
	}
	hold := r.arm("remove")
	rmDone := r.send(a, "eyes", true, &rm)
	if !waitDone(hold.applied, 10*time.Second) {
		t.Fatalf("the removal never reached Slack; Slack applied %s", r.front.record())
	}
	acts, journal, files := r.acts(), strings.Join(r.journaled(), ","), strings.Join(r.ackFiles(), ",")

	// wDone closes when the waiter's handler returns; giveUp returns once its
	// caller has given up.
	var wDone chan struct{}
	var giveUp func()
	switch way {
	case "":
	case "cancelled":
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		wDone = r.sendContext(ctx, a, "eyes", false, &w)
		giveUp = cancel
	case "deadline":
		ctx, cancel := context.WithTimeout(context.Background(), waiterDeadline)
		t.Cleanup(cancel)
		wDone = r.sendContext(ctx, a, "eyes", false, &w)
		giveUp = func() { <-ctx.Done() }
	case "client-gives-up-through-the-proxy":
		var gaveUp chan struct{}
		wDone, gaveUp = r.sendThroughProxy(a, "eyes", waiterDeadline, &w)
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
	s3Done := r.send(a, "eyes", false, &s3)
	if waitDone(s3Done, 150*time.Millisecond) {
		t.Fatalf("the third eyes did not wait for the turn r1 holds: %s", s3)
	}
	if wDone != nil {
		giveUp()
		if !waitDone(wDone, waiterPrompt) {
			t.Errorf("%s: the waiting call had not returned %s after its caller gave up, with the holder's answer still held", way, waiterPrompt)
		} else {
			if got, want := r.front.record(), "add:ok | remove:ok"; got != want {
				t.Errorf("%s: before the holder's answer, Slack applied %s, want %s", way, got, want)
			}
			if got := r.acts(); got != acts {
				t.Errorf("%s: acts=%d after the waiter returned, want %d: an act was recorded for it", way, got, acts)
			}
			if got := strings.Join(r.journaled(), ","); got != journal {
				t.Errorf("%s: journal [%s] after the waiter returned, want [%s]", way, got, journal)
			}
			if got := strings.Join(r.ackFiles(), ","); got != files {
				t.Errorf("%s: state files [%s] after the waiter returned, want [%s]", way, got, files)
			}
		}
	}
	hold.free()
	for name, done := range map[string]chan struct{}{"r1": rmDone, "s3": s3Done, "w": wDone} {
		if done != nil && !waitDone(done, 15*time.Second) {
			t.Fatalf("%s never returned after the holder's answer; Slack applied %s", name, r.front.record())
		}
	}
	r.cfg.arrival.Wait()
	// The turn is not kept: a later call on the same reaction goes at once.
	s4Done := r.send(a, "eyes", false, &s4)
	if !waitDone(s4Done, waiterPrompt) {
		t.Errorf("%s: a later call on the same reaction waited: the turn was kept", way)
		if !waitDone(s4Done, 15*time.Second) {
			t.Fatal("the later call never returned")
		}
	}
	return []string{
		"slack=" + r.front.record(),
		"acts=" + strconv.Itoa(r.acts()),
		"journal=[" + strings.Join(r.journaled(), ",") + "]",
		"s1=" + s1, "r1=" + rm, "s3=" + s3, "s4=" + s4,
		"state files=[" + strings.Join(waiterKinds(r.ackFiles()), ",") + "]",
	}, w
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

func TestReactVerbWaiterEndsWithItsCaller(t *testing.T) {
	for _, row := range []struct{ way, caller string }{
		{"cancelled", `{"delivered":false,"failure_kind":"transient"}`},
		{"deadline", `{"delivered":false,"failure_kind":"transient"}`},
		{"client-gives-up-through-the-proxy", "client timed out"},
	} {
		t.Run(row.way, func(t *testing.T) {
			oracle, _ := newHeldSlackRig(t).waiterRun("")
			got, caller := newHeldSlackRig(t).waiterRun(row.way)
			t.Logf("without the waiter: %s", strings.Join(oracle, " ; "))
			t.Logf("waiter %-34s %s ; its caller got %s", row.way+":", strings.Join(got, " ; "), caller)
			if want := "slack=add:ok | remove:ok | add:ok | add:already_reacted"; oracle[0] != want {
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
func (r *heldSlackRig) waiterLimitRun(mode string) []string {
	t := r.t
	t.Helper()
	r.start(mode)
	a := slackTimestamp(r.now)
	var s1, rm, w string
	if !waitDone(r.send(a, "eyes", false, &s1), 10*time.Second) {
		t.Fatal("the session's first eyes never returned")
	}
	hold := r.arm("remove")
	rmDone := r.send(a, "eyes", true, &rm)
	if !waitDone(hold.applied, 10*time.Second) {
		t.Fatalf("the removal never reached Slack; Slack applied %s", r.front.record())
	}
	r.human(a)
	r.human(slackTimestamp(time.Now()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wDone := r.sendContext(ctx, a, "eyes", false, &w)
	// With ACK_REACTION unset it has returned; set, it waits for r1's turn.
	waitDone(wDone, heldPending)
	cancel()
	if !waitDone(wDone, waiterPrompt) {
		t.Errorf("ACK_REACTION=%q: the waiting call had not returned %s after its caller gave up", mode, waiterPrompt)
	}
	hold.free()
	if !waitDone(rmDone, 15*time.Second) || !waitDone(wDone, 15*time.Second) {
		t.Fatalf("a call never returned; Slack applied %s", r.front.record())
	}
	r.cfg.arrival.Wait()
	return []string{
		"slack=" + r.front.record(),
		"acts=" + strconv.Itoa(r.acts()),
		"journal=[" + strings.Join(r.journaled(), ",") + "]",
		"s1=" + s1, "r1=" + rm, "w=" + w,
		"owner lines=" + strconv.Itoa(r.due()),
	}
}

// THE PARITY LIMIT, from a test: a call whose caller gives up while it waits
// is never made, where with ACK_REACTION unset it is made at once. The error
// is a missed count, so never-silent posts a notice the unset run does not
// (an extra notice, the false-alarm side), never a hidden failure; the waiter
// is answered transient, which reaches no one live (its caller has gone).
func TestReactVerbWaiterGivenUpIsAMissedCount(t *testing.T) {
	off := newHeldSlackRig(t).waiterLimitRun("")
	on := newHeldSlackRig(t).waiterLimitRun("eyes")
	t.Logf("ACK_REACTION unset: %s", strings.Join(off, " ; "))
	t.Logf("ACK_REACTION=eyes:  %s", strings.Join(on, " ; "))
	want := map[string][]string{
		"off": {"slack=add:ok | remove:ok | add:ok", "acts=2", "journal=[eyes:counted,eyes:counted]",
			`s1={"delivered":true}`, `r1={"delivered":true}`, `w={"delivered":true}`, "owner lines=0"},
		"on": {"slack=add:ok | remove:ok | add:ok | add:ok", "acts=1", "journal=[eyes:counted]",
			`s1={"delivered":true}`, `r1={"delivered":true}`, `w={"delivered":false,"failure_kind":"transient"}`, "owner lines=1"},
	}
	if strings.Join(off, "\n") != strings.Join(want["off"], "\n") {
		t.Errorf("ACK_REACTION unset:\n got: %s\nwant: %s", strings.Join(off, " ; "), strings.Join(want["off"], " ; "))
	}
	if strings.Join(on, "\n") != strings.Join(want["on"], "\n") {
		t.Errorf("ACK_REACTION=eyes:\n got: %s\nwant: %s", strings.Join(on, " ; "), strings.Join(want["on"], " ; "))
	}
}
