package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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
