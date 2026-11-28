package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakedoor"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/owners"
	"gchat-adapter/internal/principal"
	"gchat-adapter/internal/puller"
	"github.com/BurntSushi/toml"
	api "google.golang.org/api/chat/v1"
)

// Every id here is made up. The code is twelve characters of the alphabet.
const (
	dmSpace     = "spaces/dm"
	claimSender = "users/1001"
	claimName   = "Example Person"
	claimAddr   = "person@example.com"
	claimText   = "abcd-EFGH jklm"
	claimCode   = "ABCDEFGHJKLM"
	claimID     = "cccccccccccccccccccccccccccccccc"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type claimFixture struct {
	*fixture
	door *fakedoor.Door
	book *owners.Book
	logs *lockedBuffer
}

// setupClaim is the inbound fixture with a stand-in door and root's
// directory present (or absent when dir names another path).
func setupClaim(t *testing.T, settings []string, absent bool) *claimFixture {
	t.Helper()
	f := setup(t)
	cf := &claimFixture{fixture: f, door: fakedoor.New(t), logs: &lockedBuffer{}}
	log := slog.New(slog.NewJSONHandler(cf.logs, nil))
	dir := cf.door.Dir
	if absent {
		dir = filepath.Join(fakedoor.Dir(t), "absent")
	}
	cf.book = owners.ForTest(owners.TestOptions{Settings: settings, Dir: dir, IsRoot: fakedoor.Root, Log: log, Wait: fakedoor.Wait})
	f.google.Configure(func(s *fakegoogle.Server) {
		s.Spaces[dmSpace] = &api.Space{Name: dmSpace, SpaceType: "DIRECT_MESSAGE"}
		s.People[claimSender] = claimAddr
	})
	f.opts.Logger = log
	f.opts.Principals = principal.New(principal.Options{Book: cf.book, Lookup: principal.DirectoryLookup(http.DefaultClient, f.google.URL())})
	p, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	f.p = p
	return cf
}

func dmMessage(id, text string) *api.Message {
	return &api.Message{Name: dmSpace + "/messages/" + id, Text: text, CreateTime: "2026-10-22T10:00:00Z",
		Space: &api.Space{Name: dmSpace, SpaceType: "DIRECT_MESSAGE"}, Sender: &api.User{Name: claimSender, DisplayName: claimName, Type: "HUMAN"}}
}

func (cf *claimFixture) deliver(t *testing.T, pubsub, kind string, m *api.Message) {
	t.Helper()
	sp := dmSpace
	if m.Space != nil {
		sp = m.Space.Name
	}
	handle(t, cf.fixture, &puller.Event{PubSubID: pubsub, Changes: []puller.Change{{Kind: kind, Message: m, Space: sp}}})
}

// turn is the one turn the mayor got for message id.
func (cf *claimFixture) turn(t *testing.T, id string) []string {
	t.Helper()
	var lines []string
	for _, w := range cf.gc.Inbound() {
		if w.ProviderMessageID == dmSpace+"/messages/"+id || w.ProviderMessageID == space+"/messages/"+id {
			if lines != nil {
				t.Fatalf("two turns for %s", id)
			}
			lines = strings.Split(w.Text, "\n")
		}
	}
	if lines == nil {
		t.Fatalf("no turn for %s in %+v", id, cf.gc.Inbound())
	}
	return lines
}

// saved reads the state file the adapter wrote.
func (cf *claimFixture) saved(t *testing.T) (map[string]json.RawMessage, *ledger.Ledger) {
	t.Helper()
	data, err := os.ReadFile(cf.opts.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return keys, l
}

// noOwnerInState: the state file holds no owner, no claim and no try, and
// its new keys carry no code, text, name or address.
func (cf *claimFixture) noOwnerInState(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(cf.opts.LedgerPath); errors.Is(err, os.ErrNotExist) {
		return // nothing was saved at all
	}
	keys, _ := cf.saved(t)
	for key, raw := range keys {
		lower := strings.ToLower(key)
		if key != "owner_events" && key != "owner_requests" && (strings.Contains(lower, "owner") || strings.Contains(lower, "claim") || strings.Contains(lower, "tries")) {
			t.Fatalf("state key %q", key)
		}
		if key == "owner_events" || key == "owner_requests" {
			for _, secret := range []string{claimCode, strings.ToUpper(claimText), claimText, claimName, claimAddr, "code_sha256", `"tries"`, `"messages"`} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Fatalf("state key %s carries %q: %s", key, secret, raw)
				}
			}
		}
	}
}

