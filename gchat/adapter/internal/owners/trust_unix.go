//go:build unix

package owners

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// statKey changes when the copy or its directory is replaced or changed.
type statKey struct {
	dirDev, dirIno, fileDev, fileIno uint64
	dirMode, fileMode                fs.FileMode
	dirUID, fileUID                  uint32
	size, mtime                      int64
}

const (
	reasonAbsent    = "absent"
	reasonUnchanged = "unchanged"
)

// ownedByRoot is the trust rule's owner test: user id 0.
func ownedByRoot(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}

// openDir opens dir itself (never a link) and holds it to the trust rule's
// directory test: a real directory, owned by root, that neither its group nor
// others may write.
func openDir(dir string, isRoot func(fs.FileInfo) bool) (*os.File, fs.FileInfo, string) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		// Systems answer a link here with ELOOP or ENOTDIR; the reason only
		// names the case in the log, and every case is not trusted.
		if fi, e := os.Lstat(dir); e == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return nil, nil, "dir_link"
		}
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil, nil, "dir_absent"
		case errors.Is(err, unix.ENOTDIR):
			return nil, nil, "dir_not_directory"
		}
		return nil, nil, "dir_unreadable"
	}
	d := os.NewFile(uintptr(fd), dir)
	fi, err := d.Stat()
	switch {
	case err != nil:
		d.Close()
		return nil, nil, "dir_unreadable"
	case !fi.IsDir():
		d.Close()
		return nil, nil, "dir_not_directory"
	case isRoot == nil || !isRoot(fi):
		d.Close()
		return nil, nil, "dir_owner"
	case fi.Mode().Perm()&0o022 != 0:
		d.Close()
		return nil, nil, "dir_mode"
	}
	return d, fi, ""
}

// readTrusted reads dir/name through one open file under the trust rule:
// a regular file, not a link, owned by root, that neither its group nor
// others may write, at most limit bytes, in a directory that passes openDir.
// It answers reasonUnchanged without reading when unchanged accepts the key.
func readTrusted(dir, name string, limit int64, isRoot func(fs.FileInfo) bool, unchanged func(statKey) bool) (statKey, []byte, string) {
	d, dfi, why := openDir(dir, isRoot)
	if why != "" {
		return statKey{}, nil, why
	}
	defer d.Close()
	fd, err := unix.Openat(int(d.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return statKey{}, nil, reasonAbsent
		case errors.Is(err, unix.ELOOP):
			return statKey{}, nil, "link"
		}
		return statKey{}, nil, "unreadable"
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dir, name))
	defer f.Close()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return statKey{}, nil, "unreadable"
	case fi.Mode()&fs.ModeSymlink != 0:
		return statKey{}, nil, "link"
	case !fi.Mode().IsRegular():
		return statKey{}, nil, "not_regular"
	case isRoot == nil || !isRoot(fi):
		return statKey{}, nil, "owner"
	case fi.Mode().Perm()&0o022 != 0:
		return statKey{}, nil, "mode"
	case fi.Size() > limit:
		return statKey{}, nil, "size"
	}
	key := statKey{dirMode: dfi.Mode(), fileMode: fi.Mode(), size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	if st, ok := dfi.Sys().(*syscall.Stat_t); ok {
		key.dirDev, key.dirIno, key.dirUID = uint64(st.Dev), uint64(st.Ino), st.Uid
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		key.fileDev, key.fileIno, key.fileUID = uint64(st.Dev), uint64(st.Ino), st.Uid
	}
	if unchanged(key) {
		return key, nil, reasonUnchanged
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return statKey{}, nil, "unreadable"
	}
	if int64(len(data)) > limit {
		return statKey{}, nil, "size"
	}
	return key, data, ""
}

// trustedSocket holds the door's socket to the directory test and finds it
// a socket owned by root; the directory then lets nobody but root replace it.
func trustedSocket(dir string, isRoot func(fs.FileInfo) bool) (string, bool) {
	d, _, why := openDir(dir, isRoot)
	if why != "" {
		return "", false
	}
	d.Close()
	path := filepath.Join(dir, doorName)
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode().Type() != fs.ModeSocket || !isRoot(fi) {
		return "", false
	}
	return path, true
}
