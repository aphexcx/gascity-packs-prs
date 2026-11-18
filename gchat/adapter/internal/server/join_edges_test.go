package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func TestJoinRejectsAmbiguousRequests(t *testing.T) {
	f := setupJoin(t)
	for _, body := range []string{
		`{"space":"spaces/room","space":"spaces/dm","request":true}`,
		`{"space":"spaces/room","request":true} {"space":"spaces/dm","request":true}`,
		`{"space":"spaces/room","request":null}`,
		`{"check":true,"space":""}`,
		`{"space":"spaces/room","request":true,"members":["users/222"]}`,
	} {
		res, err := http.Post(f.url+"/join", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("ambiguous request status=%d", res.StatusCode)
		}
	}
	if len(f.writes()) != 0 {
		t.Fatal("ambiguous request wrote")
	}
	f.kmu.Lock()
	defer f.kmu.Unlock()
	if len(f.scopes) != 0 {
		t.Fatal("ambiguous request minted permission")
	}
}

func TestJoinConcurrentApprovalIsUsedOnce(t *testing.T) {
	f := setupJoin(t)
	code := f.request(t, "spaces/room")
	yes := f.yes(code)
	body, _ := json.Marshal(map[string]any{"space": "spaces/room", "yes": yes})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := http.Post(f.url+"/join", "application/json", strings.NewReader(string(body)))
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Errorf("status %d", res.StatusCode)
			}
		}()
	}
	wg.Wait()
	if len(f.writes()) != 1 {
		t.Fatalf("writes=%d", len(f.writes()))
	}
}

func TestJoinDurableApprovalFailureStopsWrite(t *testing.T) {
	f := setupJoin(t)
	code := f.request(t, "spaces/room")
	yes := f.yes(code)
	f.mu.Lock()
	f.l.SetSaveGuard(func() error { return errors.New("injected save failure") })
	f.mu.Unlock()
	v := f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
	if v["result"] != "no_owner_yes" || v["recorded"] != false || len(f.writes()) != 0 {
		t.Fatalf("save refusal=%v", v)
	}
	// A failed copy-on-write must not mark the in-memory request as used.
	f.mu.Lock()
	f.l.SetSaveGuard(nil)
	f.mu.Unlock()
	v = f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
	if v["result"] != "joined" {
		t.Fatalf("failed save mutated pending approval: %v", v)
	}
}

func TestJoinApprovalCannotCrossIdentityOrUsePartialCode(t *testing.T) {
	for _, row := range []string{"identity", "partial_code", "bot", "anonymous", "same_time"} {
		t.Run(row, func(t *testing.T) {
			f := setupJoin(t)
			code := f.request(t, "spaces/room")
			yes := f.yes(code)
			if row == "identity" {
				f.rewriteRequest(t, func(r map[string]any) { r["identity"] = "users/987654321" })
			}
			if row == "same_time" {
				f.rewriteRequest(t, func(r map[string]any) {
					f.g.Configure(func(g *fakegoogle.Server) { g.Messages[yes].CreateTime = r["created_at"].(string) })
				})
			}
			f.g.Configure(func(g *fakegoogle.Server) {
				m := g.Messages[yes]
				switch row {
				case "partial_code":
					m.Text = "PREFIX" + code + "SUFFIX"
				case "bot":
					m.Sender.Type = "BOT"
				case "anonymous":
					m.Sender.IsAnonymous = true
				}
			})
			v := f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
			if v["result"] != "no_owner_yes" || len(f.writes()) != 0 {
				t.Fatalf("bad approval %s: %v", row, v)
			}
		})
	}
}

func TestJoinFailureNeedsFreshApproval(t *testing.T) {
	f := setupJoin(t)
	code := f.request(t, "spaces/room")
	yes := f.yes(code)
	f.g.Configure(func(g *fakegoogle.Server) {
		g.MembershipWriteError = &fakegoogle.MembershipError{HTTP: 403, Status: "PERMISSION_DENIED"}
	})
	input := map[string]any{"space": "spaces/room", "yes": yes}
	if v := f.join(t, input); v["result"] != "refused_invited" {
		t.Fatalf("%v", v)
	}
	f.g.Configure(func(g *fakegoogle.Server) { g.MembershipWriteError = nil })
	if v := f.join(t, input); v["result"] != "no_owner_yes" || len(f.writes()) != 1 {
		t.Fatalf("reused failed approval: %v", v)
	}
	fresh := f.request(t, "spaces/room")
	if v := f.join(t, map[string]any{"space": "spaces/room", "yes": f.yes(fresh)}); v["result"] != "joined" || len(f.writes()) != 2 {
		t.Fatalf("fresh approval: %v", v)
	}
}

