//go:build unix

package media

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Remove removes regular entries directly inside exactly one message directory.
// A nil selection means every file (deletion); a non-nil selection contains
// FileIDs owed by an edit. Links, subdirectories and other nonregular entries
// are left for the operator. No path component is followed through a symlink.
func Remove(root, space, message string, selection map[string]bool) Removal {
	if root == "" || !safeSegment(space) || !safeSegment(message) {
		return Removal{Remaining: -1, Cause: "unsafe"}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Removal{Remaining: -1, Cause: "unsafe"}
	}
	room, err := openRemovalDirectory(root, space)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Removal{}
		}
		return Removal{Remaining: -1, Cause: removalCause(err)}
	}
	defer room.Close()
	fd, err := unix.Openat(int(room.Fd()), message, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Removal{}
		}
		return Removal{Remaining: -1, Cause: removalCause(err)}
	}
	dir := os.NewFile(uintptr(fd), "message")
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return Removal{Remaining: -1, Cause: "io"}
	}
	result := Removal{}
	for _, entry := range entries {
		if selection != nil && !selection[FileID(root, entry.Name())] {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(fd, entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			result.Remaining++
			result.Cause = "io"
			continue
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			result.Remaining++
			result.Cause = "unsafe"
			continue
		}
		// unlinkat never follows the leaf, even if it was replaced after stat.
		if err := unix.Unlinkat(fd, entry.Name(), 0); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			result.Remaining++
			result.Cause = "io"
			continue
		}
		result.Removed++
	}
	if selection != nil || result.Cause != "" {
		return result
	}
	// Only remove the empty directory we opened, never a replacement at its name.
	var opened, named unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || unix.Fstatat(int(room.Fd()), message, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || opened.Dev != named.Dev || opened.Ino != named.Ino || named.Mode&unix.S_IFMT != unix.S_IFDIR {
		result.Cause, result.Remaining = "unsafe", -1
		return result
	}
	if err := unix.Unlinkat(int(room.Fd()), message, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, os.ErrNotExist) {
		result.Cause, result.Remaining = "io", -1
	}
	return result
}

func removalCause(err error) string {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return "unsafe"
	}
	return "io"
}

func openRemovalDirectory(root, space string) (*os.File, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range append(strings.Split(strings.TrimPrefix(abs, "/"), "/"), space) {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), "space"), nil
}
