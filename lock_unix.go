//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const lockFileName = ".lock"

// lockConfigDir serializes every process that touches the config dir: refresh tokens rotate on use,
// so two processes refreshing the same account at once would leave one of them holding a dead token.
func lockConfigDir(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock config dir: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