func TestJoinPermissionFailureKeepsPendingRequest(t *testing.T) {
	f := setupJoin(t)
	code := f.request(t, "spaces/room")
	yes := f.yes(code)
	f.kmu.Lock()
	f.refusal = "scope_not_allowed"
	f.kmu.Unlock()
	if v := f.join(t, map[string]any{"space": "spaces/room", "request": true}); v["result"] != "permission_missing" {
		t.Fatalf("%v", v)
	}
	f.kmu.Lock()
	f.refusal = ""
	f.kmu.Unlock()
	v := f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
	if v["result"] != "joined" {
		t.Fatalf("permission refusal replaced existing request: %v", v)
	}
}

func TestJoinUnknownReadNeverAsksForApproval(t *testing.T) {
	f := setupJoin(t)
	f.g.Configure(func(g *fakegoogle.Server) {
		g.MembershipReadError = &fakegoogle.MembershipError{HTTP: 403, Status: "PERMISSION_DENIED", Detail: "do not disclose"}
	})
	v := f.join(t, map[string]any{"space": "spaces/room", "request": true})
	if v["result"] != "google_unknown" || v["code"] != nil {
		t.Fatalf("unreadable invitation: %v", v)
	}
	f.kmu.Lock()
	defer f.kmu.Unlock()
	if len(f.scopes) != 0 {
		t.Fatal("unconfirmed invitation requested permission")
	}
}

func TestJoinRecordEveryAttemptWithoutMessageText(t *testing.T) {
	f := setupJoin(t)
	code := f.request(t, "spaces/room")
	yes := f.yes(code)
	f.g.Configure(func(g *fakegoogle.Server) {
		g.Messages[yes].Text = "private owner words " + code
		g.Messages[yes].CreateTime = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
		g.Memberships["spaces/group/members/123456789"] = &api.Membership{State: "JOINED"}
	})
	f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
	f.join(t, map[string]any{"space": "spaces/group"})
	if len(operationLogLines(t, f.logs.String(), "join")) != 3 || strings.Contains(f.logs.String(), code) || strings.Contains(f.logs.String(), "private owner words") {
		t.Fatal("attempt audit omitted a refusal or exposed owner text")
	}
}

// Class: no command outcome may checkpoint an uninitialized ledger over the
// durable state. Cover every early return that previously reached the audit.
func TestJoinBeforeLedgerLoadPreservesExistingState(t *testing.T) {
	for _, row := range []string{"invalid", "check", "desktop", "unavailable"} {
		t.Run(row, func(t *testing.T) {
			f := setupJoin(t)
			f.mu.Lock()
			f.l.Counters["keep-existing-state"] = 37
			if err := f.l.Save(f.options.LedgerPath); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(f.options.LedgerPath)
			if err != nil {
				t.Fatal(err)
			}
			*f.l = *ledger.New()
			f.mu.Unlock()
			f.s.SetHealth(Health{State: "initializing", Reason: "initialization in progress"})
			input := map[string]any{"space": "spaces/room", "request": true}
			switch row {
			case "invalid":
				input = map[string]any{}
			case "check":
				input = map[string]any{"check": true}
			case "desktop":
				f.cfg.Secrets.TokenSource = config.TokenSourceOAuth
				f.s.SetConfig(*f.cfg)
			}
			status, _, _ := f.call(t, "/join", input)
			after, err := os.ReadFile(f.options.LedgerPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("join overwrote the existing ledger before it was loaded")
			}
			if status != 503 {
				t.Errorf("startup status=%d, want 503", status)
			}
			if len(f.writes()) != 0 {
				t.Error("startup attempt wrote membership")
			}
			f.kmu.Lock()
			defer f.kmu.Unlock()
			if len(f.scopes) != 0 {
				t.Error("startup attempt requested a token")
			}
		})
	}
}

