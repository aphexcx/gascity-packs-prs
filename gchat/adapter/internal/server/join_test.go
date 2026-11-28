package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakedoor"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/owners"
	"gchat-adapter/internal/token"
	api "google.golang.org/api/chat/v1"
)

const joinScopeTest = "https://www.googleapis.com/auth/chat.memberships"

type joinFixture struct {
	*fixture
	kmu       sync.Mutex
	scopes    [][]string
	refusal   string
	tokenLife time.Duration
	logs      bytes.Buffer
}

func setupJoin(t *testing.T) *joinFixture {
	t.Helper()
	f := &joinFixture{fixture: setup(t), tokenLife: time.Hour}
	dir, err := os.MkdirTemp("/tmp", "gchat-join-")
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
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		f.kmu.Lock()
		f.scopes = append(f.scopes, input.Scopes)
		refusal := f.refusal
		tokenLife := f.tokenLife
		f.kmu.Unlock()
		if reflect.DeepEqual(input.Scopes, []string{joinScopeTest}) && refusal != "" {
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": refusal, "detail": "private-detail-must-not-escape"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "join-secret-never-log", "expires_at": time.Now().Add(tokenLife).UnixMilli()})
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	f.cfg.Secrets.TokenSource = config.TokenSourceKeyholder
	f.cfg.Secrets.KeyholderSocket = socket
	f.cfg.Secrets.Owners = []string{"owner@example.com"}
	// Root's directory is a path of this test's own that does not exist, so
	// the settings name the owners whatever the host has at the fixed path.
	f.s.opts.Owners = owners.ForTest(owners.TestOptions{Dir: filepath.Join(dir, "absent"), IsRoot: fakedoor.Root})
	f.cfg.Endpoints.PeopleAPIBase = f.g.URL()
	f.s.SetConfig(*f.cfg)
	c, err := chat.New(context.Background(), http.DefaultClient, f.cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SetIdentityUserID(fakegoogle.UserID); err != nil {
		t.Fatal(err)
	}
	f.s.SetClients(c, f.options.GC)
	f.s.opts.Logger = slog.New(slog.NewJSONHandler(&f.logs, nil))
	f.g.Configure(func(g *fakegoogle.Server) {
		g.Memberships["spaces/room/members/123456789"] = &api.Membership{Name: "spaces/room/members/123456789", State: "INVITED", Member: &api.User{Name: fakegoogle.UserID, Type: "HUMAN"}}
		g.People["users/222"] = "owner@example.com"
		g.People["users/333"] = "colleague@example.com"
		g.People["users/444"] = "outside@partner.example"
		g.People[fakegoogle.UserID] = "agent@example.com"
	})
	return f
}

func (f *joinFixture) join(t *testing.T, input map[string]any) map[string]any {
	t.Helper()
	status, b, _ := f.call(t, "/join", input)
	if status != 200 {
		t.Fatalf("join status %d: %s", status, b)
	}
	return decode[map[string]any](t, b)
}

func (f *joinFixture) request(t *testing.T, space string) string {
	t.Helper()
	v := f.join(t, map[string]any{"space": space, "request": true})
	if v["result"] != "requested" {
		t.Fatalf("request: %v", v)
	}
	code, _ := v["code"].(string)
	if !regexp.MustCompile(`^[ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{6,}$`).MatchString(code) {
		t.Fatalf("invalid approval code")
	}
	expires, err := time.Parse(time.RFC3339Nano, fmt.Sprint(v["expires_at"]))
	if err != nil || time.Until(expires) < 23*time.Hour || time.Until(expires) > 24*time.Hour {
		t.Fatalf("request expiry: %v", v["expires_at"])
	}
	return code
}

func (f *joinFixture) yes(code string) string {
	name := "spaces/dm/messages/approval"
	f.g.Configure(func(g *fakegoogle.Server) {
		g.Messages[name] = &api.Message{Name: name, Text: code, Sender: &api.User{Name: "users/222", Type: "HUMAN"}, CreateTime: time.Now().UTC().Format(time.RFC3339Nano), Space: g.Spaces["spaces/dm"]}
	})
	return name
}

func (f *joinFixture) writes() []fakegoogle.MembershipCall {
	var out []fakegoogle.MembershipCall
	f.g.Configure(func(g *fakegoogle.Server) {
		for _, c := range g.MembershipCalls {
			if c.Method != "GET" {
				out = append(out, c)
			}
		}
	})
	return out
}

func (f *joinFixture) rewriteRequest(t *testing.T, edit func(map[string]any)) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(f.l)
	v := decode[map[string]any](t, b)
	requests, ok := v["join_requests"].(map[string]any)
	if !ok {
		t.Fatal("no persistent join request")
	}
	edit(requests["spaces/room"].(map[string]any))
	b, _ = json.Marshal(v)
	if err := json.Unmarshal(b, f.l); err != nil {
		t.Fatal(err)
	}
	if err := f.l.Save(f.options.LedgerPath); err != nil {
		t.Fatal(err)
	}
}

func TestJoinAnswersTable(t *testing.T) {
	// One assertion per modeled answer; evidence labels distinguish Google facts
	// from invitation behavior that only a real proof call can settle.
	for _, tc := range []struct {
		name, state string
		read, write *fakegoogle.MembershipError
		want        string
	}{
		{"DOCUMENTED_G1_joined", "JOINED", nil, nil, "already_member"},
		{"ASSUMED_U2_invited", "INVITED", nil, nil, "joined"},
		{"ASSUMED_U1_still_invited", "INVITED", nil, nil, "refused_invited"},
		{"ASSUMED_U1_lost_answer", "INVITED", nil, nil, "google_no_answer"},
		{"DOCUMENTED_G1_not_member", "NOT_A_MEMBER", nil, nil, "no_invitation"},
		{"ASSUMED_U3_missing", "INVITED", &fakegoogle.MembershipError{HTTP: 404, Status: "NOT_FOUND"}, nil, "no_invitation"},
		{"ASSUMED_U2_unknown_state", "STATE_UNSPECIFIED", nil, nil, "no_invitation"},
		{"ASSUMED_U4_policy_400", "INVITED", nil, &fakegoogle.MembershipError{HTTP: 400, Status: "FAILED_PRECONDITION"}, "refused_invited"},
		{"ASSUMED_U4_policy_403", "INVITED", nil, &fakegoogle.MembershipError{HTTP: 403, Status: "PERMISSION_DENIED"}, "refused_invited"},
		{"ASSUMED_U3_invitation_gone", "INVITED", nil, &fakegoogle.MembershipError{HTTP: 404, Status: "NOT_FOUND"}, "no_invitation"},
		{"ASSUMED_U3_unknown_create", "INVITED", nil, &fakegoogle.MembershipError{HTTP: 409, Status: "ALREADY_EXISTS", Detail: "private-body"}, "google_unknown"},
		{"ASSUMED_U2_unreadable", "INVITED", &fakegoogle.MembershipError{HTTP: 403, Status: "PERMISSION_DENIED", Detail: "private-body"}, nil, "google_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupJoin(t)
			code := f.request(t, "spaces/room")
			yes := f.yes(code)
			f.g.Configure(func(g *fakegoogle.Server) {
				g.Memberships["spaces/room/members/123456789"].State = tc.state
				g.MembershipReadError = tc.read
				g.MembershipWriteError = tc.write
				if tc.name == "ASSUMED_U1_still_invited" {
					g.MembershipCreateState = "INVITED"
				}
				if tc.name == "ASSUMED_U1_lost_answer" {
					g.DropNextResponse = true
				}
			})
			v := f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
			if v["result"] != tc.want {
				t.Fatalf("got %v want %s", v, tc.want)
			}
			if strings.Contains(f.logs.String(), "private-body") {
				t.Fatal("Google body logged")
			}
			if tc.want == "google_unknown" {
				ending := "Nothing changed"
				if tc.write != nil {
					ending = "outcome is unknown"
				}
				if !strings.Contains(fmt.Sprint(v["message"]), "HTTP") || !strings.Contains(fmt.Sprint(v["message"]), ending) {
					t.Fatalf("unknown answer: %v", v)
				}
			}
		})
	}
}

