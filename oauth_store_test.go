package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func storeJSON(t *testing.T, store oauthStore) string {
	t.Helper()

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("encode store: %v", err)
	}
	return string(data)
}

func writeRawStore(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(credentialDirectory(t), "chatgpt.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write store: %v", err)
	}
	return path
}

func TestOAuthStoreRoundTrip(t *testing.T) {
	path := filepath.Join(credentialDirectory(t), "chatgpt.json")
	store := testStore(testCredentials())

	if err := writeOAuthStore(path, store); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := readOAuthStore(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(loaded, store) {
		t.Errorf("store = %+v\nwant %+v", loaded, store)
	}
}

func TestWriteOAuthStoreProtectsTheFile(t *testing.T) {
	directory := credentialDirectory(t)
	path := filepath.Join(directory, "chatgpt.json")

	if err := writeOAuthStore(path, testStore(testCredentials())); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "chatgpt.json" {
		t.Errorf("directory contains %v, want only the credential file", entries)
	}

	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWriteOAuthStoreFailures(t *testing.T) {
	t.Run("store over the limit", func(t *testing.T) {
		credentials := testCredentials()
		credentials.IDToken = strings.Repeat("a", maxCredentialFileBytes)
		path := filepath.Join(credentialDirectory(t), "chatgpt.json")

		err := writeOAuthStore(path, testStore(credentials))
		assertError(t, err, "could not encode credentials within the 1 MiB file limit")
	})

	t.Run("missing directory", func(t *testing.T) {
		path := filepath.Join(credentialDirectory(t), "missing", "chatgpt.json")

		err := writeOAuthStore(path, testStore(testCredentials()))
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error = %v, want a missing directory error", err)
		}
	})
}

func TestReadOAuthStoreRejectsInvalidFiles(t *testing.T) {
	const invalidJSON = "credential file contains invalid or unsupported JSON"
	const invalidHost = "credential file contains invalid host or account data"
	const invalidAccount = "credential file contains invalid registration or token data"
	const noIdentity = "credential file is missing verified identity or expiry data"
	const unreadable = "credential file could not be read within its size and nesting limits"

	validStore := func() oauthStore { return testStore(testCredentials()) }
	withCredentials := func(change func(*oauthCredentials)) oauthStore {
		credentials := testCredentials()
		change(&credentials)
		return testStore(credentials)
	}

	manyAccounts := oauthStore{HostID: testHostID, Accounts: map[string]oauthCredentials{}}
	for index := 0; index <= 32; index++ {
		manyAccounts.Accounts[fmt.Sprintf("account-%d", index)] = testCredentials().withoutTokens()
	}

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"malformed", `{`, invalidJSON},
		{"unknown field", `{"unknown":1}`, invalidJSON},
		{"trailing data", storeJSON(t, validStore()) + `{}`, invalidJSON},
		{"over the limit", strings.Repeat(" ", maxCredentialFileBytes+1), unreadable},
		{"deep", strings.Repeat("[", 200), unreadable},
		{"no accounts", `{"ext_agent_host_id":"` + testHostID + `"}`, invalidHost},
		{"short host ID", storeJSON(t, oauthStore{HostID: "urn:uuid:1"}), invalidHost},
		{
			"host ID prefix",
			storeJSON(t, oauthStore{
				HostID:   strings.Replace(testHostID, "urn:uuid:", "uuid:urn:", 1),
				Accounts: map[string]oauthCredentials{},
			}),
			invalidHost,
		},
		{"too many accounts", storeJSON(t, manyAccounts), invalidHost},
		{
			"other issuer",
			storeJSON(t, withCredentials(func(c *oauthCredentials) { c.Issuer = "https://other" })),
			invalidAccount,
		},
		{
			"unissued client ID",
			storeJSON(t, withCredentials(func(c *oauthCredentials) {
				c.ClientID = "dynamic_agent_client"
			})),
			invalidAccount,
		},
		{
			"token with a space",
			storeJSON(t, withCredentials(func(c *oauthCredentials) { c.AccessToken = "a b" })),
			invalidAccount,
		},
		{
			"token over the limit",
			storeJSON(t, withCredentials(func(c *oauthCredentials) {
				c.RefreshToken = strings.Repeat("a", (64<<10)+1)
			})),
			invalidAccount,
		},
		{
			"subject over the limit",
			storeJSON(t, withCredentials(func(c *oauthCredentials) {
				c.Subject = strings.Repeat("a", 257)
			})),
			invalidAccount,
		},
		{
			"too many scopes",
			storeJSON(t, withCredentials(func(c *oauthCredentials) {
				c.Scopes = make([]string, 65)
			})),
			invalidAccount,
		},
		{
			"token without a subject",
			storeJSON(t, withCredentials(func(c *oauthCredentials) { c.Subject = "" })),
			noIdentity,
		},
		{
			"token without an expiry",
			storeJSON(t, withCredentials(func(c *oauthCredentials) {
				c.ExpiresAt = testCredentials().withoutTokens().ExpiresAt
			})),
			noIdentity,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := readOAuthStore(writeRawStore(t, testCase.content))
			assertError(t, err, testCase.want)
		})
	}
}

