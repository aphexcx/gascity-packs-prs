//go:build !unix

package chat

import (
	"errors"
	"os"
)

// This adapter is a Unix socket service; never weaken secure file access on other platforms.
func openMediaFile(root, path string, flags int) (*os.File, *os.File, error) {
	return nil, nil, errors.New("secure media access requires Unix")
}
func removeMediaFile(parent *os.File, name string) error {
	return errors.New("secure media removal requires Unix")
}