func TestJoinMakesOneWriteForOneRoom(t *testing.T) {
	f := setupJoin(t)
	for _, input := range []map[string]any{
		{"request": true}, {"space": "spaces/room,spaces/dm", "request": true}, {"space": "spaces/*", "request": true},
		{"space": "spaces/room/", "request": true}, {"space": "spaces/..", "request": true}, {"space": "spaces/room", "all": true},
		{"space": []string{"spaces/room", "spaces/dm"}, "request": true}, {"space": "spaces/room?x=y", "request": true},
		{"space": "spaces/room", "request": true, "yes": "spaces/dm/messages/a"},
	} {
		status, b, _ := f.call(t, "/join", input)
		if status != 400 {
			t.Fatalf("bad input: status=%d %s", status, b)
		}
	}
	if len(f.writes()) != 0 {
		t.Fatal("invalid input wrote")
	}
	code := f.request(t, "spaces/room")
	v := f.join(t, map[string]any{"space": "spaces/room", "yes": f.yes(code)})
	if v["result"] != "joined" {
		t.Fatalf("%v", v)
	}
	w := f.writes()
	if len(w) != 1 || w[0].Path != "spaces/room/members" || w[0].Member != fakegoogle.UserID || w[0].Method != "POST" {
		t.Fatalf("writes: %v", w)
	}
}