func TestJoinAmbiguousWritesNeverClaimNothingChanged(t *testing.T) {
	for _, lost := range []bool{true, false} {
		t.Run(map[bool]string{true: "lost_response", false: "unknown_status"}[lost], func(t *testing.T) {
			f := setupJoin(t)
			code := f.request(t, "spaces/room")
			yes := f.yes(code)
			f.g.Configure(func(g *fakegoogle.Server) {
				if lost {
					g.DropNextResponse = true
				} else {
					g.MembershipWriteError = &fakegoogle.MembershipError{HTTP: 409, Status: "ALREADY_EXISTS"}
				}
			})
			v := f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
			message, _ := v["message"].(string)
			if strings.Contains(message, "Nothing changed") || !strings.Contains(message, "outcome is unknown") || !strings.Contains(message, "read membership before any new write") {
				t.Errorf("ambiguous write misreported: %s", message)
			}
			if lost {
				f.g.Configure(func(g *fakegoogle.Server) {
					if g.Memberships["spaces/room/members/123456789"].State != "JOINED" {
						t.Error("lost response must model an accepted join")
					}
				})
			}
		})
	}
}

// Pin the approved privacy boundary through both normal message paths: join
// records contain only a code digest, while ordinary message retention stays
// unchanged. Approval codes are one-time, room-bound and owner-bound.
func TestJoinWorkflowKeepsCodesOutOfJoinRecords(t *testing.T) {
	f := setupJoin(t)
	code := f.request(t, "spaces/room")
	receipt := f.send(t, wire.PublishRequest{Conversation: conversation("spaces/dm"), Text: "Please approve this room with " + code, IdempotencyKey: "owner-ask"})
	if !receipt.Delivered {
		t.Fatal("owner ask was not delivered")
	}
	yes := f.yes(code)
	var m *api.Message
	f.g.Configure(func(g *fakegoogle.Server) { copy := *g.Messages[yes]; m = &copy })
	p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: f.l, LedgerMu: f.mu, LedgerPath: f.options.LedgerPath, Chat: f.options.Chat, GC: f.options.GC, IdentityUserID: fakegoogle.UserID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.HandleChange(t.Context(), puller.Change{Kind: "message.created", Space: "spaces/dm", Message: m}); err != nil {
		t.Fatal(err)
	}
	if v := f.join(t, map[string]any{"space": "spaces/room", "yes": yes}); v["result"] != "joined" {
		t.Fatalf("approval failed: %v", v)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(f.l)
	doc := decode[map[string]json.RawMessage](t, b)
	for _, field := range []string{"join_requests", "join_attempts"} {
		if bytes.Contains(doc[field], []byte(code)) {
			t.Fatalf("%s contains plaintext code", field)
		}
	}
	for _, field := range []string{"sends", "changes"} {
		if !bytes.Contains(doc[field], []byte(code)) {
			t.Fatalf("existing %s message retention changed", field)
		}
	}
	if strings.Contains(f.logs.String(), code) {
		t.Fatal("approval code reached command logs")
	}
}

// Class: the join reads its identity once, then waits on the owner's message,
// the key holder and the ledger. An identity that turns conflicted during any
// of those waits must not spend the owner's yes, and must never reach the one
// membership write. Each row holds one step, injects the conflict there, and
// lets the join finish.
func TestJoinRechecksIdentityBeforeConsumingAndWriting(t *testing.T) {
	const unspent = "The identity is not known safely yet. Nothing changed; try again after the adapter learns its identity."
	const spent = "The identity is not known safely yet. Nothing was sent to Google, but this owner's yes is now used; request approval again after the adapter learns its identity."
	for _, row := range []struct {
		name    string
		used    bool
		message string
	}{
		{"owner_yes_check", false, unspent},
		{"mint", false, unspent},
		{"prepare", false, unspent},
		{"consumption_save", true, spent},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := setupJoin(t)
			code := f.request(t, "spaces/room")
			yes := f.yes(code)
			entered, release := make(chan struct{}, 1), make(chan struct{})
			free := sync.OnceFunc(func() { close(release) })
			t.Cleanup(free) // A failed row must not leave the server's handler held.
			var once sync.Once
			hold := func() { once.Do(func() { entered <- struct{}{}; <-release }) }
			switch row.name {
			case "owner_yes_check":
				transport := heldGET{path: "/v1/" + yes, hold: hold}
				c, err := chat.New(context.Background(), &http.Client{Transport: transport}, f.cfg.Endpoints)
				if err != nil {
					t.Fatal(err)
				}
				if err = c.SetIdentityUserID(fakegoogle.UserID); err != nil {
					t.Fatal(err)
				}
				f.s.SetClients(c, f.options.GC)
			case "mint", "prepare":
				f.cfg.Secrets.KeyholderSocket = heldJoinMint(t, hold)
				f.s.SetConfig(*f.cfg)
			case "consumption_save":
				f.mu.Lock()
				f.l.SetSaveGuard(func() error { hold(); return nil })
				f.mu.Unlock()
			}
			body, _ := json.Marshal(map[string]any{"space": "spaces/room", "yes": yes})
			type answer struct {
				status int
				body   []byte
				err    error
			}
			done := make(chan answer, 1)
			go func() {
				res, err := http.Post(f.url+"/join", "application/json", bytes.NewReader(body))
				if err != nil {
					done <- answer{err: err}
					return
				}
				defer res.Body.Close()
				b, err := io.ReadAll(res.Body)
				done <- answer{res.StatusCode, b, err}
			}()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the join never reached the held step")
			}
			if row.name == "prepare" {
				// The prepare is local and has no seam. Hold the next step it must
				// pass, the consumption update's ledger lock, past the conflict.
				f.mu.Lock()
			}
			c, _ := f.s.clients()
			if row.name == "prepare" {
				free()
				joinAtLedgerLock(t)
			}
			if err := c.SetIdentityUserID("users/999"); err == nil {
				t.Error("the identity conflict was not injected")
			}
			if _, err := c.JoinIdentity(); err == nil {
				t.Error("JoinIdentity still answers after the conflict")
			}
			if row.name == "prepare" {
				f.mu.Unlock()
			}
			free()
			var a answer
			select {
			case a = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the join did not answer")
			}
			if a.err != nil || a.status != http.StatusOK {
				t.Fatalf("join status=%d err=%v: %s", a.status, a.err, a.body)
			}
			v := decode[map[string]any](t, a.body)
			if w := f.writes(); len(w) != 0 {
				t.Errorf("membership write after the identity conflict: %v", w)
			}
			if v["result"] != "no_owner_yes" || v["message"] != row.message {
				t.Errorf("answer=%v, want no_owner_yes: %s", v, row.message)
			}
			f.mu.Lock()
			p := f.l.JoinRequests["spaces/room"]
			f.mu.Unlock()
			if p == nil || p.Used != row.used {
				t.Errorf("approval %+v, want used=%v", p, row.used)
			}
		})
	}
}

// joinAtLedgerLock waits until the join's consumption update waits for the
// ledger lock the test holds, so the mint and the prepare are behind it. It
// never fails fatally: the caller still holds that lock.
func joinAtLedgerLock(t *testing.T) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, ").lockLedger(") && strings.Contains(g, ").update(") && strings.Contains(g, ").runJoin(") {
				return
			}
		}
	}
	t.Error("the join never reached the consumption update's ledger lock")
}

// heldGET holds the first GET of one path, then forwards every request.
type heldGET struct {
	path string
	hold func()
}

func (h heldGET) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, h.path) {
		h.hold()
	}
	return http.DefaultTransport.RoundTrip(r)
}

// heldJoinMint serves a second key holder whose join-scope mint is held.
func heldJoinMint(t *testing.T, hold func()) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gchat-join-held-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "key.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			_ = json.NewEncoder(w).Encode(map[string]string{"subject": "agent@example.com"})
			return
		}
		var input struct {
			Scopes []string `json:"scopes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		if reflect.DeepEqual(input.Scopes, []string{joinScopeTest}) {
			hold()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "join-secret-never-log", "expires_at": time.Now().Add(time.Hour).UnixMilli()})
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket
}
