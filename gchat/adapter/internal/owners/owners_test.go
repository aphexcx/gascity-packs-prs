package owners_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakedoor"
	"gchat-adapter/internal/owners"
)

// All ids are made up: users/1001 and the like, claims of one repeated digit.
const (
	claimA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	claimB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func book(dir string, isRoot func(fs.FileInfo) bool, settings []string, log *slog.Logger) *owners.Book {
	return owners.ForTest(owners.TestOptions{Settings: settings, Dir: dir, IsRoot: isRoot, Log: log, Wait: 200 * time.Millisecond})
}

func logLines(buf *bytes.Buffer, msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) == nil && v["msg"] == msg {
			out = append(out, v)
		}
	}
	return out
}

func copyJSON(owners map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"version": 1, "owners": owners})
	return b
}

func record() map[string]any {
	return map[string]any{"since": "2026-01-02T03:04:05Z", "by": "page-code", "claim": claimA}
}

// TestChatOwnerCopyTrustRule: one row that passes, then one row for each
// condition of the trust rule failing by itself. A copy that fails is no
// copy, the box then has no owner, and one log line says so.
func TestChatOwnerCopyTrustRule(t *testing.T) {
	notDir := func(fi fs.FileInfo) bool { return !fi.IsDir() && fakedoor.Root(fi) }
	onlyDir := func(fi fs.FileInfo) bool { return fi.IsDir() && fakedoor.Root(fi) }
	good := copyJSON(map[string]any{"users/1001": record()})
	many := map[string]any{}
	for i := range owners.MaxOwners + 1 {
		many[fmt.Sprintf("users/%d", 1000+i)] = record()
	}
	for _, tc := range []struct {
		name   string
		isRoot func(fs.FileInfo) bool
		setup  func(t *testing.T, dir string) string
		reason string
	}{
		{"trusted", fakedoor.Root, func(t *testing.T, dir string) string { fakedoor.WriteRaw(t, dir, good); return dir }, ""},
		{"file is a link", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, good)
			must(t, os.Rename(filepath.Join(dir, "owners.json"), filepath.Join(dir, "real.json")))
			must(t, os.Symlink("real.json", filepath.Join(dir, "owners.json")))
			return dir
		}, "link"},
		{"file not owned by root", onlyDir, func(t *testing.T, dir string) string { fakedoor.WriteRaw(t, dir, good); return dir }, "owner"},
		{"group may write the file", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, good)
			must(t, os.Chmod(filepath.Join(dir, "owners.json"), 0o664))
			return dir
		}, "mode"},
		{"others may write the file", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, good)
			must(t, os.Chmod(filepath.Join(dir, "owners.json"), 0o646))
			return dir
		}, "mode"},
		{"file is not regular", fakedoor.Root, func(t *testing.T, dir string) string {
			must(t, syscall.Mkfifo(filepath.Join(dir, "owners.json"), 0o644))
			return dir
		}, "not_regular"},
		{"directory is a link", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, good)
			link := filepath.Join(fakedoor.Dir(t), "link")
			must(t, os.Symlink(dir, link))
			return link
		}, "dir_link"},
		{"directory not owned by root", notDir, func(t *testing.T, dir string) string { fakedoor.WriteRaw(t, dir, good); return dir }, "dir_owner"},
		{"group may write the directory", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, good)
			must(t, os.Chmod(dir, 0o775))
			return dir
		}, "dir_mode"},
		{"others may write the directory", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, good)
			must(t, os.Chmod(dir, 0o757))
			return dir
		}, "dir_mode"},
		{"16385 bytes", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, append(good, bytes.Repeat([]byte(" "), 16385-len(good))...))
			return dir
		}, "size"},
		{"a third key", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, []byte(`{"version":1,"owners":{},"tries":{}}`))
			return dir
		}, "keys"},
		{"a missing key", fakedoor.Root, func(t *testing.T, dir string) string { fakedoor.WriteRaw(t, dir, []byte(`{"version":1}`)); return dir }, "keys"},
		{"a missing key in place of another", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, []byte(`{"version":1,"owner":{}}`))
			return dir
		}, "keys"},
		{"version 2", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, []byte(`{"version":2,"owners":{"users/1001":{"since":"2026-01-02T03:04:05Z","by":"page-code","claim":"`+claimA+`"}}}`))
			return dir
		}, "version"},
		{"not JSON", fakedoor.Root, func(t *testing.T, dir string) string { fakedoor.WriteRaw(t, dir, []byte("users/1001\n")); return dir }, "json"},
		{"a repeated key", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, []byte(`{"version":1,"owners":{},"owners":{"users/1001":{"since":"2026-01-02T03:04:05Z","by":"page-code","claim":"`+claimA+`"}}}`))
			return dir
		}, "json"},
		{"51 owners", fakedoor.Root, func(t *testing.T, dir string) string { fakedoor.WriteRaw(t, dir, copyJSON(many)); return dir }, "too_many"},
		{"an owner id of another shape", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, copyJSON(map[string]any{"person@example.com": record()}))
			return dir
		}, "owner_id"},
		{"a record without since", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, copyJSON(map[string]any{"users/1001": map[string]any{"by": "page-code", "claim": claimA}}))
			return dir
		}, "record"},
		{"a record with another key in place of since", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, copyJSON(map[string]any{"users/1001": map[string]any{"when": "2026-01-02T03:04:05Z", "by": "page-code", "claim": claimA}}))
			return dir
		}, "record"},
		{"a record with a name", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, copyJSON(map[string]any{"users/1001": map[string]any{"since": "2026-01-02T03:04:05Z", "by": "page-code", "claim": claimA, "name": "Example Person"}}))
			return dir
		}, "record"},
		{"a record by another way", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, copyJSON(map[string]any{"users/1001": map[string]any{"since": "2026-01-02T03:04:05Z", "by": "chat", "claim": claimA}}))
			return dir
		}, "record"},
		{"a record with a short claim", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, copyJSON(map[string]any{"users/1001": map[string]any{"since": "2026-01-02T03:04:05Z", "by": "page-code", "claim": "abc"}}))
			return dir
		}, "record"},
		{"a record with a time of another form", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, copyJSON(map[string]any{"users/1001": map[string]any{"since": "2026-01-02T03:04:05.5Z", "by": "page-code", "claim": claimA}}))
			return dir
		}, "record"},
		{"a record that is not an object", fakedoor.Root, func(t *testing.T, dir string) string {
			fakedoor.WriteRaw(t, dir, copyJSON(map[string]any{"users/1001": true}))
			return dir
		}, "record"},
		{"no stand-in at all", nil, func(t *testing.T, dir string) string { fakedoor.WriteRaw(t, dir, good); return dir }, "dir_owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			dir := tc.setup(t, fakedoor.Dir(t))
			b := book(dir, tc.isRoot, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
			owner, any, list := b.Owner("users/1001", ""), b.Any(), b.List()
			lines := logLines(&logs, "gchat owners file")
			if tc.reason == "" {
				if !owner || !any || len(list.Owners) != 1 || len(lines) != 0 {
					t.Fatalf("trusted copy: owner=%v any=%v list=%+v logs=%v", owner, any, list, lines)
				}
				return
			}
			if owner || any || len(list.Owners) != 0 || !list.Root {
				t.Fatalf("untrusted copy made an owner: owner=%v any=%v list=%+v", owner, any, list)
			}
			if len(lines) != 1 || lines[0]["reason"] != tc.reason {
				t.Fatalf("want one log line with reason %q, got %v", tc.reason, lines)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestChatOwnerCopyNotTrustedLogsOncePerMinute: the "gchat owners file" line
// is written at most once a minute.
func TestChatOwnerCopyNotTrustedLogsOncePerMinute(t *testing.T) {
	var logs bytes.Buffer
	dir := fakedoor.Dir(t)
	fakedoor.WriteRaw(t, dir, []byte("not json"))
	now := time.Unix(1000, 0)
	b := owners.ForTest(owners.TestOptions{Dir: dir, IsRoot: fakedoor.Root, Log: slog.New(slog.NewJSONHandler(&logs, nil)), Now: func() time.Time { return now }})
	for range 3 {
		b.Owner("users/1001", "")
	}
	now = now.Add(time.Minute)
	b.Owner("users/1001", "")
	if lines := logLines(&logs, "gchat owners file"); len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
}

// TestChatOwnerCopyReadAgainWhenChanged: the adapter reads the copy again
// whenever the file has changed.
func TestChatOwnerCopyReadAgainWhenChanged(t *testing.T) {
	dir := fakedoor.Dir(t)
	fakedoor.WriteCopy(t, dir, map[string]owners.Record{"users/1001": fakedoor.Record(claimA)})
	b := book(dir, fakedoor.Root, nil, nil)
	if !b.Owner("users/1001", "") || b.Owner("users/1002", "") {
		t.Fatal("first copy")
	}
	fakedoor.WriteCopy(t, dir, map[string]owners.Record{"users/1002": fakedoor.Record(claimB)})
	if b.Owner("users/1001", "") || !b.Owner("users/1002", "") {
		t.Fatal("changed copy was not read again")
	}
	fakedoor.WriteCopy(t, dir, nil)
	if b.Any() || b.Owner("users/1002", "") {
		t.Fatal("emptied copy still names an owner")
	}
	must(t, os.Remove(filepath.Join(dir, "owners.json")))
	if b.Any() {
		t.Fatal("a removed copy still names an owner")
	}
}

// TestChatOwnerReadsAnUnchangedLookingCopy: every answer reads the copy,
// even one rewritten in place with its size and time kept: nothing about
// the file is taken as proof that the owners did not change.
func TestChatOwnerReadsAnUnchangedLookingCopy(t *testing.T) {
	dir := fakedoor.Dir(t)
	fakedoor.WriteCopy(t, dir, map[string]owners.Record{"users/9999": fakedoor.Record(claimA)})
	b := book(dir, fakedoor.Root, nil, nil)
	if !b.Owner("users/9999", "") {
		t.Fatal("first copy")
	}
	rewriteInPlace(t, dir, map[string]owners.Record{"users/1001": fakedoor.Record(claimA)})
	if !b.Owner("users/1001", "") || b.Owner("users/9999", "") || len(b.List().Owners) != 1 || b.List().Owners[0].ID != "users/1001" {
		t.Fatal("a copy rewritten in place was not read again")
	}
}

// rewriteInPlace writes the copy into the same file, keeping its size and
// modification time, so it looks unchanged to anything but a read.
func rewriteInPlace(t *testing.T, dir string, records map[string]owners.Record) {
	t.Helper()
	path := filepath.Join(dir, "owners.json")
	fi, err := os.Stat(path)
	must(t, err)
	data, err := json.Marshal(map[string]any{"version": 1, "owners": records})
	must(t, err)
	if int64(len(data)) != fi.Size() {
		t.Fatalf("rewrite changes the size: %d != %d", len(data), fi.Size())
	}
	must(t, os.WriteFile(path, data, 0o644))
	must(t, os.Chtimes(path, fi.ModTime(), fi.ModTime()))
}

// TestChatOwnerWhereRootIsPresentOrAbsent is C5's two rules: present, the
// trusted copy alone, the settings ignored with one log line; absent, the
// settings by address or Chat id, as before.
func TestChatOwnerWhereRootIsPresentOrAbsent(t *testing.T) {
	settings := []string{"Person@Example.com", "users/2002"}
	t.Run("absent", func(t *testing.T) {
		var logs bytes.Buffer
		b := book(filepath.Join(fakedoor.Dir(t), "absent"), fakedoor.Root, settings, slog.New(slog.NewJSONHandler(&logs, nil)))
		b.NoteSettings()
		if b.Present() || !b.Owner("users/9", "person@example.com") || !b.Owner("users/2002", "") || b.Owner("users/1001", "") || !b.Any() {
			t.Fatal("settings' owners where root's directory is absent")
		}
		list := b.List()
		if list.Root || !reflect.DeepEqual(list.Settings, settings) || len(logLines(&logs, "gchat owners settings ignored")) != 0 {
			t.Fatalf("list = %+v, logs = %s", list, logs.String())
		}
	})
	t.Run("present", func(t *testing.T) {
		var logs bytes.Buffer
		dir := fakedoor.Dir(t)
		fakedoor.WriteCopy(t, dir, map[string]owners.Record{"users/1001": fakedoor.Record(claimA)})
		b := book(dir, fakedoor.Root, settings, slog.New(slog.NewJSONHandler(&logs, nil)))
		b.NoteSettings()
		if b.Owner("users/9", "person@example.com") || b.Owner("users/2002", "") || !b.Owner("users/1001", "") {
			t.Fatal("a settings' owner counted where root's directory is present")
		}
		b.With(settings).Owner("users/2002", "")
		if lines := logLines(&logs, "gchat owners settings ignored"); len(lines) != 1 {
			t.Fatalf("want one settings-ignored line, got %d: %s", len(lines), logs.String())
		}
		if got := b.List(); !got.Root || len(got.Owners) != 1 || got.Owners[0] != (owners.Entry{ID: "users/1001", Since: "2026-01-02T03:04:05Z", By: "page-code", Claim: claimA}) || got.Settings != nil {
			t.Fatalf("list = %+v", got)
		}
	})
	t.Run("present without a copy", func(t *testing.T) {
		b := book(fakedoor.Dir(t), fakedoor.Root, settings, nil)
		if b.Any() || b.Owner("users/2002", "") || len(b.List().Owners) != 0 {
			t.Fatal("no copy is no owner")
		}
	})
	t.Run("present as something that is not a directory", func(t *testing.T) {
		path := filepath.Join(fakedoor.Dir(t), "name")
		must(t, os.WriteFile(path, nil, 0o644))
		b := book(path, fakedoor.Root, settings, nil)
		if !b.Present() || b.Any() || b.Owner("users/2002", "") {
			t.Fatal("anything with the name counts as present, with no owner")
		}
	})
}

// TestChatOwnerPathIsFixed: no setting, environment variable or flag moves
// the path or relaxes the rule. Each candidate name is set, and the book the
// adapter builds still reads root's fixed directory with its fixed bound.
func TestChatOwnerPathIsFixed(t *testing.T) {
	dir := fakedoor.Dir(t)
	for _, name := range []string{"GCHAT_OWNER_CLAIM_FILE", "GCHAT_OWNER_CLAIM_DIR", "GCHAT_OWNERS_FILE", "GCHAT_OWNERS_COPY",
		"GCHAT_OWNER_DOOR", "GCHAT_OWNER_DOOR_SOCKET", "GCHAT_OWNERS_DIR", "GC_GCHAT_OWNER_CLAIM_DIR", "GCHAT_OWNER_TRUST", "GCHAT_OWNERS_TRUST_UID"} {
		t.Setenv(name, dir)
	}
	b := owners.New([]string{"users/2002"}, nil)
	if owners.Dir != "/run/gchat-owner-claim" || b.Dir() != owners.Dir || b.With(nil).Dir() != owners.Dir {
		t.Fatalf("dir = %q", b.Dir())
	}
	if owners.DoorWait != 2*time.Second || b.Wait() != owners.DoorWait {
		t.Fatalf("wait = %v", b.Wait())
	}
}

// TestChatOwnerDoorLines: each request is one JSON line of exactly its keys,
// and only an answer of the door's line form is taken; anything else is no
// answer, as is a door that is silent past the bound.
func TestChatOwnerDoorLines(t *testing.T) {
	door := fakedoor.New(t)
	b := door.Book(nil, nil)
	ctx := context.Background()
	door.Answer(func(r map[string]string) string {
		switch r["request"] {
		case "take":
			return `{"result":"taken","claim":"` + claimA + `"}`
		case "remove":
			return `{"result":"removed"}`
		}
		return `{"result":"here"}`
	})
	if a := b.Take(ctx, "ABCDEFGHJKLM", "users/1001", "spaces/AAA/messages/BBB"); a != (owners.Answer{Result: "taken", Claim: claimA}) {
		t.Fatalf("take = %+v", a)
	}
	if a := b.Remove(ctx, "users/1001", "spaces/AAA/messages/CCC"); a.Result != "removed" {
		t.Fatalf("remove = %+v", a)
	}
	if !b.Here(ctx) {
		t.Fatal("here")
	}
	want := []map[string]string{
		{"request": "take", "code": "ABCDEFGHJKLM", "sender": "users/1001", "message": "spaces/AAA/messages/BBB"},
		{"request": "remove", "owner": "users/1001", "message": "spaces/AAA/messages/CCC"},
		{"request": "here"},
	}
	if got := door.Requests(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %v", got)
	}
	// Fields of another shape are never sent.
	b.Take(ctx, "ABCDEFGHJKL1", "users/1001", "spaces/AAA/messages/BBB")
	b.Take(ctx, "ABCDEFGHJKLM", "users/x", "spaces/AAA/messages/BBB")
	b.Remove(ctx, "users/1001", "spaces/AAA/messages/../x")
	if n := len(door.Requests()); n != 3 {
		t.Fatalf("a malformed request reached the door: %d lines", n)
	}
	for _, tc := range []struct{ name, answer, want string }{
		{"taken without claim", `{"result":"taken"}`, ""},
		{"claim of another shape", `{"result":"taken","claim":"AAAA"}`, ""},
		{"another key", `{"result":"wrong","claim":"` + claimA + `","sender":"users/1"}`, ""},
		{"not_live with a claim", `{"result":"not_live","claim":"` + claimA + `"}`, ""},
		{"an unknown result", `{"result":"owner"}`, ""},
		{"not JSON", `taken`, ""},
		{"too long", `{"result":"refused"}` + strings.Repeat(" ", 600), ""},
		{"silent", fakedoor.Silent, ""},
		{"refused", `{"result":"refused"}`, "refused"},
		{"unavailable", `{"result":"unavailable"}`, "unavailable"},
		{"full", `{"result":"full","claim":"` + claimA + `"}`, "full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			door.Answer(func(map[string]string) string { return tc.answer })
			if a := b.Take(ctx, "ABCDEFGHJKLM", "users/1001", "spaces/AAA/messages/BBB"); a.Result != tc.want {
				t.Fatalf("answer = %+v", a)
			}
		})
	}
	door.Answer(func(map[string]string) string { return `{"result":"unavailable"}` })
	if b.Here(ctx) {
		t.Fatal("here on unavailable")
	}
	// A socket in a directory that fails the trust rule is never called.
	untrusted := owners.ForTest(owners.TestOptions{Dir: door.Dir, IsRoot: func(fs.FileInfo) bool { return false }, Wait: 200 * time.Millisecond})
	before := len(door.Requests())
	if untrusted.Here(ctx) || len(door.Requests()) != before {
		t.Fatal("called a door in an untrusted directory")
	}
	// No door at all.
	if book(fakedoor.Dir(t), fakedoor.Root, nil, nil).Here(ctx) {
		t.Fatal("here without a door")
	}
}

// TestChatOwnerCodeShape: spaces and hyphens removed and letters made upper
// case, exactly 12 characters of the code's alphabet. Anything else is not
// code-shaped (C4); a newline or a tab stays a character.
func TestChatOwnerCodeShape(t *testing.T) {
	for _, tc := range []struct {
		text, want string
		ok         bool
	}{
		{"ABCD-EFGH-JKLM", "ABCDEFGHJKLM", true},
		{"abcd efgh jklm", "ABCDEFGHJKLM", true},
		{" AbCd-EfGh-JkLm ", "ABCDEFGHJKLM", true},
		{"ABCDEFGHJKLM", "ABCDEFGHJKLM", true},
		{"ABCDEFGHJKLM\n", "", false},
		{"\nABCDEFGHJKLM", "", false},
		{"ABCD\tEFGH\tJKLM", "", false},
		{"ABCDEFGHJK", "", false},
		{"ABCDEFGHJKLMN", "", false},
		{"ABCDEFGHJKL0", "", false},
		{"ABCDEFGHJKLI", "", false},
		{"ABCDEFGHJKLſ", "", false},
		{"code ABCDEFGHJKLM", "", false},
		{"", "", false},
	} {
		got, ok := owners.CodeShaped(tc.text)
		if got != tc.want || ok != tc.ok {
			t.Errorf("CodeShaped(%q) = %q, %v", tc.text, got, ok)
		}
	}
}