func TestJoinRefusesWithoutTheOwnersYes(t *testing.T) {
	for _, row := range []struct{ name, reason string }{
		{"colleague", "The message sender is not an owner of this box."},
		{"outsider", "The message sender is not an owner of this box."},
		{"self", "The identity cannot approve its own join."},
		{"room", "The owner's yes must be in a direct message."},
		{"old", "The message was written before this request."},
		{"expired", "This request has expired."},
		{"late", "The message was written after this request expired."},
		{"wrong_code", "The owner must reply with this room's code alone, nothing else."},
		{"other_room_code", "The owner must reply with this room's code alone, nothing else."},
		{"used", "This request was already used."},
		{"no_owner", "No owner is configured for this box."},
		{"no_yes", "Supply the owner's direct message with --yes MESSAGE."},
		{"no_request", "Request approval for this room first with --request."},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := setupJoin(t)
			code := f.request(t, "spaces/room")
			yes := f.yes(code)
			switch row.name {
			case "expired":
				f.rewriteRequest(t, func(r map[string]any) { r["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano) })
			case "used":
				f.rewriteRequest(t, func(r map[string]any) { r["used"] = true })
			case "no_owner":
				f.cfg.Secrets.Owners = nil
				f.s.SetConfig(*f.cfg)
			case "no_yes":
				yes = ""
			case "no_request":
				f.mu.Lock()
				b, _ := json.Marshal(f.l)
				v := decode[map[string]any](t, b)
				v["join_requests"] = nil
				b, _ = json.Marshal(v)
				_ = json.Unmarshal(b, f.l)
				f.mu.Unlock()
			case "other_room_code":
				f.g.Configure(func(g *fakegoogle.Server) {
					g.Memberships["spaces/group/members/123456789"] = &api.Membership{State: "INVITED"}
				})
				other := f.request(t, "spaces/group")
				f.yes(other)
			}
			f.g.Configure(func(g *fakegoogle.Server) {
				m := g.Messages["spaces/dm/messages/approval"]
				switch row.name {
				case "colleague":
					m.Sender.Name = "users/333"
				case "outsider":
					m.Sender.Name = "users/444"
				case "self":
					m.Sender.Name = fakegoogle.UserID
				case "room":
					m.Space = g.Spaces["spaces/room"]
					m.Name = "spaces/room/messages/approval"
					g.Messages[m.Name] = m
					yes = m.Name
				case "old":
					m.CreateTime = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
				case "late":
					m.CreateTime = time.Now().Add(25 * time.Hour).Format(time.RFC3339Nano)
				case "wrong_code":
					m.Text = "WRONGCODE"
				}
			})
			v := f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
			if v["result"] != "no_owner_yes" || v["message"] != row.reason {
				t.Fatalf("got %v want %s", v, row.reason)
			}
			if len(f.writes()) != 0 {
				t.Fatal("refusal wrote membership")
			}
			f.kmu.Lock()
			wantMints := 1
			if row.name == "other_room_code" {
				wantMints = 2
			}
			if len(f.scopes) != wantMints {
				t.Error("an invalid owner's yes requested a write token")
			}
			f.kmu.Unlock()
		})
	}
	for _, row := range []struct {
		name, text string
		approved   bool
	}{
		{"refusal", "No, do not join CODE", false},
		{"question", "What is CODE?", false},
		{"quoted_ask", "> To say yes, reply with: CODE\nno thanks", false},
		{"yes_code", "yes CODE", false},
		{"twice", "CODE CODE", false},
		{"words_before", "please join CODE", false},
		{"words_after", "CODE please join", false},
		{"words_around", "please CODE join", false},
		{"longer_token", "prefixCODEsuffix", false},
		{"punctuation", "CODE!", false},
		{"empty", " \t\n", false},
		{"exact_code", "CODE", true},
		{"lowercase", "lowercase", true},
		{"surrounding_whitespace", " \t\nCODE\r\n ", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := setupJoin(t)
			code := f.request(t, "spaces/room")
			message := strings.ReplaceAll(row.text, "CODE", code)
			if row.name == "lowercase" {
				message = strings.ToLower(code)
			}
			yes := f.yes(message)
			v := f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
			wantResult, wantWrites, wantMints := "no_owner_yes", 0, 1
			if row.approved {
				wantResult, wantWrites, wantMints = "joined", 1, 2
			} else if v["message"] != "The owner must reply with this room's code alone, nothing else." {
				t.Errorf("refusal does not explain the required reply: %v", v)
			}
			if v["result"] != wantResult || len(f.writes()) != wantWrites {
				t.Errorf("result=%v writes=%d; want %s and %d", v["result"], len(f.writes()), wantResult, wantWrites)
			}
			f.kmu.Lock()
			if len(f.scopes) != wantMints {
				t.Error("unexpected permission request count")
			}
			f.kmu.Unlock()
			b, err := os.ReadFile(f.options.LedgerPath)
			if err != nil {
				t.Fatal(err)
			}
			stored := decode[ledger.Ledger](t, b)
			if stored.JoinRequests["spaces/room"].Used != row.approved {
				t.Error("durable approval consumption does not match the owner's yes")
			}
		})
	}
}