func TestReadOAuthStoreAcceptsSignedOutAccount(t *testing.T) {
	store := testStore(testCredentials().withoutTokens())

	if _, err := readOAuthStore(writeRawStore(t, storeJSON(t, store))); err != nil {
		t.Errorf("read: %v", err)
	}
}

func TestReadOAuthStoreChecksTheFile(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(credentialDirectory(t), "chatgpt.json")

		_, err := readOAuthStore(path)
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error = %v, want a missing file error", err)
		}
	})

	t.Run("shared file", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows has no Unix permission bits")
		}
		path := writeRawStore(t, storeJSON(t, testStore(testCredentials())))
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatalf("chmod: %v", err)
		}

		_, err := readOAuthStore(path)
		assertError(t, err, "credential file requires owner-only permissions; use chmod 600")
	})

	t.Run("symbolic link", func(t *testing.T) {
		target := writeRawStore(t, storeJSON(t, testStore(testCredentials())))
		link := filepath.Join(filepath.Dir(target), "link.json")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("create symbolic link: %v", err)
		}

		_, err := readOAuthStore(link)
		assertError(t, err, "credential file must be a regular file without a symlink")
	})
}

func TestNewOAuthStore(t *testing.T) {
	store, err := newOAuthStore()
	if err != nil {
		t.Fatalf("newOAuthStore: %v", err)
	}

	// The UUID format is urn:uuid:xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx.
	if len(store.HostID) != 45 || !strings.HasPrefix(store.HostID, "urn:uuid:") {
		t.Fatalf("host ID = %q", store.HostID)
	}
	if store.HostID[23] != '4' {
		t.Errorf("host ID %q is not a version 4 UUID", store.HostID)
	}
	if !strings.ContainsRune("89ab", rune(store.HostID[28])) {
		t.Errorf("host ID %q does not have the RFC 4122 variant", store.HostID)
	}

	// A new store must pass the validation that later reads apply.
	if _, err := readOAuthStore(writeRawStore(t, storeJSON(t, store))); err != nil {
		t.Errorf("read a new store: %v", err)
	}
}

func TestWithAccountKeepsTheOriginalStore(t *testing.T) {
	original := testStore(testCredentials())
	updated := original.withAccount("other", testCredentials().withoutTokens())

	if _, exists := original.Accounts["other"]; exists {
		t.Error("withAccount changed the original store")
	}
	if len(updated.Accounts) != 2 || updated.HostID != original.HostID {
		t.Errorf("updated store = %+v", updated)
	}
}

func TestWithoutTokensKeepsTheRegistration(t *testing.T) {
	credentials := testCredentials().withoutTokens()

	if credentials.ClientID != "app_client" || credentials.Subject != "subject-1" ||
		credentials.Issuer != chatGPTIssuer {
		t.Errorf("registration data was lost: %+v", credentials)
	}
	if credentials.AccessToken != "" || credentials.RefreshToken != "" ||
		credentials.IDToken != "" || !credentials.ExpiresAt.IsZero() || credentials.Scopes != nil {
		t.Errorf("tokens remain: %+v", credentials)
	}
}

func TestCredentialLock(t *testing.T) {
	path := filepath.Join(credentialDirectory(t), "chatgpt.json")

	lock, err := lockCredentials(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	_, err = lockCredentials(path)
	if !errors.Is(err, errCredentialsLocked) {
		t.Fatalf("second lock: %v, want %v", err, errCredentialsLocked)
	}

	if err := lock.close(); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := lockCredentials(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := second.close(); err != nil {
		t.Errorf("release: %v", err)
	}
}

func TestCredentialLockCreatesAPrivateDirectory(t *testing.T) {
	directory := filepath.Join(credentialDirectory(t), "created")

	lock, err := lockCredentials(filepath.Join(directory, "chatgpt.json"))
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer lock.close()

	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0700 {
		t.Errorf("mode = %o, want 700", info.Mode().Perm())
	}
}

func TestCredentialLockRejectsSharedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	directory := credentialDirectory(t)
	if err := os.Chmod(directory, 0777); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := lockCredentials(filepath.Join(directory, "chatgpt.json"))
	assertError(t, err, "credential directory must not be writable by other users")
}

func TestCredentialPath(t *testing.T) {
	t.Run("explicit file", func(t *testing.T) {
		cfg := testConfig()
		cfg.authFile = "/explicit/auth.json"

		path, err := credentialPath(cfg)
		if err != nil {
			t.Fatalf("credentialPath: %v", err)
		}
		if path != "/explicit/auth.json" {
			t.Errorf("path = %q", path)
		}
	})

	t.Run("default file", func(t *testing.T) {
		path, err := credentialPath(testConfig())
		if err != nil {
			t.Skipf("no OS config directory: %v", err)
		}
		suffix := filepath.Join("ant2oai", "chatgpt.json")
		if !strings.HasSuffix(path, suffix) {
			t.Errorf("path = %q, want the suffix %q", path, suffix)
		}
	})

	t.Run("default file is outside the real home directory", func(t *testing.T) {
		path, err := credentialPath(testConfig())
		if err != nil {
			t.Skipf("no OS config directory: %v", err)
		}
		if !strings.HasPrefix(path, testHome) {
			t.Errorf("path = %q, want a path inside the test home %q", path, testHome)
		}
	})
}
