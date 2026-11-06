package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tokenReply returns a successful token response after applying overrides. A
// nil override removes the field.
func tokenReply(idToken string, overrides map[string]any) string {
	reply := map[string]any{
		"access_token": "access-2", "refresh_token": "refresh-2", "token_type": "Bearer",
		"expires_in": 3600, "scope": "openid offline_access resource.invoke " + chatGPTScope,
	}
	if idToken != "" {
		reply["id_token"] = idToken
	}
	for name, value := range overrides {
		if value == nil {
			delete(reply, name)
			continue
		}
		reply[name] = value
	}

	data, err := json.Marshal(reply)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func refreshToken() string {
	return signToken(validHeader(), withClaims(map[string]any{"nonce": nil}))
}

// expiredCredentials returns credentials that need a refresh. A fixed date keeps
// the result independent of the current time.
func expiredCredentials() oauthCredentials {
	credentials := testCredentials()
	credentials.ExpiresAt = longPast
	return credentials
}

// newTestSession returns a session whose store also exists on disk.
func newTestSession(
	t *testing.T, provider *fakeOAuthProvider, credentials oauthCredentials,
) *oauthSession {
	t.Helper()

	path := filepath.Join(credentialDirectory(t), "chatgpt.json")
	store := testStore(credentials)
	if err := writeOAuthStore(path, store); err != nil {
		t.Fatalf("write store: %v", err)
	}
	return &oauthSession{path: path, account: "default", store: store, oauth: provider.client()}
}

func savedCredentials(t *testing.T, session *oauthSession) oauthCredentials {
	t.Helper()

	store, err := readOAuthStore(session.path)
	if err != nil {
		t.Fatalf("read saved store: %v", err)
	}
	return store.Accounts["default"]
}

func TestAccessTokenWithoutRefresh(t *testing.T) {
	const noPlan = "ChatGPT plan usage is disabled; stop the proxy and run login -reconsent"

	cases := []struct {
		name      string
		change    func(*oauthCredentials)
		wantToken string
		wantError string
	}{
		{name: "valid token", change: func(*oauthCredentials) {}, wantToken: "access-1"},
		{
			name:      "signed out",
			change:    func(c *oauthCredentials) { c.AccessToken = "" },
			wantError: "ChatGPT credentials are unavailable; stop the proxy and run ant2oai login",
		},
		{
			name:      "no plan scope",
			change:    func(c *oauthCredentials) { c.Scopes = []string{"resource.invoke"} },
			wantError: noPlan,
		},
		{
			name:      "no invoke scope",
			change:    func(c *oauthCredentials) { c.Scopes = []string{chatGPTScope} },
			wantError: noPlan,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			credentials := testCredentials()
			testCase.change(&credentials)
			provider := newFakeOAuthProvider()
			session := newTestSession(t, provider, credentials)

			token, err := session.accessToken(context.Background())
			if testCase.wantError != "" {
				assertError(t, err, testCase.wantError)
			} else if err != nil {
				t.Fatalf("accessToken: %v", err)
			}
			if token != testCase.wantToken {
				t.Errorf("token = %q, want %q", token, testCase.wantToken)
			}
			if provider.tokenCallCount() != 0 {
				t.Error("a token that needs no refresh reached the token endpoint")
			}
		})
	}
}

func TestAccessTokenRefreshes(t *testing.T) {
	cases := []struct {
		name  string
		reply func() string
	}{
		{"reply with an ID token", func() string { return tokenReply(refreshToken(), nil) }},
		{"reply without an ID token", func() string { return tokenReply("", nil) }},
		{
			"reply without a scope keeps the old scopes",
			func() string { return tokenReply("", map[string]any{"scope": nil}) },
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newFakeOAuthProvider()
			provider.exchange = func(url.Values) (int, string) {
				return http.StatusOK, testCase.reply()
			}
			session := newTestSession(t, provider, expiredCredentials())

			token, err := session.accessToken(context.Background())
			if err != nil {
				t.Fatalf("accessToken: %v", err)
			}
			if token != "access-2" {
				t.Errorf("token = %q, want access-2", token)
			}

			saved := savedCredentials(t, session)
			if saved.AccessToken != "access-2" || saved.RefreshToken != "refresh-2" {
				t.Errorf("rotated tokens were not saved: %+v", saved)
			}
			if saved.ExpiresAt.Equal(longPast) {
				t.Error("the saved expiry did not change")
			}

			form := provider.tokenForms[0]
			want := url.Values{
				"grant_type": {"refresh_token"}, "client_id": {"app_client"},
				"refresh_token": {"refresh-1"}, "resource": {chatGPTResource},
			}
			if !reflect.DeepEqual(form, want) {
				t.Errorf("token form = %v, want %v", form, want)
			}
		})
	}
}