func TestJoinShortLivedPermissionPreservesYes(t *testing.T) {
	f := setupJoin(t)
	yes := f.yes(f.request(t, "spaces/room"))
	f.kmu.Lock()
	f.tokenLife = 5 * time.Second
	f.kmu.Unlock()
	v := f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
	if v["result"] != "google_no_answer" || !strings.Contains(fmt.Sprint(v["message"]), "Nothing changed") || !strings.Contains(fmt.Sprint(v["message"]), "same owner's yes") {
		t.Errorf("short-lived permission answer: %v", v)
	}
	if len(f.writes()) != 0 {
		t.Fatal("short-lived permission sent a create")
	}
	b, err := os.ReadFile(f.options.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	stored := decode[ledger.Ledger](t, b)
	if stored.JoinRequests["spaces/room"].Used {
		t.Error("short-lived permission consumed the durable request")
	}
	f.kmu.Lock()
	f.tokenLife = time.Hour
	f.kmu.Unlock()
	v = f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
	if v["result"] != "joined" || len(f.writes()) != 1 {
		t.Fatalf("same yes with a good token: %v; writes=%d", v, len(f.writes()))
	}
}

func TestJoinAskNamesRoomAndID(t *testing.T) {
	f := setupJoin(t)
	v := f.join(t, map[string]any{"space": "spaces/room", "request": true})
	message := fmt.Sprint(v["message"])
	for _, want := range []string{"Example", "spaces/room", "code alone, nothing else"} {
		if !strings.Contains(message, want) {
			t.Errorf("ask is missing %q", want)
		}
	}
}

func TestJoinReadsBeforeItWrites(t *testing.T) {
	f := setupJoin(t)
	f.g.Configure(func(g *fakegoogle.Server) { g.Memberships["spaces/room/members/123456789"].State = "JOINED" })
	v := f.join(t, map[string]any{"space": "spaces/room", "yes": "not-a-message"})
	if v["result"] != "already_member" || len(f.writes()) != 0 {
		t.Fatalf("%v", v)
	}
	f.kmu.Lock()
	defer f.kmu.Unlock()
	if len(f.scopes) != 0 {
		t.Fatal("already member requested a write token")
	}
}

func TestJoinIsIdempotent(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			f := setupJoin(t)
			code := f.request(t, "spaces/room")
			yes := f.yes(code)
			f.g.Configure(func(g *fakegoogle.Server) { g.DropNextResponse = lost })
			input := map[string]any{"space": "spaces/room", "yes": yes}
			first := f.join(t, input)
			want := "joined"
			if lost {
				want = "google_no_answer"
			}
			if first["result"] != want {
				t.Fatalf("first=%v", first)
			}
			second := f.join(t, input)
			if second["result"] != "already_member" || len(f.writes()) != 1 {
				t.Fatalf("second=%v writes=%d", second, len(f.writes()))
			}
		})
	}
}

