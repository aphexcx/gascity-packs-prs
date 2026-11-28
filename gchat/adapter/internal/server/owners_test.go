package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakedoor"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/owners"
	api "google.golang.org/api/chat/v1"
)

// Every id is made up. users/222 is the owner whose yes the join fixture
// writes (its directory address is owner@example.com); users/333 another.
const ownersClaim = "dddddddddddddddddddddddddddddddd"

type ownersFixture struct {
	*joinFixture
	door *fakedoor.Door
}

// setupOwners is the join fixture with a stand-in door. The settings still
// name owner@example.com, which root's directory makes count for nothing.
func setupOwners(t *testing.T, copyOwners ...string) *ownersFixture {
	t.Helper()
	f := &ownersFixture{joinFixture: setupJoin(t), door: fakedoor.New(t)}
	f.s.opts.Owners = f.door.Book(nil, f.s.opts.Logger)
	f.publish(t, copyOwners...)
	// The door removes as asked and publishes the copy without that owner.
	f.door.Answer(func(r map[string]string) string {
		if r["request"] != "remove" {
			return `{"result":"unavailable"}`
		}
		current := f.s.opts.Owners.List().Owners
		var left []string
		found := false
		for _, e := range current {
			if e.ID == r["owner"] {
				found = true
				continue
			}
			left = append(left, e.ID)
		}
		if !found {
			return `{"result":"not_owner"}`
		}
		f.publish(t, left...)
		return `{"result":"removed"}`
	})
	return f
}

func (f *ownersFixture) publish(t *testing.T, ids ...string) {
	records := map[string]owners.Record{}
	for _, id := range ids {
		records[id] = fakedoor.Record(ownersClaim)
	}
	fakedoor.WriteCopy(t, f.door.Dir, records)
}

func (f *ownersFixture) owners(t *testing.T, input map[string]any) (int, map[string]any) {
	t.Helper()
	status, b, _ := f.call(t, "/owners", input)
	return status, decode[map[string]any](t, b)
}

func (f *ownersFixture) requestRemoval(t *testing.T, target string) string {
	t.Helper()
	_, v := f.owners(t, map[string]any{"remove": target, "request": true})
	code, _ := v["code"].(string)
	if v["result"] != "requested" || len(code) != 10 {
		t.Fatalf("request: %v", v)
	}
	return code
}

// yesFrom writes the owner's reply in a direct chat. UNPROVEN (U5): that
// Google gives every delivered message its creation time; the check of a
// yes, the join's, requires it.
func (f *ownersFixture) yesFrom(sender, spaceName, text string, at time.Time) string {
	name := spaceName + "/messages/yes-" + sender[6:] + "-" + strings.ToLower(text)
	f.g.Configure(func(g *fakegoogle.Server) {
		g.Messages[name] = &api.Message{Name: name, Text: text, Sender: &api.User{Name: sender, Type: "HUMAN"}, CreateTime: at.UTC().Format(time.RFC3339Nano), Space: g.Spaces[spaceName]}
	})
	return name
}

func (f *ownersFixture) removes() []map[string]string {
	var out []map[string]string
	for _, r := range f.door.Requests() {
		if r["request"] == "remove" {
			out = append(out, r)
		}
	}
	return out
}

func (f *ownersFixture) ownersLogs(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) == nil && v["msg"] == "gchat owners" {
			out = append(out, v)
		}
	}
	return out
}

// TestChatOwnerOwnersList: gc gchat owners lists exactly what root's copy
// holds; where root's directory is absent it lists the settings' entries and
// says they come from the settings.
func TestChatOwnerOwnersList(t *testing.T) {
	f := setupOwners(t, "users/333", "users/222")
	status, v := f.owners(t, map[string]any{})
	want := []any{
		map[string]any{"id": "users/222", "since": "2026-01-02T03:04:05Z", "by": "page-code", "claim": ownersClaim},
		map[string]any{"id": "users/333", "since": "2026-01-02T03:04:05Z", "by": "page-code", "claim": ownersClaim},
	}
	if status != 200 || v["result"] != "owners" || !reflect.DeepEqual(v["owners"], want) || v["settings"] != nil {
		t.Fatalf("list: %d %v", status, v)
	}
	f.publish(t)
	if _, v = f.owners(t, map[string]any{}); v["result"] != "no_owner" || v["owners"] != nil {
		t.Fatalf("empty copy: %v", v)
	}
	f.s.opts.Owners = owners.ForTest(owners.TestOptions{Dir: filepath.Join(f.door.Dir, "absent"), IsRoot: fakedoor.Root})
	if _, v = f.owners(t, map[string]any{}); v["result"] != "settings_owner" || !reflect.DeepEqual(v["settings"], []any{"owner@example.com"}) || !strings.Contains(v["message"].(string), "settings") {
		t.Fatalf("absent: %v", v)
	}
	if len(f.door.Requests()) != 0 {
		t.Fatal("a list called the door")
	}
}