func TestRefreshedTokenIsReused(t *testing.T) {
	provider := newFakeOAuthProvider()
	provider.exchange = func(url.Values) (int, string) {
		return http.StatusOK, tokenReply("", nil)
	}
	session := newTestSession(t, provider, expiredCredentials())

	if _, err := session.accessToken(context.Background()); err != nil {
		t.Fatalf("first accessToken: %v", err)
	}
	if _, err := session.accessToken(context.Background()); err != nil {
		t.Fatalf("second accessToken: %v", err)
	}
	if provider.tokenCallCount() != 1 {
		t.Errorf("token calls = %d, want 1", provider.tokenCallCount())
	}
}

func TestRefreshFailuresKeepCredentials(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*fakeOAuthProvider)
		want    string
	}{
		{
			"server error",
			func(provider *fakeOAuthProvider) {
				provider.exchange = func(url.Values) (int, string) { return 500, "garbage" }
			},
			"ChatGPT token exchange returned HTTP 500; " +
				"check OpenAI availability and sign in again if needed",
		},
		{
			"rejected client",
			func(provider *fakeOAuthProvider) {
				provider.exchange = func(url.Values) (int, string) {
					return 401, `{"error":"invalid_client"}`
				}
			},
			"OpenAI rejected this client registration; check the saved account registration",
		},
		{
			"token endpoint network failure",
			func(provider *fakeOAuthProvider) { provider.failedPaths["/oauth/token"] = true },
			"could not reach OpenAI for token exchange; check network and TLS settings",
		},
		{
			"discovery failure",
			func(provider *fakeOAuthProvider) { provider.discoveryStatus = 500 },
			"OpenAI OAuth discovery failed; check service availability",
		},
		{
			"key failure",
			func(provider *fakeOAuthProvider) { provider.keysStatus = 500 },
			"OpenAI OAuth discovery failed; check service availability",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newFakeOAuthProvider()
			testCase.prepare(provider)
			session := newTestSession(t, provider, expiredCredentials())

			_, err := session.accessToken(context.Background())
			assertError(t, err, testCase.want)

			saved := savedCredentials(t, session)
			if saved.RefreshToken != "refresh-1" {
				t.Errorf("a temporary failure changed the refresh token: %+v", saved)
			}
		})
	}
}

func TestKeysAreFetchedBeforeTheTokenRotates(t *testing.T) {
	provider := newFakeOAuthProvider()
	provider.keysStatus = http.StatusInternalServerError
	session := newTestSession(t, provider, expiredCredentials())

	if _, err := session.accessToken(context.Background()); err == nil {
		t.Fatal("error = nil")
	}
	if provider.tokenCallCount() != 0 {
		t.Error("the refresh token was spent although verification keys were unavailable")
	}
}