func TestJoinSaysWhatHappened(t *testing.T) {
	for _, row := range []struct{ state, want string }{
		{"JOINED", "Already a member of this room. Nothing changed."},
		{"NOT_A_MEMBER", "Google shows no invitation for this identity in this room. Check the room id with the person who invited you."},
		{"INVITED", "Joined Example. People in this room can now write to this identity."},
	} {
		t.Run(row.state, func(t *testing.T) {
			f := setupJoin(t)
			code := f.request(t, "spaces/room")
			yes := f.yes(code)
			f.g.Configure(func(g *fakegoogle.Server) { g.Memberships["spaces/room/members/123456789"].State = row.state })
			v := f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
			if v["message"] != row.want {
				t.Fatalf("%v", v)
			}
		})
	}
	for _, row := range []struct{ name, result, message string }{
		{"policy", "refused_invited", "An invitation exists, but Google refused the join. Google answered HTTP 403 PERMISSION_DENIED. The cause is not known yet."},
		{"missing", "permission_missing", "This box cannot accept invitations yet. A Workspace super administrator must open Google Admin console > Security > Access and data control > API controls > Manage Domain Wide Delegation, edit this client's entry, add https://www.googleapis.com/auth/chat.memberships beside the existing permissions, and click Authorize; changes can take up to 24 hours."},
		{"software", "permission_missing", "This box is not set up to hand out that permission yet. Whoever runs the box updates it or turns the permission on; the admin's step alone will not help."},
		{"no_yes", "no_owner_yes", "Supply the owner's direct message with --yes MESSAGE."},
		{"desktop", "not_available", "Not available on this sign-in. This box uses desktop sign-in."},
		{"lost", "google_no_answer", "Google did not answer. The join outcome is unknown; run it again to read membership before any new write."},
		{"unknown", "google_unknown", "Google answered HTTP 409 ALREADY_EXISTS. The join outcome is unknown; run it again to read membership before any new write."},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := setupJoin(t)
			code := f.request(t, "spaces/room")
			yes := f.yes(code)
			switch row.name {
			case "policy":
				f.g.Configure(func(g *fakegoogle.Server) {
					g.MembershipWriteError = &fakegoogle.MembershipError{HTTP: 403, Status: "PERMISSION_DENIED"}
				})
			case "missing":
				f.kmu.Lock()
				f.refusal = "delegation_not_authorized"
				f.kmu.Unlock()
			case "software":
				f.kmu.Lock()
				f.refusal = "scope_not_allowed"
				f.kmu.Unlock()
			case "no_yes":
				yes = ""
			case "desktop":
				f.cfg.Secrets.TokenSource = config.TokenSourceOAuth
				f.s.SetConfig(*f.cfg)
			case "lost":
				f.g.Configure(func(g *fakegoogle.Server) { g.DropNextResponse = true })
			case "unknown":
				f.g.Configure(func(g *fakegoogle.Server) {
					g.MembershipWriteError = &fakegoogle.MembershipError{HTTP: 409, Status: "ALREADY_EXISTS"}
				})
			}
			v := f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
			if v["result"] != row.result || v["message"] != row.message {
				t.Fatalf("%v", v)
			}
		})
	}
}

