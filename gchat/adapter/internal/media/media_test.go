package media

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUsageCountsRegularFilesWithoutFollowingLinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "spaces", "A", "messages", "M"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "spaces", "A", "messages", "M", "one.txt"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "spaces", "A", "two.txt"), []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A link to a large file outside the store is not ours to count.
	outside := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(outside, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "spaces", "A", "link.bin")); err != nil {
		t.Fatal(err)
	}
	got, err := Usage(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != 8 {
		t.Fatalf("usage=%d, want 8", got)
	}
}

func TestUsageTreatsAMissingRootAsEmpty(t *testing.T) {
	for _, root := range []string{"", filepath.Join(t.TempDir(), "absent")} {
		got, err := Usage(root)
		if got != 0 || err != nil {
			t.Fatalf("Usage(%q)=%d, %v", root, got, err)
		}
	}
}