func TestRefreshFailuresClearCredentials(t *testing.T) {
	const expired = "ChatGPT credentials expired or were revoked; " +
		"run ant2oai login for the selected account"
	const invalidTokens = "OpenAI returned an invalid token set; sign in again"

	otherSubject := func() string {
		return signToken(validHeader(), withClaims(map[string]any{
			"nonce": nil, "sub": "someone-else",
		}))
	}

	cases := []struct {
		name   string
		status int
		reply  func() string
		want   string
	}{
		{"invalid grant", 400, func() string { return `{"error":"invalid_grant"}` }, expired},
		{"reused token", 400, func() string { return `{"error":"refresh_token_reused"}` }, expired},
		{"expired token", 401, func() string { return `{"error":"token_expired"}` }, expired},
		{"malformed success", 200, func() string { return `{` }, invalidTokens},
		{
			"no access token", 200,
			func() string { return tokenReply("", map[string]any{"access_token": ""}) },
			invalidTokens,
		},
		{
			"token with a space", 200,
			func() string { return tokenReply("", map[string]any{"access_token": "a b"}) },
			invalidTokens,
		},
		{
			"token type", 200,
			func() string { return tokenReply("", map[string]any{"token_type": "mac"}) },
			invalidTokens,
		},
		{
			"zero lifetime", 200,
			func() string { return tokenReply("", map[string]any{"expires_in": 0}) },
			invalidTokens,
		},
		{
			"lifetime over one day", 200,
			func() string { return tokenReply("", map[string]any{"expires_in": 86401}) },
			invalidTokens,
		},
		{
			"no replacement token", 200,
			func() string {
				return tokenReply("", map[string]any{
					"refresh_token": nil, "scope": "resource.invoke " + chatGPTScope,
				})
			},
			"ChatGPT refresh returned no replacement refresh token; sign in again",
		},
		{
			"offline access without a token", 200,
			func() string { return tokenReply("", map[string]any{"refresh_token": nil}) },
			"OpenAI granted offline access without a refresh token; sign in again",
		},
		{
			"scope over the limit", 200,
			func() string {
				return tokenReply("", map[string]any{"scope": strings.Repeat("a", 8193)})
			},
			"OpenAI token response contains an oversized scope set",
		},
		{
			"too many scopes", 200,
			func() string {
				return tokenReply("", map[string]any{"scope": strings.Repeat("a ", 65)})
			},
			"OpenAI token response exceeds the supported scope limit",
		},
		{
			"invalid ID token", 200, func() string { return tokenReply("not.a.jwt", nil) },
			"OpenAI ID token uses an unsupported JWT header or claims encoding",
		},
		{
			"changed identity", 200, func() string { return tokenReply(otherSubject(), nil) },
			"ChatGPT refresh changed the verified identity; sign in again",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newFakeOAuthProvider()
			provider.exchange = func(url.Values) (int, string) {
				return testCase.status, testCase.reply()
			}
			session := newTestSession(t, provider, expiredCredentials())

			_, err := session.accessToken(context.Background())
			assertError(t, err, testCase.want)

			// The account stays registered, and a later call needs a new sign-in.
			saved := savedCredentials(t, session)
			if saved.AccessToken != "" || saved.RefreshToken != "" {
				t.Errorf("unusable tokens stayed on disk: %+v", saved)
			}
			if saved.ClientID != "app_client" || saved.Subject != "subject-1" {
				t.Errorf("the registration was lost: %+v", saved)
			}
			_, err = session.accessToken(context.Background())
			assertError(t, err,
				"ChatGPT credentials are unavailable; stop the proxy and run ant2oai login")
		})
	}
}

func TestRefreshWithoutRefreshToken(t *testing.T) {
	credentials := expiredCredentials()
	credentials.RefreshToken = ""
	session := newTestSession(t, newFakeOAuthProvider(), credentials)

	_, err := session.accessToken(context.Background())
	assertError(t, err, "this ChatGPT session cannot renew; stop the proxy and run ant2oai login")
}

func TestRefreshRemovesPlanPermission(t *testing.T) {
	provider := newFakeOAuthProvider()
	provider.exchange = func(url.Values) (int, string) {
		return http.StatusOK, tokenReply("", map[string]any{"scope": "offline_access"})
	}
	session := newTestSession(t, provider, expiredCredentials())

	_, err := session.accessToken(context.Background())
	assertError(t, err,
		"ChatGPT plan permission was removed; stop the proxy and run login -reconsent")
}

func TestRefreshPersistenceFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		reply  string
		want   string
	}{
		{
			"rotated tokens cannot be saved", http.StatusOK, tokenReply("", nil),
			"could not clear unusable ChatGPT credentials; " +
				"stop the proxy, check credential storage, and sign in again",
		},
		{
			"unusable tokens cannot be cleared", http.StatusBadRequest, `{"error":"invalid_grant"}`,
			"could not clear unusable ChatGPT credentials; " +
				"stop the proxy, check credential storage, and sign in again",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newFakeOAuthProvider()
			provider.exchange = func(url.Values) (int, string) {
				return testCase.status, testCase.reply
			}
			session := newTestSession(t, provider, expiredCredentials())
			session.path = filepath.Join(filepath.Dir(session.path), "missing", "chatgpt.json")

			_, err := session.accessToken(context.Background())
			assertError(t, err, testCase.want)

			// The rotated token is lost, so memory must not keep the previous tokens.
			if token := session.store.Accounts["default"].AccessToken; token != "" {
				t.Errorf("memory kept the token %q", token)
			}
		})
	}
}

func TestConcurrentCallersShareOneRefresh(t *testing.T) {
	const callers = 16

	provider := newFakeOAuthProvider()
	provider.exchange = func(url.Values) (int, string) {
		return http.StatusOK, tokenReply("", nil)
	}
	session := newTestSession(t, provider, expiredCredentials())

	type result struct {
		token string
		err   error
	}
	results := make(chan result, callers)
	for caller := 0; caller < callers; caller++ {
		go func() {
			token, err := session.accessToken(context.Background())
			results <- result{token, err}
		}()
	}

	for caller := 0; caller < callers; caller++ {
		outcome := <-results
		if outcome.err != nil || outcome.token != "access-2" {
			t.Errorf("result = %q, %v", outcome.token, outcome.err)
		}
	}
	if provider.tokenCallCount() != 1 {
		t.Errorf("token calls = %d, want 1", provider.tokenCallCount())
	}
}

// The next two tests check a shared refresh failure in two halves, and both use
// the private refresh state. A test cannot observe that a goroutine is really
// waiting, so an end-to-end version would depend on goroutine scheduling.

func TestRefreshPublishesItsFailure(t *testing.T) {
	provider := newFakeOAuthProvider()
	session := newTestSession(t, provider, expiredCredentials())

	var pending *oauthRefresh
	provider.exchange = func(url.Values) (int, string) {
		session.mutex.Lock()
		defer session.mutex.Unlock()
		pending = session.refreshing
		return http.StatusInternalServerError, ""
	}

	_, err := session.accessToken(context.Background())
	if err == nil {
		t.Fatal("error = nil")
	}

	<-pending.done
	if !errors.Is(pending.err, err) {
		t.Errorf("published error = %v, want %v", pending.err, err)
	}
	if session.refreshing != nil {
		t.Error("the finished refresh stayed active")
	}
}

func TestWaitingCallerReceivesTheRefreshFailure(t *testing.T) {
	provider := newFakeOAuthProvider()
	session := newTestSession(t, provider, expiredCredentials())

	failure := errors.New("shared refresh failure")
	pending := &oauthRefresh{done: make(chan struct{}), err: failure}
	close(pending.done)
	session.refreshing = pending

	_, err := session.accessToken(context.Background())
	if !errors.Is(err, failure) {
		t.Errorf("error = %v, want %v", err, failure)
	}
	if provider.tokenCallCount() != 0 {
		t.Error("a waiting caller started its own refresh")
	}
}

func TestAccessTokenHonorsCancellation(t *testing.T) {
	const cancelled = "ChatGPT credential request was cancelled"

	t.Run("before the call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		session := newTestSession(t, newFakeOAuthProvider(), testCredentials())

		_, err := session.accessToken(ctx)
		assertError(t, err, cancelled)
	})

	t.Run("while another caller refreshes", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		provider := newFakeOAuthProvider()
		provider.exchange = func(url.Values) (int, string) {
			close(entered)
			<-release
			return http.StatusOK, tokenReply("", nil)
		}
		session := newTestSession(t, provider, expiredCredentials())

		refreshed := make(chan error, 1)
		go func() {
			_, err := session.accessToken(context.Background())
			refreshed <- err
		}()
		<-entered

		// The refresh stays blocked until this caller has returned. Therefore
		// the caller cannot depend on the end of the refresh.
		ctx, cancel := context.WithCancel(context.Background())
		waiting := make(chan error, 1)
		go func() {
			_, err := session.accessToken(ctx)
			waiting <- err
		}()
		cancel()
		assertError(t, <-waiting, cancelled)

		close(release)
		if err := <-refreshed; err != nil {
			t.Errorf("refresh: %v", err)
		}
	})
}

