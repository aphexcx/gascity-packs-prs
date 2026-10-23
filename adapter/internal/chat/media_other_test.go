//go:build !unix

package chat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMediaAccessFailsClosedWithoutUnix(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private")
	file, parent, err := openMediaFile(root, path, os.O_CREATE|os.O_WRONLY)
	if err == nil || file != nil || parent != nil {
		t.Fatalf("unsupported open: file=%v parent=%v err=%v", file, parent, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unsupported open touched filesystem: %v", err)
	}
	if err := removeMediaFile(nil, "private"); err == nil {
		t.Fatal("unsupported removal succeeded")
	}
}
