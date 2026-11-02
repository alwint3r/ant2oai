package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxCredentialFileBytes = 1 << 20

type oauthCredentials struct {
	Issuer       string    `json:"issuer"`
	ClientID     string    `json:"client_id"`
	Subject      string    `json:"subject,omitempty"`
	Email        string    `json:"email,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes,omitempty"`
}

func (credentials oauthCredentials) hasScope(wanted string) bool {
	for _, scope := range credentials.Scopes {
		if scope == wanted {
			return true
		}
	}
	return false
}

func (credentials oauthCredentials) withoutTokens() oauthCredentials {
	credentials.AccessToken = ""
	credentials.RefreshToken = ""
	credentials.IDToken = ""
	credentials.ExpiresAt = time.Time{}
	credentials.Scopes = nil
	return credentials
}

type oauthStore struct {
	HostID   string                      `json:"ext_agent_host_id"`
	Accounts map[string]oauthCredentials `json:"accounts"`
}

func (store oauthStore) withAccount(label string, credentials oauthCredentials) oauthStore {
	// Published stores are immutable. A refresh builds a replacement snapshot.
	updated := oauthStore{HostID: store.HostID, Accounts: make(map[string]oauthCredentials)}
	for name, account := range store.Accounts {
		updated.Accounts[name] = account
	}
	updated.Accounts[label] = credentials
	return updated
}

func credentialPath(cfg config) (string, error) {
	if cfg.authFile != "" {
		return cfg.authFile, nil
	}
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", errors.New("cannot find the OS config directory; set -auth-file")
	}
	return filepath.Join(directory, "ant2oai", "chatgpt.json"), nil
}

type credentialLock struct {
	path string
}

// Hold this lock for the entire login command or proxy lifetime. Rotating
// refresh tokens must have exactly one process writing the credential file.
func lockCredentials(path string) (*credentialLock, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, fmt.Errorf("create credential directory: %w", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect credential directory: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("credential directory must not be writable by other users")
	}

	lockPath := path + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil, errors.New("credential file is locked; stop the other proxy or login command. " +
			"If it crashed, verify that it has stopped before removing the .lock file")
	}
	if err != nil {
		return nil, fmt.Errorf("lock credentials: %w", err)
	}
	_, writeError := fmt.Fprintf(file, "pid=%d\n", os.Getpid())
	closeError := file.Close()
	if writeError != nil || closeError != nil {
		removeError := os.Remove(lockPath)
		return nil, fmt.Errorf("write credential lock: %w",
			errors.Join(writeError, closeError, removeError))
	}
	return &credentialLock{path: lockPath}, nil
}

func (lock *credentialLock) close() error {
	return os.Remove(lock.path)
}

func readOAuthStore(path string) (oauthStore, error) {
	var store oauthStore
	before, err := os.Lstat(path)
	if err != nil {
		return store, err
	}
	if !before.Mode().IsRegular() {
		return store, errors.New("credential file must be a regular file without a symlink")
	}
	if runtime.GOOS != "windows" && before.Mode().Perm()&0077 != 0 {
		return store, errors.New("credential file requires owner-only permissions; use chmod 600")
	}
	file, err := os.Open(path)
	if err != nil {
		return store, fmt.Errorf("open credentials: %w", err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return store, errors.New("credential file changed while it was being opened")
	}

	raw, err := io.ReadAll(io.LimitReader(file, maxCredentialFileBytes+1))
	if err != nil || len(raw) > maxCredentialFileBytes || checkJSONDepth(raw) != nil {
		return store, errors.New("credential file could not be read within its size and nesting limits")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var trailing any
	if decoder.Decode(&store) != nil || decoder.Decode(&trailing) != io.EOF {
		return store, errors.New("credential file contains invalid or unsupported JSON")
	}
	if len(store.HostID) != 45 || !strings.HasPrefix(store.HostID, "urn:uuid:") ||
		store.Accounts == nil || len(store.Accounts) > 32 {
		return store, errors.New("credential file contains invalid host or account data")
	}
	for _, account := range store.Accounts {
		if account.Issuer != chatGPTIssuer || !validIssuedClientID(account.ClientID) ||
			!validOAuthSecret(account.AccessToken) || !validOAuthSecret(account.RefreshToken) ||
			!validOAuthSecret(account.IDToken) || len(account.Subject) > 256 ||
			len(account.Scopes) > 64 {
			return store, errors.New("credential file contains invalid registration or token data")
		}
		if account.AccessToken != "" && (account.Subject == "" || account.ExpiresAt.IsZero()) {
			return store, errors.New("credential file is missing verified identity or expiry data")
		}
	}
	return store, nil
}

func newOAuthStore() (oauthStore, error) {
	var identifier [16]byte
	if _, err := rand.Read(identifier[:]); err != nil {
		return oauthStore{}, errors.New("could not generate the OAuth host identifier")
	}
	identifier[6] = identifier[6]&0x0f | 0x40
	identifier[8] = identifier[8]&0x3f | 0x80
	text := hex.EncodeToString(identifier[:])
	hostID := "urn:uuid:" + text[:8] + "-" + text[8:12] + "-" + text[12:16] +
		"-" + text[16:20] + "-" + text[20:]
	return oauthStore{HostID: hostID, Accounts: make(map[string]oauthCredentials)}, nil
}

func writeOAuthStore(path string, store oauthStore) error {
	raw, err := json.MarshalIndent(store, "", "  ")
	if err != nil || len(raw)+1 > maxCredentialFileBytes {
		return errors.New("could not encode credentials within the 1 MiB file limit")
	}

	// CreateTemp starts at 0600. Rename replaces the whole token set together.
	file, err := os.CreateTemp(filepath.Dir(path), ".chatgpt-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary credential file: %w", err)
	}
	temporaryPath := file.Name()
	defer func() {
		if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			// The file contains secrets, so report only the cleanup operation.
			fmt.Fprintln(os.Stderr, "Warning: could not remove a temporary credential file.")
		}
	}()

	_, writeError := file.Write(append(raw, '\n'))
	if writeError == nil {
		writeError = file.Sync()
	}
	closeError := file.Close()
	if writeError != nil || closeError != nil {
		return fmt.Errorf("write credentials: %w", errors.Join(writeError, closeError))
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace credentials: %w", err)
	}

	// Persist the renamed entry on Unix before reporting a successful write.
	if runtime.GOOS != "windows" {
		directory, err := os.Open(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("open credential directory after replacement: %w", err)
		}
		directorySyncError := directory.Sync()
		directoryCloseError := directory.Close()
		if directorySyncError != nil || directoryCloseError != nil {
			return fmt.Errorf("sync credential directory after replacement: %w",
				errors.Join(directorySyncError, directoryCloseError))
		}
	}
	return nil
}

func validIssuedClientID(value string) bool {
	if value == "" || value == "dynamic_agent_client" || len(value) > 256 {
		return false
	}
	for _, character := range []byte(value) {
		if character <= ' ' || character > '~' {
			return false
		}
	}
	return true
}

func validOAuthSecret(value string) bool {
	if len(value) > 64<<10 {
		return false
	}
	for _, character := range []byte(value) {
		if character <= ' ' || character > '~' {
			return false
		}
	}
	return true
}
