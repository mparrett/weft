//go:build unix

package store

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive advisory lock, blocking until it is available.
//
// The lock is on the log itself rather than a sidecar, so there is no second
// file whose absence or staleness could desynchronize from the log.
func lockFile(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("locking log: %w", err)
	}
	return nil
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
