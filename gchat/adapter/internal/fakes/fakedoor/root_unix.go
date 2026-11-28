//go:build unix

package fakedoor

import (
	"io/fs"
	"os"
	"syscall"
)

// Root stands in for "owned by user id 0": the test's own user.
func Root(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Getuid())
}