// TestChatOwnerOwnersAdd: asked to add, with any other flag, it makes no
// request, changes nothing and answers that a new owner comes by a code
// from the owner page.
func TestChatOwnerOwnersAdd(t *testing.T) {
	f := setupOwners(t, "users/222")
	for _, input := range []map[string]any{
		{"add": true},
		{"add": true, "remove": "users/333", "request": true},
		{"add": true, "remove": "users/333", "yes": "spaces/dm/messages/x"},
		{"add": false, "remove": "users/x"},
	} {
		status, v := f.owners(t, input)
		if status != 200 || v["result"] != "add_by_page" || !strings.Contains(v["message"].(string), "owner page") || v["code"] != nil {
			t.Fatalf("%v: %d %v", input, status, v)
		}
	}
	snapshot, _ := f.s.snapshot(t.Context())
	if len(snapshot.OwnerRequests) != 0 || len(f.door.Requests()) != 0 {
		t.Fatalf("add changed something: %v %v", snapshot.OwnerRequests, f.door.Requests())
	}
	if got := f.s.opts.Owners.List().Owners; len(got) != 1 || got[0].ID != "users/222" {
		t.Fatalf("owners after add: %v", got)
	}
}

// TestChatOwnerOwnersRemove: a removal follows the join's check of the
// owner's yes; a good yes is used once and the door gets one remove line
// with the Chat id and the name of the message that asked; any failed check
// sends the door nothing.
func TestChatOwnerOwnersRemove(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		owners []string
		run    func(t *testing.T, f *ownersFixture) map[string]any
		result string
		sent   int
	}{
		{"a good yes", []string{"users/222", "users/333"}, func(t *testing.T, f *ownersFixture) map[string]any {
			code := f.requestRemoval(t, "users/333")
			yes := f.yesFrom("users/222", "spaces/dm", code, time.Now())
			_, v := f.owners(t, map[string]any{"remove": "users/333", "yes": yes})
			if got := f.removes(); !reflect.DeepEqual(got, []map[string]string{{"request": "remove", "owner": "users/333", "message": yes}}) {
				t.Fatalf("remove line: %v", got)
			}
			if f.s.opts.Owners.Owner("users/333", "") || !f.s.opts.Owners.Owner("users/222", "") {
				t.Fatal("the copy after the removal")
			}
			snapshot, _ := f.s.snapshot(t.Context())
			if !snapshot.OwnerRequests["users/333"].Used || len(snapshot.OwnerEvents) != 1 || snapshot.OwnerEvents[0].Event != "removed" || snapshot.OwnerEvents[0].Owner != "users/333" || snapshot.OwnerEvents[0].Message != yes {
				t.Fatalf("state: %+v %+v", snapshot.OwnerRequests, snapshot.OwnerEvents)
			}
			b, _ := json.Marshal(snapshot.OwnerRequests)
			if strings.Contains(string(b), code) {
				t.Fatal("the state keeps the request's code")
			}
			return v
		}, "removed", 1},
		{"an owner removes themself, the last owner", []string{"users/222"}, func(t *testing.T, f *ownersFixture) map[string]any {
			code := f.requestRemoval(t, "users/222")
			yes := f.yesFrom("users/222", "spaces/dm", code, time.Now())
			_, v := f.owners(t, map[string]any{"remove": "users/222", "yes": yes})
			if f.s.opts.Owners.Any() {
				t.Fatal("an owner is left")
			}
			if j := f.join(t, map[string]any{"space": "spaces/room", "request": true}); j["result"] != "no_owner_yes" || j["message"] != "No owner is configured for this box." {
				t.Fatalf("join after the last owner's removal: %v", j)
			}
			return v
		}, "removed", 1},
		{"a yes from a sender who is not an owner", []string{"users/333"}, func(t *testing.T, f *ownersFixture) map[string]any {
			code := f.requestRemoval(t, "users/333")
			_, v := f.owners(t, map[string]any{"remove": "users/333", "yes": f.yesFrom("users/222", "spaces/dm", code, time.Now())})
			return v
		}, "no_owner_yes", 0},
		{"a yes written before the request", []string{"users/222", "users/333"}, func(t *testing.T, f *ownersFixture) map[string]any {
			code := f.requestRemoval(t, "users/333")
			_, v := f.owners(t, map[string]any{"remove": "users/333", "yes": f.yesFrom("users/222", "spaces/dm", code, now.Add(-time.Hour))})
			return v
		}, "no_owner_yes", 0},
		{"a yes in a room", []string{"users/222", "users/333"}, func(t *testing.T, f *ownersFixture) map[string]any {
			code := f.requestRemoval(t, "users/333")
			_, v := f.owners(t, map[string]any{"remove": "users/333", "yes": f.yesFrom("users/222", "spaces/room", code, time.Now())})
			return v
		}, "no_owner_yes", 0},
		{"a yes with another code", []string{"users/222", "users/333"}, func(t *testing.T, f *ownersFixture) map[string]any {
			f.requestRemoval(t, "users/333")
			_, v := f.owners(t, map[string]any{"remove": "users/333", "yes": f.yesFrom("users/222", "spaces/dm", "ABCDEFGHJK", time.Now())})
			return v
		}, "no_owner_yes", 0},
		{"a yes from the identity", []string{"users/222", "users/333"}, func(t *testing.T, f *ownersFixture) map[string]any {
			code := f.requestRemoval(t, "users/333")
			_, v := f.owners(t, map[string]any{"remove": "users/333", "yes": f.yesFrom(fakegoogle.UserID, "spaces/dm", code, time.Now())})
			return v
		}, "no_owner_yes", 0},
		{"a request used twice", []string{"users/222", "users/333"}, func(t *testing.T, f *ownersFixture) map[string]any {
			code := f.requestRemoval(t, "users/333")
			yes := f.yesFrom("users/222", "spaces/dm", code, time.Now())
			if _, v := f.owners(t, map[string]any{"remove": "users/333", "yes": yes}); v["result"] != "removed" {
				t.Fatalf("first: %v", v)
			}
			_, v := f.owners(t, map[string]any{"remove": "users/333", "yes": yes})
			return v
		}, "no_owner_yes", 1},
		{"a newer request replaces an older", []string{"users/222", "users/333"}, func(t *testing.T, f *ownersFixture) map[string]any {
			old := f.requestRemoval(t, "users/333")
			f.requestRemoval(t, "users/333")
			_, v := f.owners(t, map[string]any{"remove": "users/333", "yes": f.yesFrom("users/222", "spaces/dm", old, time.Now())})
			return v
		}, "no_owner_yes", 0},
		{"a yes without a request", []string{"users/222", "users/333"}, func(t *testing.T, f *ownersFixture) map[string]any {
			_, v := f.owners(t, map[string]any{"remove": "users/333", "yes": f.yesFrom("users/222", "spaces/dm", "ABCDEFGHJK", time.Now())})
			return v
		}, "no_owner_yes", 0},
		{"the identity as the target", []string{"users/222"}, func(t *testing.T, f *ownersFixture) map[string]any {
			_, v := f.owners(t, map[string]any{"remove": fakegoogle.UserID, "request": true})
			return v
		}, "invalid_request", 0},
		{"no owner yet", nil, func(t *testing.T, f *ownersFixture) map[string]any {
			_, v := f.owners(t, map[string]any{"remove": "users/333", "request": true})
			return v
		}, "no_owner", 0},
		{"not an owner by the door's word", []string{"users/222"}, func(t *testing.T, f *ownersFixture) map[string]any {
			code := f.requestRemoval(t, "users/333")
			_, v := f.owners(t, map[string]any{"remove": "users/333", "yes": f.yesFrom("users/222", "spaces/dm", code, time.Now())})
			return v
		}, "not_owner", 1},
		{"root's directory absent", []string{"users/222"}, func(t *testing.T, f *ownersFixture) map[string]any {
			f.s.opts.Owners = owners.ForTest(owners.TestOptions{Dir: filepath.Join(f.door.Dir, "absent"), IsRoot: fakedoor.Root})
			_, v := f.owners(t, map[string]any{"remove": "users/222", "request": true})
			return v
		}, "settings_owner", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupOwners(t, tc.owners...)
			v := tc.run(t, f)
			if v["result"] != tc.result {
				t.Fatalf("result: %v", v)
			}
			if n := len(f.removes()); n != tc.sent {
				t.Fatalf("remove lines = %d: %v", n, f.door.Requests())
			}
			for _, line := range f.ownersLogs(t) {
				for _, key := range []string{"action", "target", "owner_message", "result"} {
					if _, ok := line[key]; !ok {
						t.Fatalf("owners log line lacks %s: %v", key, line)
					}
				}
			}
			if logs := f.logs.String(); strings.Contains(logs, "@example.com") {
				t.Fatalf("an address in the log: %s", logs)
			}
		})
	}
}

