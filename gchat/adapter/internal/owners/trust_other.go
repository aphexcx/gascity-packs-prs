//go:build !unix

package owners

import "io/fs"

type statKey struct{}

const (
	reasonAbsent    = "absent"
	reasonUnchanged = "unchanged"
)

// ownedByRoot: a system where a reader cannot read a file's owner trusts no file.
func ownedByRoot(fs.FileInfo) bool { return false }

func readTrusted(string, string, int64, func(fs.FileInfo) bool, func(statKey) bool) (statKey, []byte, string) {
	return statKey{}, nil, "unsupported"
}

func trustedSocket(string, func(fs.FileInfo) bool) (string, bool) { return "", false }
