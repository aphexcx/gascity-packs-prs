//go:build !unix

package puller

import (
	"errors"
	"os"
)

// Fail closed: quarantine depends on Unix directory durability guarantees.
func openDurableDirectory(path string) (*os.Root, error) {
	return nil, errors.New("secure quarantine access requires Unix")
}
func openDurableChild(parent *os.Root, name string) (*os.Root, error) {
	return nil, errors.New("secure quarantine access requires Unix")
}
func syncDirectory(root interface {
	Open(string) (*os.File, error)
}) error {
	return errors.New("secure quarantine durability requires Unix")
}