// TestChatOwnerOwnersLogLines: one "gchat owners" line for each call, with
// the action, the target, the owner's message name and the result, and no
// line carries the request's code or an address.
func TestChatOwnerOwnersLogLines(t *testing.T) {
	f := setupOwners(t, "users/222", "users/333")
	f.owners(t, map[string]any{})
	f.owners(t, map[string]any{"add": true})
	code := f.requestRemoval(t, "users/333")
	yes := f.yesFrom("users/222", "spaces/dm", code, time.Now())
	f.owners(t, map[string]any{"remove": "users/333", "yes": yes})
	want := []map[string]any{
		{"action": "list", "target": "", "owner_message": "", "result": "owners"},
		{"action": "add", "target": "", "owner_message": "", "result": "add_by_page"},
		{"action": "request", "target": "users/333", "owner_message": "", "result": "requested"},
		{"action": "yes", "target": "users/333", "owner_message": yes, "result": "removed"},
	}
	var got []map[string]any
	for _, line := range f.ownersLogs(t) {
		got = append(got, map[string]any{"action": line["action"], "target": line["target"], "owner_message": line["owner_message"], "result": line["result"]})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("owners lines = %v", got)
	}
	if logs := f.logs.String(); strings.Contains(logs, code) || strings.Contains(logs, "@example.com") {
		t.Fatalf("a log line carries the code or an address: %s", logs)
	}
}

