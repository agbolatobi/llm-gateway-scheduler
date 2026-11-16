//go:build linux

package auth

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Linux advisory locks are released even if an administration command crashes.
func (keyStore *Store) lock() (func(), error) {
	if err := keyStore.checkDir(); err != nil {
		return nil, err
	}
	path := filepath.Join(keyStore.Dir, ".keys.lock")
	if err := secureFile(path); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		syscall.Close(fd)
		return nil, errors.New("could not open key lock")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = file.Close() }, nil
}
