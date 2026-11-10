//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// lockHelperPathVariable tells a child test process which credential file to lock.
const lockHelperPathVariable = "ANT2OAI_TEST_LOCK_HELPER_PATH"

// TestCredentialLockHelperProcess runs only as a child of
// TestCredentialLockSurvivesKilledHolder. It takes the lock, reports it, and
// waits until the parent kills it.
func TestCredentialLockHelperProcess(t *testing.T) {
	path := os.Getenv(lockHelperPathVariable)
	if path == "" {
		t.Skip("runs only as a child process")
	}

	if _, err := lockCredentials(path); err != nil {
		fmt.Println("lock failed:", err)
		os.Exit(1)
	}
	fmt.Println("locked")

	// Standard input stays open until the parent exits, so this blocks until SIGKILL.
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(1)
}

func TestCredentialLockSurvivesKilledHolder(t *testing.T) {
	path := filepath.Join(credentialDirectory(t), "chatgpt.json")

	// Start a child process that holds the lock.
	child := exec.Command(os.Args[0], "-test.run=^TestCredentialLockHelperProcess$")
	child.Env = append(os.Environ(), lockHelperPathVariable+"="+path)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatalf("child stdin: %v", err)
	}
	defer stdin.Close()
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout: %v", err)
	}
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	isChildRunning := true
	defer func() {
		if isChildRunning {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()

	report, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || report != "locked\n" {
		t.Fatalf("child report = %q, %v; want %q", report, err, "locked\n")
	}

	// The lock blocks other processes while its holder is alive.
	_, err = lockCredentials(path)
	if !errors.Is(err, errCredentialsLocked) {
		t.Fatalf("lock while child runs: %v, want %v", err, errCredentialsLocked)
	}

	// SIGKILL skips all cleanup, so the child leaves its lock file behind.
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_ = child.Wait()
	isChildRunning = false
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("lock file after kill: %v, want it left behind", err)
	}

	// The leftover file must not block the next process.
	lock, err := lockCredentials(path)
	if err != nil {
		t.Fatalf("lock after killed holder: %v", err)
	}
	if err := lock.close(); err != nil {
		t.Errorf("release: %v", err)
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file after release: %v, want removed", err)
	}
}
