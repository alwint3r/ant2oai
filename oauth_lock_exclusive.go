//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package main

import (
	"errors"
	"fmt"
	"os"
)

// credentialLock is a lock file that only exclusive creation protects. These
// platforms have no flock(2) in the Go standard library. If the process stops
// without running close, the file remains and blocks later processes until a
// person removes it.
type credentialLock struct {
	path string
}

func acquireCredentialLock(path string) (*credentialLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%w. If it crashed, "+
			"verify that it has stopped before removing the .lock file", errCredentialsLocked)
	}
	if err != nil {
		return nil, fmt.Errorf("lock credentials: %w", err)
	}

	_, writeError := fmt.Fprintf(file, "pid=%d\n", os.Getpid())
	closeError := file.Close()
	if writeError != nil || closeError != nil {
		removeError := os.Remove(path)
		return nil, fmt.Errorf("write credential lock: %w",
			errors.Join(writeError, closeError, removeError))
	}
	return &credentialLock{path: path}, nil
}

func (lock *credentialLock) close() error {
	return os.Remove(lock.path)
}
