package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gchat-adapter/internal/fakes/fakedoor"
	"gchat-adapter/internal/owners"
)

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestChatOwnerClaimCheck: owner-claim-check prints exactly "owner-claim 1"
// and one newline and exits 0 only when the door answers here; it prints
// nothing on stdout and exits non-zero when the door is absent, silent or
// refuses. It reads no setting and writes nothing.
func TestChatOwnerClaimCheck(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		door        bool
		ok          bool
	}{
		{"here", `{"result":"here"}`, true, true},
		{"no door", "", false, false},
		{"silent", fakedoor.Silent, true, false},
		{"refused", `{"result":"refused"}`, true, false},
		{"unavailable", `{"result":"unavailable"}`, true, false},
		{"outside the line form", `{"result":"here","extra":"x"}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := fakedoor.Dir(t)
			if tc.door {
				door := fakedoor.New(t)
				door.Answer(func(map[string]string) string { return tc.reply })
				dir = door.Dir
			}
			old := claimBook
			t.Cleanup(func() { claimBook = old })
			claimBook = func() *owners.Book {
				return owners.ForTest(owners.TestOptions{Dir: dir, IsRoot: fakedoor.Root, Wait: fakedoor.Wait})
			}
			before := listDir(t, dir)
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"owner-claim-check"}, &stdout, &stderr, func(string) string { t.Fatal("owner-claim-check read a setting"); return "" }, nil)
			if tc.ok && (code != 0 || stdout.String() != "owner-claim 1\n") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
			if !tc.ok && (code == 0 || stdout.Len() != 0 || stderr.Len() != 0) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
			if after := listDir(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatalf("it wrote: %v -> %v", before, after)
			}
		})
	}
}

// TestChatOwnerClaimCheckPathIsFixed: no flag and no setting moves the path
// the check word asks: a flag is refused, and with every candidate name set
// the book it builds is root's fixed directory with the fixed bound.
func TestChatOwnerClaimCheckPathIsFixed(t *testing.T) {
	for _, args := range [][]string{{"owner-claim-check", "--dir", "x"}, {"owner-claim-check", "--socket", "x"}, {"owner-claim-check", "x"}} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, &stdout, &stderr, func(string) string { return "" }, nil); code != 2 || stdout.Len() != 0 {
			t.Fatalf("%v: code=%d stdout=%q", args, code, &stdout)
		}
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	for _, name := range []string{"GCHAT_OWNER_CLAIM_FILE", "GCHAT_OWNER_CLAIM_DIR", "GCHAT_OWNERS_FILE", "GCHAT_OWNER_DOOR", "GC_GCHAT_OWNER_CLAIM_DIR", "GCHAT_OWNERS_DIR"} {
		t.Setenv(name, elsewhere)
	}
	if b := claimBook(); b.Dir() != owners.Dir || b.Wait() != owners.DoorWait {
		t.Fatalf("dir=%q wait=%v", b.Dir(), b.Wait())
	}
}
