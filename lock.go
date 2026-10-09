package main

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// errLocked reports that another run holds the lock, nothing was done
var errLocked = errors.New("another mariadb-backuper run is in progress (the lock is held)")

// fileLock is a flock on a file: the kernel releases it when the process
// dies, so a crash never leaves a stale lock behind
type fileLock struct {
	file *os.File
}

func acquireLock(path string) (*fileLock, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, errors.Join(err, closeErr)
	}
	return &fileLock{file: file}, nil
}

// release drops the lock by closing the file
func (l *fileLock) release() error {
	return l.file.Close()
}

// lockHeld reports whether a backup is running right now, without blocking it
func lockHeld(path string) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()

	err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}