// noSecretsLogged: no log line carries the code, the message's text, the
// sender's name or an address.
func (cf *claimFixture) noSecretsLogged(t *testing.T) {
	t.Helper()
	logs := cf.logs.String()
	for _, secret := range []string{claimCode, claimText, claimName, claimAddr, "@example.com"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("a log line carries %q: %s", secret, logs)
		}
	}
}

func (cf *claimFixture) claimLogs(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(cf.logs.String()), "\n") {
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) == nil && v["msg"] == "gchat owner claim" {
			out = append(out, v)
		}
	}
	return out
}

func others(n int) map[string]owners.Record {
	out := map[string]owners.Record{}
	for i := range n {
		out[fmt.Sprintf("users/%d", 5000+i)] = fakedoor.Record(claimID)
	}
	return out
}

// TestChatOwnerTakeRows: a code-shaped message is knocked for once, with the
// 12 characters, the sender's Chat id and the message's name, before it is
// handed to the mayor; the turn's first line follows root's copy after the
// answer and its last line is the fixed line of that answer. The adapter
// adds no owner by itself and its state holds no owner, claim or try.
func TestChatOwnerTakeRows(t *testing.T) {
	answer := func(result string) string {
		if result == "not_live" || result == "refused" || result == "unavailable" {
			return `{"result":"` + result + `"}`
		}
		return `{"result":"` + result + `","claim":"` + claimID + `"}`
	}
	for _, tc := range []struct {
		name, reply, line string
		before            map[string]owners.Record
		publish           map[string]owners.Record
		owner             bool
		event             string
	}{
		{name: "taken", reply: answer("taken"), line: claimLines["taken"], publish: map[string]owners.Record{claimSender: fakedoor.Record(claimID)}, owner: true, event: "taken"},
		{name: "already owner", reply: answer("already_owner"), line: claimLines["already_owner"], before: map[string]owners.Record{claimSender: fakedoor.Record(claimID)}, owner: true},
		{name: "full", reply: answer("full"), line: claimLines["full"], before: others(50)},
		{name: "wrong", reply: answer("wrong"), line: claimLines["wrong"]},
		{name: "fifth wrong", reply: answer("tries_ended"), line: claimLines["tries_ended"], event: "tries_ended"},
		{name: "not live", reply: answer("not_live"), line: claimLines["not_live"]},
		{name: "no answer in time", reply: fakedoor.Silent, line: claimLines["no_answer"]},
		{name: "refused", reply: answer("refused"), line: claimLines["no_answer"]},
		{name: "unavailable", reply: answer("unavailable"), line: claimLines["no_answer"]},
		{name: "outside the line form", reply: `{"result":"taken"}`, line: claimLines["no_answer"]},
		// A door that publishes the sender and answers nothing: the adapter
		// says it could not check, adds nobody, and the copy alone decides.
		{name: "no answer after a copy naming the sender", reply: fakedoor.Silent, line: claimLines["no_answer"], publish: map[string]owners.Record{claimSender: fakedoor.Record(claimID)}, owner: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cf := setupClaim(t, nil, false)
			if tc.before != nil {
				fakedoor.WriteCopy(t, cf.door.Dir, tc.before)
			}
			var handedBefore atomic.Int64
			handedBefore.Store(-1)
			cf.door.Answer(func(map[string]string) string {
				handedBefore.Store(int64(len(cf.gc.Inbound())))
				if tc.publish != nil {
					fakedoor.WriteCopy(t, cf.door.Dir, tc.publish)
				}
				return tc.reply
			})
			cf.deliver(t, "m1", "message.created", dmMessage("m1", claimText))
			want := []map[string]string{{"request": "take", "code": claimCode, "sender": claimSender, "message": dmSpace + "/messages/m1"}}
			if got := cf.door.Requests(); !reflect.DeepEqual(got, want) {
				t.Fatalf("door lines = %v", got)
			}
			if n := handedBefore.Load(); n != 0 {
				t.Fatalf("the message was handed to the mayor before the knock (%d turns)", n)
			}
			lines := cf.turn(t, "m1")
			if last := lines[len(lines)-1]; last != tc.line {
				t.Fatalf("last line = %q", last)
			}
			if lines[1] != claimText {
				t.Fatalf("the message's text did not reach the mayor: %q", lines)
			}
			if got := strings.Contains(lines[0], "| owner"); got != tc.owner {
				t.Fatalf("first line %q, owner %v", lines[0], tc.owner)
			}
			for _, line := range lines[1:] {
				if !tc.owner && strings.Contains(line, "| owner") {
					t.Fatalf("a line calls the sender an owner: %q", line)
				}
			}
			if got := cf.book.Owner(claimSender, ""); got != tc.owner {
				t.Fatalf("owner after the turn = %v", got)
			}
			cf.noOwnerInState(t)
			cf.noSecretsLogged(t)
			_, l := cf.saved(t)
			var events []string
			for _, e := range l.OwnerEvents {
				events = append(events, e.Event)
				if e.Owner != claimSender || e.Message != dmSpace+"/messages/m1" || e.Claim != claimID {
					t.Fatalf("event = %+v", e)
				}
			}
			if tc.event == "" && len(events) != 0 || tc.event != "" && !reflect.DeepEqual(events, []string{tc.event}) {
				t.Fatalf("events = %v", events)
			}
			logs := cf.claimLogs(t)
			result := strings.ReplaceAll(strings.ToLower(tc.name), " ", "_")
			for word, answer := range map[string]string{"already_owner": "already_owner", "fifth_wrong": "tries_ended", "not_live": "not_live"} {
				if result == word {
					result = answer
				}
			}
			if tc.line == claimLines["no_answer"] {
				result = "no_answer"
			}
			if len(logs) != 1 || logs[0]["result"] != result || logs[0]["sender"] != claimSender || logs[0]["message"] != dmSpace+"/messages/m1" {
				t.Fatalf("claim log = %v", logs)
			}
		})
	}
}

