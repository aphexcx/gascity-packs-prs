//go:build unix

package puller

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Walk from the filesystem root using directory handles, refusing symlinks in
// every component (including existing ancestors). Each opened child must match
// its lstat identity, so a concurrent replacement cannot redirect the walk.
func openDurableDirectory(path string) (*os.Root, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(abs) + string(filepath.Separator)
	root, err := os.OpenRoot(volume)
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.TrimPrefix(abs, volume), string(filepath.Separator)) {
		if name == "" {
			continue
		}
		child, err := openDurableChild(root, name)
		root.Close()
		if err != nil {
			return nil, err
		}
		root = child
	}
	// Check the final directory's durability before accepting any events.
	if err := syncDirectory(root); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

// Sync a parent only when this call created an entry in it. Existing ancestors
// need identity verification but do not need to be opened again for fsync.
func openDurableChild(parent *os.Root, name string) (*os.Root, error) {
	created := false
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		err = parent.Mkdir(name, 0700)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		created = err == nil
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("quarantine path is not a directory (symlinks are refused): %s", name)
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	// Coverage gap: no deterministic swap between Lstat and OpenRoot is
	// injected here. Symlink and between-delivery rotation tests do not pin
	// this identity check; doing so needs an opening seam or filesystem hook.
	if err == nil && !os.SameFile(info, opened) {
		err = fmt.Errorf("quarantine directory changed while opening: %s", name)
	}
	if err == nil && created {
		err = syncDirectory(parent)
	}
	if err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

func syncDirectory(root interface {
	Open(string) (*os.File, error)
}) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
