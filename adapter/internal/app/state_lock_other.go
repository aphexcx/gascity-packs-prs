//go:build !unix

package app

import (
	"errors"
	"os"
)

var errStateLockUnsupported = errors.New("exclusive ledger locking is unsupported on this platform")

// Platforms without the Unix locking implementation must not run with an
// unprotected ledger. The package still compiles for offline tooling checks.
func openStateLock(string) (*os.File, error) { return nil, errStateLockUnsupported }
func tryStateLock(*os.File) error            { return errStateLockUnsupported }