// TestChatOwnerOwnersAddWhereRootIsAbsent: asked to add in a city that is
// not a box, the answer is still add_by_page, and it says the settings name
// the owners there.
func TestChatOwnerOwnersAddWhereRootIsAbsent(t *testing.T) {
	f := setupOwners(t)
	f.s.opts.Owners = owners.ForTest(owners.TestOptions{Dir: filepath.Join(f.door.Dir, "absent"), IsRoot: fakedoor.Root})
	if _, v := f.owners(t, map[string]any{"add": true}); v["result"] != "add_by_page" || v["message"] != addBySettings {
		t.Fatalf("add: %v", v)
	}
}

// TestChatOwnerOwnersDoorDoesNotConfirm: when the door does not answer, or
// answers refused or unavailable, the adapter removes nothing by itself and
// sends nothing again; the request stays used and the answer carries the
// one fixed line.
func TestChatOwnerOwnersDoorDoesNotConfirm(t *testing.T) {
	for name, reply := range map[string]string{"silent": fakedoor.Silent, "refused": `{"result":"refused"}`, "unavailable": `{"result":"unavailable"}`, "outside the line form": `{"result":"removed","owner":"users/333"}`} {
		t.Run(name, func(t *testing.T) {
			f := setupOwners(t, "users/222", "users/333")
			f.door.Answer(func(map[string]string) string { return reply })
			code := f.requestRemoval(t, "users/333")
			yes := f.yesFrom("users/222", "spaces/dm", code, time.Now())
			_, v := f.owners(t, map[string]any{"remove": "users/333", "yes": yes})
			if v["result"] != "door_unavailable" || v["message"] != doorUnavailable {
				t.Fatalf("answer: %v", v)
			}
			if _, again := f.owners(t, map[string]any{"remove": "users/333", "yes": yes}); again["result"] != "no_owner_yes" || again["message"] != "This request was already used." {
				t.Fatalf("second yes: %v", again)
			}
			if n := len(f.removes()); n != 1 {
				t.Fatalf("remove lines = %d", n)
			}
			if !f.s.opts.Owners.Owner("users/333", "") {
				t.Fatal("the adapter removed an owner by itself")
			}
		})
	}
}