func TestJoinRecord(t *testing.T) {
	f := setupJoin(t)
	code := f.request(t, "spaces/room")
	yes := f.yes(code)
	f.join(t, map[string]any{"space": "spaces/room", "yes": yes})
	b, err := os.ReadFile(f.options.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{code, "join-secret-never-log", "private-detail-must-not-escape"} {
		if bytes.Contains(b, []byte(secret)) || strings.Contains(f.logs.String(), secret) {
			t.Fatal("secret persisted in audit or ledger")
		}
	}
	v := decode[map[string]any](t, b)
	attempts, ok := v["join_attempts"].([]any)
	if !ok || len(attempts) != 2 {
		t.Fatalf("attempt records: %v", v["join_attempts"])
	}
	last := attempts[1].(map[string]any)
	if last["space"] != "spaces/room" || last["owner_message"] != yes || last["result"] != "joined" || last["at"] == nil {
		t.Fatalf("audit fields: %v", last)
	}
	lines := operationLogLines(t, f.logs.String(), "join")
	if len(lines) != 2 {
		t.Fatalf("audit log count=%d", len(lines))
	}
}

func TestJoinCheck(t *testing.T) {
	f := setupJoin(t)
	v := f.join(t, map[string]any{"check": true})
	if v["result"] != "permission_present" || v["message"] != "The permission to accept invitations is present. No room was touched." {
		t.Fatalf("%v", v)
	}
	f.g.Configure(func(g *fakegoogle.Server) {
		if len(g.MembershipCalls) != 0 {
			t.Fatal("check touched a room")
		}
	})
	f.kmu.Lock()
	if !reflect.DeepEqual(f.scopes, [][]string{{joinScopeTest}}) {
		t.Fatalf("scope requests: %v", f.scopes)
	}
	f.kmu.Unlock()
	f.cfg.Secrets.TokenSource = config.TokenSourceOAuth
	f.s.SetConfig(*f.cfg)
	v = f.join(t, map[string]any{"check": true})
	if v["result"] != "not_available" || v["message"] != "Not available on this sign-in. This box uses desktop sign-in." {
		t.Fatalf("desktop: %v", v)
	}
}

func TestMissingPermissionChangesNothing(t *testing.T) {
	for _, code := range []string{"delegation_not_authorized", "scope_not_allowed", "invalid_scope", "grant_rejected", "subject_not_found", "key_rejected", "bad_request"} {
		t.Run(code, func(t *testing.T) {
			f := setupJoin(t)
			source, err := token.NewKeyholder(f.cfg.Secrets.KeyholderSocket, f.cfg.Endpoints, token.KeyholderScopes)
			if err != nil {
				t.Fatal(err)
			}
			client, err := source.Client(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			res, err := client.Get(f.g.URL() + "/v1/spaces")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			before := f.s.Health()
			f.kmu.Lock()
			f.refusal = code
			f.kmu.Unlock()
			v := f.join(t, map[string]any{"space": "spaces/room", "request": true})
			if v["result"] != "permission_missing" {
				t.Fatalf("%v", v)
			}
			if code == "scope_not_allowed" {
				if v["message"] != "This box is not set up to hand out that permission yet. Whoever runs the box updates it or turns the permission on; the admin's step alone will not help." {
					t.Fatalf("%v", v)
				}
			} else if code == "delegation_not_authorized" {
				if !strings.Contains(fmt.Sprint(v["message"]), joinScopeTest) || !strings.Contains(fmt.Sprint(v["message"]), "super administrator") {
					t.Fatalf("admin instruction: %v", v)
				}
			} else if v["message"] != "The permission could not be given for another reason. The admin's permission step will not fix it; nothing changed." || v["permission_error"] != code {
				t.Fatalf("other permission refusal: %v", v)
			}
			if f.s.Health() != before || source.State() != token.Ready || len(f.writes()) != 0 {
				t.Fatal("refused permission changed running adapter")
			}
			res, err = client.Get(f.g.URL() + "/v1/spaces")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			f.kmu.Lock()
			if len(f.scopes) != 2 || !reflect.DeepEqual(f.scopes[0], token.KeyholderScopes) || !reflect.DeepEqual(f.scopes[1], []string{joinScopeTest}) {
				t.Error("permission refusal changed or refreshed the standing token")
			}
			f.kmu.Unlock()
			if f.count() != 0 {
				t.Fatal("join refusal posted a notice")
			}
		})
	}
}

func TestJoinRequestReplacementAndRestart(t *testing.T) {
	f := setupJoin(t)
	old := f.request(t, "spaces/room")
	fresh := f.request(t, "spaces/room")
	if old == fresh {
		t.Fatal("request code reused")
	}
	loaded, err := ledger.Load(f.options.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	*f.l = *loaded
	f.mu.Unlock()
	v := f.join(t, map[string]any{"space": "spaces/room", "yes": f.yes(old)})
	if v["result"] != "no_owner_yes" {
		t.Fatalf("old code accepted: %v", v)
	}
	v = f.join(t, map[string]any{"space": "spaces/room", "yes": f.yes(fresh)})
	if v["result"] != "joined" {
		t.Fatalf("fresh persisted code: %v", v)
	}
}
