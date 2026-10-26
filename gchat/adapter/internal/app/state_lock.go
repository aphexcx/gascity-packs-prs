package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// StateLock excludes services and offline recovery using the same ledger,
// regardless of the socket gc assigns. The file must never be unlinked: flock
// protects the inode, and an unlinked file would allow a second owner.
type StateLock struct {
	file *os.File
	path string
	mu   sync.Mutex
	lost error
}

var ErrStateLockLost = errors.New("state lock ownership lost; stop the service and recover state before restarting")

type StateLockedError struct{ Socket string }

func (e *StateLockedError) Error() string {
	if e.Socket == "" {
		return "ledger is in use by another state operation; wait for it to finish"
	}
	return fmt.Sprintf("ledger is in use (service socket %q); stop the service before starting another", e.Socket)
}

func LockState(path string) (*StateLock, error) {
	return lockState(path, nil)
}

// afterAcquire lets tests replace the pathname at the acquisition boundary.
func lockState(path string, afterAcquire func()) (*StateLock, error) {
	path += ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := openStateLock(path)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := tryStateLock(f); err != nil {
		defer f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			data, readErr := io.ReadAll(io.LimitReader(f, 4096))
			if readErr != nil {
				return nil, fmt.Errorf("read state lock: %w", readErr)
			}
			return nil, &StateLockedError{Socket: strings.TrimSpace(string(data))}
		}
		return nil, fmt.Errorf("acquire state lock: %w", err)
	}
	if afterAcquire != nil {
		afterAcquire()
	}
	owned, err := f.Stat()
	if err == nil {
		current, statErr := os.Lstat(path)
		err = statErr
		if err == nil && (!owned.Mode().IsRegular() || !os.SameFile(owned, current)) {
			err = errors.New("state lock path changed or is not a regular file")
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	l := &StateLock{file: f, path: path}
	// An offline command holding the lock must not advertise a stale service.
	if err := l.RecordSocket(""); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Check permanently revokes this owner when the pathname no longer names the
// locked inode. Returning that inode later cannot establish exclusive ownership.
func (l *StateLock) Check() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost != nil {
		return l.lost
	}
	owned, err := l.file.Stat()
	if err == nil {
		var current os.FileInfo
		current, err = os.Lstat(l.path)
		if err == nil && (!current.Mode().IsRegular() || !os.SameFile(owned, current)) {
			err = ErrStateLockLost
		}
	}
	if err != nil {
		l.lost = ErrStateLockLost
	}
	return l.lost
}

func (l *StateLock) RecordSocket(socket string) error {
	if err := l.Check(); err != nil {
		return err
	}
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	if _, err := l.file.WriteAt([]byte(socket+"\n"), 0); err != nil {
		return err
	}
	return l.file.Sync()
}

func (l *StateLock) Close() error { return l.file.Close() }

// RemoveSocket removes only the socket inspected by the caller.
func RemoveSocket(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSocket == 0 || !os.SameFile(expected, current) {
		return fmt.Errorf("socket path %s changed", path)
	}
	return os.Remove(path)
}