// TestChatOwnerTakenReadsTheCopyAgain: after taken the copy is read again
// before the frame, even when the door rewrote it in place so that it looks
// unchanged.
func TestChatOwnerTakenReadsTheCopyAgain(t *testing.T) {
	cf := setupClaim(t, nil, false)
	fakedoor.WriteCopy(t, cf.door.Dir, map[string]owners.Record{"users/9999": fakedoor.Record(claimID)})
	if cf.book.Owner(claimSender, "") {
		t.Fatal("not yet an owner")
	}
	cf.door.Answer(func(map[string]string) string {
		path := filepath.Join(cf.door.Dir, "owners.json")
		fi, err := os.Stat(path)
		if err != nil {
			return `{"result":"unavailable"}`
		}
		data, _ := json.Marshal(map[string]any{"version": 1, "owners": map[string]owners.Record{claimSender: fakedoor.Record(claimID)}})
		if int64(len(data)) != fi.Size() || os.WriteFile(path, data, 0o644) != nil || os.Chtimes(path, fi.ModTime(), fi.ModTime()) != nil {
			return `{"result":"unavailable"}`
		}
		return `{"result":"taken","claim":"` + claimID + `"}`
	})
	cf.deliver(t, "m1", "message.created", dmMessage("m1", claimText))
	lines := cf.turn(t, "m1")
	if !strings.Contains(lines[0], "| owner") || lines[len(lines)-1] != claimLines["taken"] {
		t.Fatalf("turn = %q", lines)
	}
}

