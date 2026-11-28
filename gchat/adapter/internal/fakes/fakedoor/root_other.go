//go:build !unix

package fakedoor

import "io/fs"

// Root: where a file's owner cannot be read, nothing stands in for root.
func Root(fs.FileInfo) bool { return false }
