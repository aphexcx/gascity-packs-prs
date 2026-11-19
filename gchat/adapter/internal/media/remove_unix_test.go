//go:build unix

package media

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemovalKeepsDebtUntilDirectoryChangesAreDurable(t *testing.T) {
	for _, phase := range []string{"delete_message", "edit_message", "delete_parent", "edit_retry"} {
		t.Run(phase, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "room", "message")
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if phase != "edit_retry" {
				if err = os.WriteFile(filepath.Join(dir, "saved.txt"), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var selection map[string]bool
			if phase == "edit_message" || phase == "edit_retry" {
				selection = map[string]bool{FileID(root, "saved.txt"): true}
			}
			failAt := "message"
			if phase == "delete_parent" {
				failAt = "space"
			}
			syncDirectory := func(f *os.File) error {
				if f.Name() == failAt {
					return errors.New("injected directory sync failure")
				}
				return f.Sync()
			}
			result := removeWithSync(root, "room", "message", selection, syncDirectory)
			if result.Cause != "io" {
				t.Fatalf("cleanup can clear debt before directory persistence: %+v", result)
			}
			// A retry after successful rmdir must still persist its parent even
			// though the message directory is now absent in the live filesystem.
			if phase == "delete_parent" {
				if again := removeWithSync(root, "room", "message", selection, syncDirectory); again.Cause != "io" {
					t.Fatal("retry cleared debt without persisting the missing directory")
				}
			}
			if retry := Remove(root, "room", "message", selection); retry.Cause != "" {
				t.Fatalf("successful synchronization did not settle removal: %+v", retry)
			}
		})
	}
}
