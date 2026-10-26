//go:build !unix

package puller

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQuarantineFailsClosedWithoutUnix(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "quarantine")
	root, err := openDurableDirectory(path)
	if root != nil || err == nil {
		t.Fatalf("unsupported open: root=%v err=%v", root, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unsupported open touched filesystem: %v", err)
	}
	if root, err := openDurableChild(nil, "child"); root != nil || err == nil {
		t.Fatalf("unsupported child: root=%v err=%v", root, err)
	}
	if err := syncDirectory(nil); err == nil {
		t.Fatal("unsupported durability succeeded")
	}
	if store, err := openQuarantine(filepath.Join(base, "inbound")); store != nil || err == nil {
		t.Fatalf("unsupported quarantine: store=%v err=%v", store, err)
	}
}
