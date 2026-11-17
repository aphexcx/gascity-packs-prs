package media

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// FileID links an attachment to a directory entry without storing its name or
// local path in the cleanup ledger. Roots are canonical absolute paths, so
// equal basenames in two configured stores retain independent ownership.
func FileID(root, name string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(root+"\x00"+name)))
}

// Removal contains only counts and a closed failure category, never an OS error
// that could disclose a local path. Remaining is -1 when the directory could
// not be inspected; otherwise it counts entries still requiring attention.
type Removal struct {
	Removed   int
	Remaining int
	Cause     string
}

func safeSegment(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}
