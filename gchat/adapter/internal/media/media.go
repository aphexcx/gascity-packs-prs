// Package media keeps the inbound attachment store bounded. A space member
// chooses how many files a message carries and how large they are, so the
// adapter caps the count per message and the bytes under the media root; it
// never deletes on a schedule, because the agent may still be reading a file
// whose path it was handed.
package media

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Usage returns the bytes of the regular files under root. Symlinks are not
// followed, and a root that does not exist yet is empty rather than an error.
func Usage(root string) (int64, error) {
	if root == "" {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return total, err
}
