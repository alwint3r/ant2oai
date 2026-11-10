//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// maxLockAttempts bounds the retries when other processes replace the lock
// file during acquisition. Each retry requires a new concurrent release.
const maxLockAttempts = 5

// credentialLock holds an flock(2) lock on an open lock file. The kernel
// releases the lock when the process exits for any reason, including SIGKILL,
// SIGHUP, and crashes. Therefore a lock file left by a dead process does not
// block a later process.
type credentialLock struct {
	path string
	file *os.File
}

func acquireCredentialLock(path string) (*credentialLock, error) {
	for range maxLockAttempts {
		// O_NOFOLLOW keeps a planted symbolic link from redirecting the lock.
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return nil, fmt.Errorf("open credential lock: %w", err)
		}

		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(errCredentialsLocked, file.Close())
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("lock credentials: %w", err), file.Close())
		}

		// A releasing process removes the path before it unlocks. If that happened
		// after our open, this lock is on a removed file, so open the path again.
		isCurrent, err := isLockedFileAtPath(file, path)
		if err != nil {
			return nil, errors.Join(err, file.Close())
		}
		if !isCurrent {
			if err := file.Close(); err != nil {
				return nil, fmt.Errorf("close replaced credential lock: %w", err)
			}
			continue
		}

		// The process ID helps a person who inspects the file. The lock does not use it.
		if err := writeLockOwner(file); err != nil {
			return nil, errors.Join(err, file.Close())
		}
		return &credentialLock{path: path, file: file}, nil
	}
	return nil, errors.New("credential lock file kept changing; try again")
}

func isLockedFileAtPath(file *os.File, path string) (bool, error) {
	locked, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect credential lock: %w", err)
	}
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect credential lock path: %w", err)
	}
	return os.SameFile(locked, current), nil
}

// writeLockOwner replaces any process ID that an earlier holder left in the file.
func writeLockOwner(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("write credential lock: %w", err)
	}
	if _, err := fmt.Fprintf(file, "pid=%d\n", os.Getpid()); err != nil {
		return fmt.Errorf("write credential lock: %w", err)
	}
	return nil
}

// close removes the path while the lock is still held. A process that opened
// the old file before the removal then detects the change and opens the path
// again. Closing the file releases the lock.
func (lock *credentialLock) close() error {
	removeError := os.Remove(lock.path)
	closeError := lock.file.Close()
	return errors.Join(removeError, closeError)
}
