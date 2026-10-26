//go:build unix

package chat

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
)

// openMediaFile walks with directory descriptors and O_NOFOLLOW, so a concurrent
// symlink replacement below the configured root cannot redirect a checked path.
// The trusted root may have symlinked ancestors (e.g. /var on macOS); resolve it
// once and anchor all untrusted relative components to its open descriptor.
func openMediaFile(root, path string, flags int) (*os.File, *os.File, error) {
	if root == "" {
		return nil, nil, errors.New("media root is required")
	}
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return nil, nil, errors.New("media path contains parent traversal")
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, nil, errors.New("media path is outside its configured root")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil, err
	}
	fd, err := unix.Open(resolvedRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if part == "." {
			continue
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if openErr != nil {
			return nil, nil, openErr
		}
		fd = next
	}
	parent := os.NewFile(uintptr(fd), filepath.Dir(path))
	fileFD, err := unix.Openat(fd, filepath.Base(path), flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		parent.Close()
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fileFD), path)
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("media path is not a regular file")
	}
	if err != nil {
		file.Close()
		parent.Close()
		return nil, nil, err
	}
	return file, parent, nil
}

func removeMediaFile(parent *os.File, name string) error {
	return unix.Unlinkat(int(parent.Fd()), name, 0)
}