// TestChatOwnerUntouchedMessages: every message that is not code-shaped, or
// not alone in a direct chat from a person who is not the identity, makes
// no knock and gets no line.
func TestChatOwnerUntouchedMessages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		absent bool
		run    func(t *testing.T, cf *claimFixture) string
	}{
		{"in a room", false, func(t *testing.T, cf *claimFixture) string {
			m := dmMessage("r1", claimText)
			m.Name, m.Space = space+"/messages/r1", &api.Space{Name: space, SpaceType: "SPACE"}
			cf.deliver(t, "r1", "message.created", m)
			return "r1"
		}},
		{"with other words", false, func(t *testing.T, cf *claimFixture) string {
			cf.deliver(t, "w1", "message.created", dmMessage("w1", "my code "+claimText))
			return "w1"
		}},
		{"with a newline after it", false, func(t *testing.T, cf *claimFixture) string {
			cf.deliver(t, "n1", "message.created", dmMessage("n1", claimCode+"\n"))
			return "n1"
		}},
		{"of the join's shape", false, func(t *testing.T, cf *claimFixture) string {
			cf.deliver(t, "j1", "message.created", dmMessage("j1", "ABCDEFGHJK"))
			return "j1"
		}},
		{"from the identity", false, func(t *testing.T, cf *claimFixture) string {
			m := dmMessage("i1", claimText)
			m.Sender.Name = identity
			cf.deliver(t, "i1", "message.created", m)
			return ""
		}},
		{"from a bot", false, func(t *testing.T, cf *claimFixture) string {
			m := dmMessage("b1", claimText)
			m.Sender.Type = "BOT"
			cf.deliver(t, "b1", "message.created", m)
			return "b1"
		}},
		{"from a deleted user", false, func(t *testing.T, cf *claimFixture) string {
			m := dmMessage("d1", claimText)
			m.Sender.IsAnonymous = true
			cf.deliver(t, "d1", "message.created", m)
			return "d1"
		}},
		{"with an attachment", false, func(t *testing.T, cf *claimFixture) string {
			m := dmMessage("a1", claimText)
			m.Attachment = []*api.Attachment{{Name: m.Name + "/attachments/x", ContentName: "x.txt"}}
			cf.deliver(t, "a1", "message.created", m)
			return "a1"
		}},
		{"an edit into the code", false, func(t *testing.T, cf *claimFixture) string {
			cf.deliver(t, "e1", "message.created", dmMessage("e1", "hello"))
			m := dmMessage("e1", claimText)
			m.LastUpdateTime = "2026-10-22T10:01:00Z"
			cf.deliver(t, "e1-edit", "message.updated", m)
			return ""
		}},
		{"a catch-up snapshot edited into the code", false, func(t *testing.T, cf *claimFixture) string {
			m := dmMessage("c1", claimText)
			m.LastUpdateTime = "2026-10-22T10:01:00Z"
			if out, err := cf.p.HandleChange(context.Background(), puller.Change{Kind: "message.created", Message: m, Space: dmSpace, PublishTime: time.Now()}); out != puller.Ack || err != nil {
				t.Fatalf("catch-up = %v, %v", out, err)
			}
			return ""
		}},
		{"where root's directory is absent", true, func(t *testing.T, cf *claimFixture) string {
			cf.deliver(t, "x1", "message.created", dmMessage("x1", claimText))
			return "x1"
		}},
		{"while the identity is unknown", false, func(t *testing.T, cf *claimFixture) string {
			// Inbound holds every message until it knows the identity, so it
			// can vouch that the sender is not the identity's own account.
			cf.opts.IdentityUserID = ""
			p, err := New(cf.opts)
			if err != nil {
				t.Fatal(err)
			}
			cf.p = p
			ev := &puller.Event{PubSubID: "u1", Changes: []puller.Change{{Kind: "message.created", Message: dmMessage("u1", claimText), Space: dmSpace, PublishTime: time.Now()}}}
			if out, err := cf.p.Handle(context.Background(), ev); out == puller.Ack && err == nil {
				t.Fatal("a message was taken while the identity is unknown")
			}
			return ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cf := setupClaim(t, nil, tc.absent)
			cf.door.Answer(func(map[string]string) string { return `{"result":"taken","claim":"` + claimID + `"}` })
			id := tc.run(t, cf)
			if got := cf.door.Requests(); len(got) != 0 {
				t.Fatalf("knocked for an untouched message: %v", got)
			}
			for _, w := range cf.gc.Inbound() {
				for _, line := range claimLines {
					if strings.Contains(w.Text, line) {
						t.Fatalf("an untouched turn has a fixed line: %q", w.Text)
					}
				}
			}
			if id != "" {
				cf.turn(t, id)
			}
			if len(cf.claimLogs(t)) != 0 {
				t.Fatal("a claim line for an untouched message")
			}
			cf.noOwnerInState(t)
		})
	}
}