func TestSessionClose(t *testing.T) {
	t.Run("releases the lock", func(t *testing.T) {
		session := newTestSession(t, newFakeOAuthProvider(), testCredentials())
		lock, err := lockCredentials(session.path)
		if err != nil {
			t.Fatalf("lock: %v", err)
		}
		session.lock = lock

		if err := session.close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}
		if _, err := os.Stat(session.path + ".lock"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("lock file: %v, want removed", err)
		}
	})

	t.Run("rejects later requests", func(t *testing.T) {
		session := newTestSession(t, newFakeOAuthProvider(), testCredentials())
		lock, err := lockCredentials(session.path)
		if err != nil {
			t.Fatalf("lock: %v", err)
		}
		session.lock = lock
		if err := session.close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}

		_, err = session.accessToken(context.Background())
		assertError(t, err, "the ChatGPT credential session is shutting down")
	})
}

func TestSessionCloseRetainsLockDuringRefresh(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	provider := newFakeOAuthProvider()
	provider.exchange = func(url.Values) (int, string) {
		close(entered)
		<-release
		return http.StatusOK, tokenReply("", nil)
	}
	session := newTestSession(t, provider, expiredCredentials())
	lock, err := lockCredentials(session.path)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	session.lock = lock

	refreshed := make(chan error, 1)
	go func() {
		_, err := session.accessToken(context.Background())
		refreshed <- err
	}()
	<-entered

	// A cleanup deadline must not release the lock while a writer is active.
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	err = session.close(expired)
	assertError(t, err, "credential renewal is still active; the lock was retained. "+
		"Verify this process has stopped before removing the .lock file")
	if _, err := os.Stat(session.path + ".lock"); err != nil {
		t.Errorf("lock file: %v, want retained", err)
	}

	close(release)
	if err := <-refreshed; err != nil {
		t.Errorf("refresh: %v", err)
	}
	if err := session.close(context.Background()); err != nil {
		t.Errorf("close after refresh: %v", err)
	}
}

func TestOpenOAuthSession(t *testing.T) {
	cases := []struct {
		name   string
		change func(*oauthCredentials)
		want   string
	}{
		{name: "valid account", change: func(*oauthCredentials) {}},
		{
			name:   "signed out",
			change: func(c *oauthCredentials) { *c = c.withoutTokens() },
			want:   "no tokens for the selected account; run ant2oai login",
		},
		{
			name:   "no plan permission",
			change: func(c *oauthCredentials) { c.Scopes = []string{"openid"} },
			want: "ChatGPT plan usage is disabled; run ant2oai login -reconsent " +
				"or explicitly select api-key mode",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			credentials := testCredentials()
			testCase.change(&credentials)
			cfg := testConfig()
			cfg.authFile = filepath.Join(credentialDirectory(t), "chatgpt.json")
			if err := writeOAuthStore(cfg.authFile, testStore(credentials)); err != nil {
				t.Fatalf("write store: %v", err)
			}

			session, err := openOAuthSession(cfg, &http.Client{})
			if testCase.want != "" {
				assertError(t, err, testCase.want)
				if _, err := os.Stat(cfg.authFile + ".lock"); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("lock file: %v, want removed after a failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("openOAuthSession: %v", err)
			}

			// The session holds the lock for the life of the proxy.
			if _, err := os.Stat(cfg.authFile + ".lock"); err != nil {
				t.Errorf("lock file: %v", err)
			}
			if err := session.close(context.Background()); err != nil {
				t.Errorf("close: %v", err)
			}
		})
	}
}

func TestOpenOAuthSessionWithoutStore(t *testing.T) {
	cfg := testConfig()
	cfg.authFile = filepath.Join(credentialDirectory(t), "chatgpt.json")

	_, err := openOAuthSession(cfg, &http.Client{})
	assertError(t, err, "no ChatGPT credentials found; run ant2oai login "+
		"with the same config, account, and auth-file settings")
}