// TestChatOwnerOwnersInvalid: malformed requests change nothing.
func TestChatOwnerOwnersInvalid(t *testing.T) {
	f := setupOwners(t, "users/222")
	for _, body := range []string{
		`{"remove":"users/abc","request":true}`,
		`{"remove":"person@example.com","request":true}`,
		`{"remove":"users/333"}`,
		`{"remove":"users/333","request":true,"yes":"spaces/dm/messages/a"}`,
		`{"request":true}`,
		`{"yes":"spaces/dm/messages/a"}`,
		`{"remove":"users/333","yes":"spaces/dm/messages/../a"}`,
		`{"remove":"users/333","remove":"users/222","request":true}`,
		`{"list":true}`,
		`[]`,
	} {
		w := httptest.NewRecorder()
		f.s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/owners", strings.NewReader(body)))
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"invalid_request"`) {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	snapshot, _ := f.s.snapshot(t.Context())
	if len(snapshot.OwnerRequests) != 0 || len(f.door.Requests()) != 0 {
		t.Fatal("an invalid request changed something")
	}
}

// TestChatOwnerJoinAsksTheOneFunction: the join's two questions, "is any
// owner configured" and "is this yes an owner's", are root's copy where
// root's directory is present, never the settings there.
func TestChatOwnerJoinAsksTheOneFunction(t *testing.T) {
	t.Run("an owner of the copy", func(t *testing.T) {
		f := setupOwners(t, "users/222")
		f.cfg.Secrets.Owners = nil
		f.s.SetConfig(*f.cfg)
		code := f.request(t, "spaces/room")
		if v := f.join(t, map[string]any{"space": "spaces/room", "yes": f.yes(code)}); v["result"] != "joined" {
			t.Fatalf("join: %v", v)
		}
	})
	t.Run("the copy stops naming the owner", func(t *testing.T) {
		f := setupOwners(t, "users/222")
		code := f.request(t, "spaces/room")
		f.publish(t, "users/333")
		if v := f.join(t, map[string]any{"space": "spaces/room", "yes": f.yes(code)}); v["result"] != "no_owner_yes" || v["message"] != "The message sender is not an owner of this box." {
			t.Fatalf("join: %v", v)
		}
		if len(f.writes()) != 0 {
			t.Fatal("joined without an owner's yes")
		}
	})
	t.Run("an owner named only in the settings", func(t *testing.T) {
		f := setupOwners(t)
		f.s.opts.Owners = f.door.Book(nil, slog.New(slog.NewJSONHandler(&f.logs, nil)))
		if v := f.join(t, map[string]any{"space": "spaces/room", "request": true}); v["result"] != "no_owner_yes" || v["message"] != "No owner is configured for this box." {
			t.Fatalf("join: %v", v)
		}
		if !strings.Contains(f.logs.String(), "gchat owners settings ignored") {
			t.Fatal("no settings-ignored line")
		}
	})
}

// TestChatOwnerRestoreResetBackup: a restore, a reset and a backup made by
// hand change no owner; the copy alone decides.
func TestChatOwnerRestoreResetBackup(t *testing.T) {
	f := setupOwners(t, "users/222")
	before := f.s.opts.Owners.List()
	backup := ledger.New()
	backup.OwnerRequests = map[string]*ledger.OwnerRequest{"users/222": {CodeHash: strings.Repeat("0", 64), Identity: fakegoogle.UserID, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}
	backup.OwnerEvents = []ledger.OwnerEvent{{At: time.Now().UTC(), Event: "taken", Claim: ownersClaim, Owner: "users/999", Message: "spaces/dm/messages/x"}}
	raw, _ := json.Marshal(backup)
	var hand map[string]any
	_ = json.Unmarshal(raw, &hand)
	// What a hand-made backup might try: owners, claims and tries of its own.
	hand["owners"] = map[string]any{"users/999": map[string]any{"since": "2026-01-02T03:04:05Z", "by": "page-code", "claim": ownersClaim}}
	hand["owner_claim"] = map[string]any{"code_sha256": strings.Repeat("a", 64)}
	hand["tries"] = map[string]any{"claim": ownersClaim, "messages": []string{}}
	raw, _ = json.Marshal(hand)
	for _, body := range []map[string]string{{"action": "restore", "ledger": string(raw)}, {"action": "reset"}} {
		f.s.SetHealth(Health{State: "recovery-required"})
		status, b, _ := f.call(t, "/state", body)
		if status != 200 {
			t.Fatalf("%s: %d %s", body["action"], status, b)
		}
		f.s.SetHealth(Health{State: "ready"})
		if after := f.s.opts.Owners.List(); !reflect.DeepEqual(after, before) || f.s.ownerBook(f.s.configuration()).Owner("users/999", "") {
			t.Fatalf("%s changed the owners: %+v", body["action"], after)
		}
		if _, v := f.owners(t, map[string]any{}); v["result"] != "owners" || len(v["owners"].([]any)) != 1 {
			t.Fatalf("%s: list %v", body["action"], v)
		}
		saved, err := os.ReadFile(f.cfg.Endpoints.StateFile)
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(saved, &keys); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"owners", "owner_claim", "tries"} {
			if _, ok := keys[key]; ok {
				t.Fatalf("%s kept the backup's %q in the state", body["action"], key)
			}
		}
	}
	if len(f.door.Requests()) != 0 {
		t.Fatal("a restore or reset called the door")
	}
}