// TestChatOwnerConflictedIdentityMakesNoKnock: while Chat has shown another
// account than the identity the adapter holds, the adapter cannot vouch
// that a sender is not the mayor's own account, and it does not knock.
func TestChatOwnerConflictedIdentityMakesNoKnock(t *testing.T) {
	cf := setupClaim(t, nil, false)
	cf.door.Answer(func(map[string]string) string { return `{"result":"taken","claim":"` + claimID + `"}` })
	if err := cf.opts.Chat.SetIdentityUserID(identity); err != nil {
		t.Fatal(err)
	}
	_ = cf.opts.Chat.SetIdentityUserID(claimSender) // Chat now says the account is another
	if _, conflicted := cf.opts.Chat.ObservedIdentityUserID(); !conflicted {
		t.Fatal("the fixture did not make a conflict")
	}
	ev := &puller.Event{PubSubID: "m1", Changes: []puller.Change{{Kind: "message.created", Message: dmMessage("m1", claimText), Space: dmSpace, PublishTime: time.Now()}}}
	_, _ = cf.p.Handle(context.Background(), ev)
	if got := cf.door.Requests(); len(got) != 0 {
		t.Fatalf("knocked with a conflicted identity: %v", got)
	}
}

// TestChatOwnerCatchUpClaimTurnIsItsOwn: a knocked message is not merged
// with its neighbours, so its fixed line stays the last line of its own turn.
func TestChatOwnerCatchUpClaimTurnIsItsOwn(t *testing.T) {
	cf := setupClaim(t, nil, false)
	if _, err := toml.Decode(`coalesce_window = "8s"`, &cf.opts.Config.Settings); err != nil {
		t.Fatal(err)
	}
	p, err := New(cf.opts)
	if err != nil {
		t.Fatal(err)
	}
	cf.p = p
	cf.door.Answer(func(map[string]string) string { return `{"result":"not_live"}` })
	// Catch-up hands its pass over as one batch, the way it merges.
	batch := []puller.Change{
		{Kind: "message.created", Message: dmMessage("c1", claimText), Space: dmSpace, PublishTime: time.Now()},
		{Kind: "message.created", Message: dmMessage("c2", "and one more thing"), Space: dmSpace, PublishTime: time.Now()},
	}
	if out, err := cf.p.HandleChanges(context.Background(), batch); out != puller.Ack || err != nil {
		t.Fatalf("catch-up = %v, %v", out, err)
	}
	if err := cf.p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range cf.gc.Inbound() {
		lines := strings.Split(w.Text, "\n")
		for i, line := range lines {
			if line == claimLines["not_live"] {
				found = true
				if i != len(lines)-1 {
					t.Fatalf("the fixed line is not the last line of its turn: %q", lines)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no turn carries the fixed line: %+v", cf.gc.Inbound())
	}
}

// TestChatOwnerCatchUpClaimKeepsItsPlace: a knocked catch-up message is
// delivered after the messages before it and before the messages after it.
func TestChatOwnerCatchUpClaimKeepsItsPlace(t *testing.T) {
	cf := setupClaim(t, nil, false)
	if _, err := toml.Decode(`coalesce_window = "8s"`, &cf.opts.Config.Settings); err != nil {
		t.Fatal(err)
	}
	p, err := New(cf.opts)
	if err != nil {
		t.Fatal(err)
	}
	cf.p = p
	cf.door.Answer(func(map[string]string) string { return `{"result":"not_live"}` })
	a, c, b := dmMessage("a1", "first"), dmMessage("c1", claimText), dmMessage("b1", "last")
	a.CreateTime, c.CreateTime, b.CreateTime = "2026-10-22T10:00:00Z", "2026-10-22T10:00:01Z", "2026-10-22T10:00:02Z"
	batch := []puller.Change{
		{Kind: "message.created", Message: a, Space: dmSpace, PublishTime: time.Now()},
		{Kind: "message.created", Message: c, Space: dmSpace, PublishTime: time.Now()},
		{Kind: "message.created", Message: b, Space: dmSpace, PublishTime: time.Now()},
	}
	if out, err := cf.p.HandleChanges(context.Background(), batch); out != puller.Ack || err != nil {
		t.Fatalf("catch-up = %v, %v", out, err)
	}
	if err := cf.p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	at := map[string]int{}
	for i, w := range cf.gc.Inbound() {
		for _, word := range []string{"first", claimLines["not_live"], "last"} {
			if strings.Contains(w.Text, word) {
				at[word] = i
			}
		}
	}
	if len(at) != 3 || !(at["first"] < at[claimLines["not_live"]] && at[claimLines["not_live"]] < at["last"]) {
		t.Fatalf("delivery order %v: %+v", at, cf.gc.Inbound())
	}
}

// TestChatOwnerCatchUpCreationKnocks: a message first seen by catch-up is a
// new message; the door judges it by root's clock.
func TestChatOwnerCatchUpCreationKnocks(t *testing.T) {
	cf := setupClaim(t, nil, false)
	cf.door.Answer(func(map[string]string) string { return `{"result":"not_live"}` })
	m := dmMessage("c2", claimText)
	if out, err := cf.p.HandleChange(context.Background(), puller.Change{Kind: "message.created", Message: m, Space: dmSpace, PublishTime: time.Now()}); out != puller.Ack || err != nil {
		t.Fatalf("catch-up = %v, %v", out, err)
	}
	if len(cf.door.Requests()) != 1 {
		t.Fatalf("door lines = %v", cf.door.Requests())
	}
	lines := cf.turn(t, "c2")
	if lines[0] != "[catch-up]" || lines[len(lines)-1] != claimLines["not_live"] {
		t.Fatalf("turn = %q", lines)
	}
	if c := cf.l.Changes[dmSpace+"/messages/c2"]; c == nil || !c.FixedLine {
		t.Fatalf("the turn is not marked as carrying the fixed line: %+v", c)
	}
}

// TestChatOwnerDeliveredTwice: one knock for each distinct message. A
// message already recorded does not knock again. A message whose turn was
// not recorded after the door acted knocks again, gets the door's answer for
// a code that is no longer live, and the turn says that line and no other.
func TestChatOwnerDeliveredTwice(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		cf := setupClaim(t, nil, false)
		cf.door.Answer(func(map[string]string) string {
			fakedoor.WriteCopy(t, cf.door.Dir, map[string]owners.Record{claimSender: fakedoor.Record(claimID)})
			return `{"result":"taken","claim":"` + claimID + `"}`
		})
		m := dmMessage("m1", claimText)
		cf.deliver(t, "first", "message.created", m)
		cf.deliver(t, "again", "message.created", dmMessage("m1", claimText))
		if n := len(cf.door.Requests()); n != 1 {
			t.Fatalf("knocks = %d", n)
		}
		if n := len(cf.gc.Inbound()); n != 1 {
			t.Fatalf("turns = %d", n)
		}
	})
	t.Run("not recorded", func(t *testing.T) {
		cf := setupClaim(t, nil, false)
		var taken atomic.Bool
		cf.door.Answer(func(map[string]string) string {
			if taken.Swap(true) {
				return `{"result":"not_live"}`
			}
			fakedoor.WriteCopy(t, cf.door.Dir, map[string]owners.Record{claimSender: fakedoor.Record(claimID)})
			return `{"result":"taken","claim":"` + claimID + `"}`
		})
		failed := false
		cf.p.opts.Save = func(l *ledger.Ledger) error {
			if !failed && l.Changes[dmSpace+"/messages/m1"] != nil {
				failed = true
				return errors.New("disk full")
			}
			return l.Save(cf.opts.LedgerPath)
		}
		ev := &puller.Event{PubSubID: "first", Changes: []puller.Change{{Kind: "message.created", Message: dmMessage("m1", claimText), Space: dmSpace, PublishTime: time.Now()}}}
		if out, err := cf.p.Handle(context.Background(), ev); out == puller.Ack && err == nil {
			t.Fatal("the failed save was acknowledged")
		}
		cf.deliver(t, "again", "message.created", dmMessage("m1", claimText))
		if n := len(cf.door.Requests()); n != 2 {
			t.Fatalf("knocks = %d", n)
		}
		lines := cf.turn(t, "m1")
		if !strings.Contains(lines[0], "| owner") || lines[len(lines)-1] != claimLines["not_live"] {
			t.Fatalf("turn = %q", lines)
		}
		n := 0
		for _, line := range lines {
			for _, fixed := range claimLines {
				if line == fixed {
					n++
				}
			}
		}
		if n != 1 {
			t.Fatalf("fixed lines = %d", n)
		}
	})
}

// TestChatOwnerOneFunctionInbound: the frame of a message and of a reaction
// asks the one owner function: the trusted copy where root's directory is
// present, never the settings there; the settings where it is absent.
func TestChatOwnerOneFunctionInbound(t *testing.T) {
	react := func(t *testing.T, cf *claimFixture, id string) string {
		t.Helper()
		r := &api.Reaction{Name: dmSpace + "/messages/base/reactions/" + id, User: &api.User{Name: claimSender, Type: "HUMAN"}, Emoji: &api.Emoji{Unicode: "👍"}}
		handle(t, cf.fixture, &puller.Event{PubSubID: id, Changes: []puller.Change{{Kind: "reaction.created", Space: dmSpace, Reaction: r}}})
		for _, w := range cf.gc.Inbound() {
			if w.ProviderMessageID == r.Name+"#created" {
				return strings.Split(w.Text, "\n")[0]
			}
		}
		t.Fatalf("no reaction turn %s", id)
		return ""
	}
	first := func(t *testing.T, cf *claimFixture, id string) string {
		t.Helper()
		cf.deliver(t, id, "message.created", dmMessage(id, "hello "+id))
		return cf.turn(t, id)[0]
	}
	t.Run("present", func(t *testing.T) {
		cf := setupClaim(t, []string{claimAddr, "users/2002"}, false)
		base := dmMessage("base", "hi")
		base.Sender.Name = identity
		cf.google.Configure(func(s *fakegoogle.Server) { s.Messages[base.Name] = base })
		fakedoor.WriteCopy(t, cf.door.Dir, map[string]owners.Record{claimSender: fakedoor.Record(claimID)})
		if line := first(t, cf, "a"); !strings.Contains(line, "| owner") {
			t.Fatalf("copy owner framed %q", line)
		}
		if line := react(t, cf, "r1"); !strings.Contains(line, "| owner") {
			t.Fatalf("copy owner's reaction framed %q", line)
		}
		fakedoor.WriteCopy(t, cf.door.Dir, map[string]owners.Record{"users/2002": fakedoor.Record(claimID)})
		if line := first(t, cf, "b"); strings.Contains(line, "| owner") {
			t.Fatalf("a settings' owner framed owner where root's directory is present: %q", line)
		}
		if line := react(t, cf, "r2"); strings.Contains(line, "| owner") {
			t.Fatalf("reaction after the copy changed: %q", line)
		}
		if !strings.Contains(cf.logs.String(), "gchat owners settings ignored") {
			t.Fatal("no settings-ignored line")
		}
	})
	t.Run("present without a trusted copy", func(t *testing.T) {
		cf := setupClaim(t, []string{claimAddr}, false)
		fakedoor.WriteRaw(t, cf.door.Dir, []byte("{}"))
		if line := first(t, cf, "a"); strings.Contains(line, "| owner") {
			t.Fatalf("framed %q", line)
		}
	})
	t.Run("absent", func(t *testing.T) {
		cf := setupClaim(t, []string{claimAddr}, true)
		if line := first(t, cf, "a"); !strings.Contains(line, "| owner") {
			t.Fatalf("settings' owner framed %q", line)
		}
	})
}
