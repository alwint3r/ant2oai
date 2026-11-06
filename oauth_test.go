package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// loginSafetyTimeout only stops a hung test. No test waits for it to expire.
	loginSafetyTimeout = 30 * time.Second
	newClientID        = "app_new"
	loginCode          = "authorization-code"
)

// loginOutput gives the printed sign-in link to the test, so the test can act on
// it without polling.
type loginOutput struct {
	links chan string

	mutex sync.Mutex
	text  strings.Builder
}

func (output *loginOutput) Write(data []byte) (int, error) {
	output.mutex.Lock()
	defer output.mutex.Unlock()

	output.text.Write(data)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "https://") {
			output.links <- line
		}
	}
	return len(data), nil
}

func (output *loginOutput) String() string {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	return output.text.String()
}

// loginRun is one sign-in that is waiting for its browser callback.
type loginRun struct {
	parameters url.Values
	done       chan error
	output     *loginOutput
	path       string
	cancel     context.CancelFunc
}

// startLogin begins a sign-in and returns when the link has been printed. The
// callback listener is a real loopback socket, because login owns that listener.
func startLogin(
	t *testing.T, provider *fakeOAuthProvider, cfg config, store oauthStore,
) *loginRun {
	t.Helper()

	path := filepath.Join(credentialDirectory(t), "chatgpt.json")
	if err := writeOAuthStore(path, store); err != nil {
		t.Fatalf("write store: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), loginSafetyTimeout)
	t.Cleanup(cancel)

	run := &loginRun{
		done: make(chan error, 1), path: path, cancel: cancel,
		output: &loginOutput{links: make(chan string, 1)},
	}
	go func() { run.done <- provider.client().login(ctx, cfg, path, store, run.output) }()

	select {
	case link := <-run.output.links:
		parsed, err := url.Parse(link)
		if err != nil {
			t.Fatalf("parse sign-in link: %v", err)
		}
		run.parameters = parsed.Query()
	case err := <-run.done:
		t.Fatalf("login ended before printing a link: %v", err)
	}
	return run
}

func (run *loginRun) validQuery() string {
	values := url.Values{
		"state": {run.parameters.Get("state")}, "code": {loginCode}, "client_id": {newClientID},
	}
	return values.Encode()
}

// visitCallback acts as the browser. It reports a failure without stopping the
// test, because the caller may need to release a gated token exchange.
func (run *loginRun) visitCallback(t *testing.T, method, query string) (int, string) {
	t.Helper()

	return run.send(t, method, run.parameters.Get("redirect_uri")+"?"+query, "")
}

func (run *loginRun) send(t *testing.T, method, target, host string) (int, string) {
	t.Helper()

	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Errorf("create callback request: %v", err)
		return 0, ""
	}
	if host != "" {
		request.Host = host
	}

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("visit callback: %v", err)
		return 0, ""
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Errorf("read callback reply: %v", err)
	}
	return response.StatusCode, string(body)
}

// newLoginToken returns an ID token for a sign-in with the given nonce.
func newLoginToken(nonce, audience string, overrides map[string]any) string {
	claims := withClaims(map[string]any{"aud": audience, "nonce": nonce})
	for name, value := range overrides {
		claims[name] = value
	}
	return signToken(validHeader(), claims)
}

func emptyStore() oauthStore {
	return oauthStore{HostID: testHostID, Accounts: map[string]oauthCredentials{}}
}

func readSavedAccount(t *testing.T, path string) (oauthCredentials, bool) {
	t.Helper()

	store, err := readOAuthStore(path)
	if err != nil {
		t.Fatalf("read saved store: %v", err)
	}
	credentials, exists := store.Accounts["default"]
	return credentials, exists
}

func TestLoginNewAccount(t *testing.T) {
	// The exchange waits for the browser reply. Therefore this test also proves
	// that the callback server stays available during the token exchange.
	browserAnswered := make(chan struct{})
	var nonce atomic.Value
	provider := newFakeOAuthProvider()
	provider.exchange = func(url.Values) (int, string) {
		<-browserAnswered
		return http.StatusOK, tokenReply(newLoginToken(nonce.Load().(string), newClientID, nil), nil)
	}

	run := startLogin(t, provider, testConfig(), emptyStore())
	nonce.Store(run.parameters.Get("nonce"))

	status, body := run.visitCallback(t, http.MethodGet, run.validQuery())
	close(browserAnswered)
	if err := <-run.done; err != nil {
		t.Fatalf("login: %v", err)
	}

	if status != http.StatusOK ||
		body != "Sign-in callback received. Return to the ant2oai terminal.\n" {
		t.Errorf("browser reply = %d %q", status, body)
	}

	// The link identifies this client and asks for the plan permission.
	wantParameters := map[string]string{
		"response_type": "code", "client_id": "dynamic_agent_client",
		"agent_name_hint": "ant2oai", "resource": chatGPTResource,
		"ext_agent_host_id": testHostID, "code_challenge_method": "S256",
		"scope": "openid profile email offline_access resource.invoke " + chatGPTScope,
	}
	for name, want := range wantParameters {
		if got := run.parameters.Get(name); got != want {
			t.Errorf("parameter %s = %q, want %q", name, got, want)
		}
	}
	if run.parameters.Has("prompt") {
		t.Error("a normal sign-in must not ask for consent again")
	}

	// PKCE: the verifier in the exchange must match the challenge in the link.
	form := provider.tokenForms[0]
	digest := sha256.Sum256([]byte(form.Get("code_verifier")))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if challenge != run.parameters.Get("code_challenge") {
		t.Error("the code verifier does not match the code challenge")
	}
	if form.Get("code") != loginCode || form.Get("client_id") != newClientID ||
		form.Get("grant_type") != "authorization_code" ||
		form.Get("redirect_uri") != run.parameters.Get("redirect_uri") ||
		form.Get("resource") != chatGPTResource {
		t.Errorf("token form = %v", form)
	}

	saved, exists := readSavedAccount(t, run.path)
	if !exists {
		t.Fatal("the account was not saved")
	}
	if saved.ClientID != newClientID || saved.Subject != "subject-1" ||
		saved.Email != "person@example.com" || saved.AccessToken != "access-2" ||
		saved.RefreshToken != "refresh-2" || saved.Issuer != chatGPTIssuer {
		t.Errorf("saved account = %+v", saved)
	}

	output := run.output.String()
	for _, want := range []string{
		`Signed in account "default" ("person@example.com").`,
		"ChatGPT plan usage is enabled. Use the models command to list available models.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
}

func TestLoginSecretsAreUnique(t *testing.T) {
	provider := newFakeOAuthProvider()
	run := startLogin(t, provider, testConfig(), emptyStore())
	defer func() {
		run.cancel()
		<-run.done
	}()

	state, nonce := run.parameters.Get("state"), run.parameters.Get("nonce")
	if len(state) < 43 || len(nonce) < 43 || state == nonce {
		t.Errorf("state = %q, nonce = %q", state, nonce)
	}
}

func TestLoginOptions(t *testing.T) {
	t.Run("reconsent asks again", func(t *testing.T) {
		cfg := testConfig()
		cfg.requestConsent = true
		run := startLogin(t, newFakeOAuthProvider(), cfg, emptyStore())
		defer func() {
			run.cancel()
			<-run.done
		}()

		if prompt := run.parameters.Get("prompt"); prompt != "consent" {
			t.Errorf("prompt = %q, want consent", prompt)
		}
	})

	t.Run("returning account keeps its client", func(t *testing.T) {
		store := testStore(testCredentials())
		run := startLogin(t, newFakeOAuthProvider(), testConfig(), store)
		defer func() {
			run.cancel()
			<-run.done
		}()

		if clientID := run.parameters.Get("client_id"); clientID != "app_client" {
			t.Errorf("client_id = %q", clientID)
		}
		if run.parameters.Has("agent_name_hint") {
			t.Error("a returning account must not register again")
		}
	})
}

func TestLoginGrantedPermissions(t *testing.T) {
	cases := []struct {
		name       string
		overrides  map[string]any
		wantOutput string
		wantScopes []string
	}{
		{
			name:       "plan permission was declined",
			overrides:  map[string]any{"scope": "openid"},
			wantOutput: "ChatGPT plan usage is disabled by the granted permissions.",
			wantScopes: []string{"openid"},
		},
		{
			// OAuth permits a server to omit the scope when it grants the request.
			name:       "omitted scope means the requested scopes",
			overrides:  map[string]any{"scope": nil},
			wantOutput: "ChatGPT plan usage is enabled.",
			wantScopes: []string{
				"openid", "profile", "email", "offline_access", "resource.invoke", chatGPTScope,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var nonce atomic.Value
			provider := newFakeOAuthProvider()
			provider.exchange = func(url.Values) (int, string) {
				token := newLoginToken(nonce.Load().(string), newClientID, nil)
				return http.StatusOK, tokenReply(token, testCase.overrides)
			}

			run := startLogin(t, provider, testConfig(), emptyStore())
			nonce.Store(run.parameters.Get("nonce"))
			run.visitCallback(t, http.MethodGet, run.validQuery())
			if err := <-run.done; err != nil {
				t.Fatalf("login: %v", err)
			}

			if !strings.Contains(run.output.String(), testCase.wantOutput) {
				t.Errorf("output lacks %q:\n%s", testCase.wantOutput, run.output.String())
			}
			saved, _ := readSavedAccount(t, run.path)
			if strings.Join(saved.Scopes, " ") != strings.Join(testCase.wantScopes, " ") {
				t.Errorf("scopes = %v, want %v", saved.Scopes, testCase.wantScopes)
			}
		})
	}
}

func TestLoginReturningAccount(t *testing.T) {
	var nonce atomic.Value
	provider := newFakeOAuthProvider()
	provider.exchange = func(url.Values) (int, string) {
		token := newLoginToken(nonce.Load().(string), "app_client", nil)
		return http.StatusOK, tokenReply(token, nil)
	}

	run := startLogin(t, provider, testConfig(), testStore(testCredentials().withoutTokens()))
	nonce.Store(run.parameters.Get("nonce"))

	query := url.Values{"state": {run.parameters.Get("state")}, "code": {loginCode}}
	run.visitCallback(t, http.MethodGet, query.Encode())
	if err := <-run.done; err != nil {
		t.Fatalf("login: %v", err)
	}

	saved, _ := readSavedAccount(t, run.path)
	if saved.ClientID != "app_client" || saved.AccessToken != "access-2" {
		t.Errorf("saved account = %+v", saved)
	}
}

func TestLoginRejectsInvalidCallbackResult(t *testing.T) {
	const incomplete = "ChatGPT sign-in was declined or returned an incomplete callback"
	const noClientID = "ChatGPT registration did not return an issued client ID"

	cases := []struct {
		name  string
		extra url.Values
		want  string
	}{
		{"declined", url.Values{"code": {"x"}, "error": {"access_denied"}}, incomplete},
		{"no code", url.Values{}, incomplete},
		{"two codes", url.Values{"code": {"a", "b"}}, incomplete},
		{"empty code", url.Values{"code": {""}}, incomplete},
		{"code over the limit", url.Values{"code": {strings.Repeat("c", 8193)}}, incomplete},
		{"two client IDs", url.Values{"code": {"a"}, "client_id": {"a", "b"}}, incomplete},
		{"no client ID", url.Values{"code": {"a"}}, noClientID},
		{
			"unissued client ID",
			url.Values{"code": {"a"}, "client_id": {"dynamic_agent_client"}}, noClientID,
		},
		{"client ID with a space", url.Values{"code": {"a"}, "client_id": {"a b"}}, noClientID},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newFakeOAuthProvider()
			run := startLogin(t, provider, testConfig(), emptyStore())

			query := url.Values{"state": {run.parameters.Get("state")}}
			for name, values := range testCase.extra {
				query[name] = values
			}

			// Login can close the server before the browser reads the reply, so
			// this test checks only the error that login reports.
			request, err := http.NewRequest(http.MethodGet,
				run.parameters.Get("redirect_uri")+"?"+query.Encode(), nil)
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			if response, err := client.Do(request); err == nil {
				response.Body.Close()
			}

			assertError(t, <-run.done, testCase.want)
			if provider.tokenCallCount() != 0 {
				t.Error("an invalid callback reached the token endpoint")
			}
		})
	}
}

func TestLoginRejectsOtherClientForReturningAccount(t *testing.T) {
	provider := newFakeOAuthProvider()
	run := startLogin(t, provider, testConfig(), testStore(testCredentials()))

	request, err := http.NewRequest(http.MethodGet,
		run.parameters.Get("redirect_uri")+"?"+run.validQuery(), nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	if response, err := client.Do(request); err == nil {
		response.Body.Close()
	}

	assertError(t, <-run.done, "ChatGPT returned a different client ID for the selected account")
}

func TestLoginCallbackValidation(t *testing.T) {
	const invalidCallback = "Invalid callback.\n"
	const invalidState = "Invalid OAuth state. Return to the original sign-in link.\n"
	const badRequest = http.StatusBadRequest

	var nonce atomic.Value
	provider := newFakeOAuthProvider()
	provider.exchange = func(url.Values) (int, string) {
		token := newLoginToken(nonce.Load().(string), newClientID, nil)
		return http.StatusOK, tokenReply(token, nil)
	}
	run := startLogin(t, provider, testConfig(), emptyStore())
	nonce.Store(run.parameters.Get("nonce"))

	callbackURI := run.parameters.Get("redirect_uri")
	parsedCallback, err := url.Parse(callbackURI)
	if err != nil {
		t.Fatalf("parse callback URI: %v", err)
	}
	state := run.parameters.Get("state")
	otherPath := "http://" + parsedCallback.Host + "/other?" + run.validQuery()

	// Invalid visits must leave the transaction open for the real callback.
	cases := []struct {
		name       string
		method     string
		target     string
		host       string
		wantStatus int
		wantBody   string
	}{
		{
			"wrong method", http.MethodPost, callbackURI + "?" + run.validQuery(), "",
			badRequest, invalidCallback,
		},
		{"wrong path", http.MethodGet, otherPath, "", badRequest, invalidCallback},
		{
			"wrong host header", http.MethodGet, callbackURI + "?" + run.validQuery(),
			"other.test", badRequest, invalidCallback,
		},
		{"no state", http.MethodGet, callbackURI + "?code=a", "", badRequest, invalidState},
		{
			"wrong state", http.MethodGet, callbackURI + "?state=wrong&code=a", "",
			badRequest, invalidState,
		},
		{
			"two states", http.MethodGet,
			callbackURI + "?state=" + state + "&state=" + state + "&code=a", "",
			badRequest, invalidState,
		},
		{"malformed query", http.MethodGet, callbackURI + "?%zz", "", badRequest, invalidState},
		{
			// The HTTP server rejects this request before the handler runs,
			// because the query exceeds the header size limit.
			"query over the limit", http.MethodGet,
			callbackURI + "?a=" + strings.Repeat("b", 16<<10), "",
			http.StatusRequestHeaderFieldsTooLarge, "",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := run.send(t, testCase.method, testCase.target, testCase.host)

			if status != testCase.wantStatus {
				t.Errorf("status = %d, want %d", status, testCase.wantStatus)
			}
			if testCase.wantBody != "" && body != testCase.wantBody {
				t.Errorf("body = %q, want %q", body, testCase.wantBody)
			}
		})
	}

	run.visitCallback(t, http.MethodGet, run.validQuery())
	if err := <-run.done; err != nil {
		t.Fatalf("login after invalid visits: %v", err)
	}
}

func TestLoginCallbackWorksOnlyOnce(t *testing.T) {
	release := make(chan struct{})
	var nonce atomic.Value
	provider := newFakeOAuthProvider()
	provider.exchange = func(url.Values) (int, string) {
		<-release
		token := newLoginToken(nonce.Load().(string), newClientID, nil)
		return http.StatusOK, tokenReply(token, nil)
	}
	run := startLogin(t, provider, testConfig(), emptyStore())
	nonce.Store(run.parameters.Get("nonce"))

	firstStatus, _ := run.visitCallback(t, http.MethodGet, run.validQuery())
	secondStatus, secondBody := run.visitCallback(t, http.MethodGet, run.validQuery())
	close(release)
	if err := <-run.done; err != nil {
		t.Fatalf("login: %v", err)
	}

	if firstStatus != http.StatusOK {
		t.Errorf("first status = %d", firstStatus)
	}
	if secondStatus != http.StatusConflict || secondBody != "This callback was already used.\n" {
		t.Errorf("second reply = %d %q", secondStatus, secondBody)
	}
	if provider.tokenCallCount() != 1 {
		t.Errorf("token calls = %d, want 1", provider.tokenCallCount())
	}
}

func TestLoginCallbackSecurityHeaders(t *testing.T) {
	provider := newFakeOAuthProvider()
	run := startLogin(t, provider, testConfig(), emptyStore())
	defer func() {
		run.cancel()
		<-run.done
	}()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Get(run.parameters.Get("redirect_uri") + "?state=wrong")
	if err != nil {
		t.Fatalf("visit callback: %v", err)
	}
	defer response.Body.Close()

	assertHeader(t, response.Header, "Cache-Control", "no-store")
	assertHeader(t, response.Header, "Referrer-Policy", "no-referrer")
	assertHeader(t, response.Header, "Content-Security-Policy", "default-src 'none'")
	assertHeader(t, response.Header, "X-Content-Type-Options", "nosniff")
}

func TestLoginExchangeFailures(t *testing.T) {
	const expired = "ChatGPT credentials expired or were revoked; " +
		"run ant2oai login for the selected account"

	cases := []struct {
		name  string
		reply func(nonce string) (int, string)
		want  string
	}{
		{
			"rejected code",
			func(string) (int, string) { return 400, `{"error":"invalid_grant"}` },
			expired,
		},
		{
			"no ID token",
			func(string) (int, string) { return 200, tokenReply("", nil) },
			"ChatGPT sign-in returned no ID token",
		},
		{
			"invalid ID token",
			func(string) (int, string) { return 200, tokenReply("not.a.jwt", nil) },
			"OpenAI ID token uses an unsupported JWT header or claims encoding",
		},
		{
			"replayed ID token",
			func(string) (int, string) {
				return 200, tokenReply(newLoginToken("old-nonce", newClientID, nil), nil)
			},
			"OpenAI ID token nonce does not match the sign-in attempt",
		},
		{
			"scope over the limit",
			func(nonce string) (int, string) {
				return 200, tokenReply(newLoginToken(nonce, newClientID, nil),
					map[string]any{"scope": strings.Repeat("a", 8193)})
			},
			"OpenAI token response contains an oversized scope set",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var nonce atomic.Value
			provider := newFakeOAuthProvider()
			provider.exchange = func(url.Values) (int, string) {
				return testCase.reply(nonce.Load().(string))
			}
			run := startLogin(t, provider, testConfig(), emptyStore())
			nonce.Store(run.parameters.Get("nonce"))

			run.visitCallback(t, http.MethodGet, run.validQuery())
			assertError(t, <-run.done, testCase.want)

			// The registration survives a failed exchange, and it has no tokens.
			saved, exists := readSavedAccount(t, run.path)
			if !exists || saved.ClientID != newClientID {
				t.Fatalf("the registration was not retained: %+v", saved)
			}
			if saved.AccessToken != "" || saved.RefreshToken != "" {
				t.Errorf("unverified tokens were saved: %+v", saved)
			}
		})
	}
}

func TestLoginRejectsChangedIdentity(t *testing.T) {
	var nonce atomic.Value
	provider := newFakeOAuthProvider()
	provider.exchange = func(url.Values) (int, string) {
		token := newLoginToken(nonce.Load().(string), "app_client",
			map[string]any{"sub": "someone-else"})
		return http.StatusOK, tokenReply(token, nil)
	}
	run := startLogin(t, provider, testConfig(), testStore(testCredentials()))
	nonce.Store(run.parameters.Get("nonce"))

	query := url.Values{"state": {run.parameters.Get("state")}, "code": {loginCode}}
	run.visitCallback(t, http.MethodGet, query.Encode())
	assertError(t, <-run.done, "ChatGPT identity changed; "+
		"sign in with the original account or use a new -account label")

	saved, _ := readSavedAccount(t, run.path)
	if saved.AccessToken != "access-1" {
		t.Errorf("the stored account changed: %+v", saved)
	}
}

func TestLoginCancellation(t *testing.T) {
	run := startLogin(t, newFakeOAuthProvider(), testConfig(), emptyStore())

	run.cancel()
	assertError(t, <-run.done, "ChatGPT sign-in was cancelled or exceeded five minutes")
}

func TestLoginBeforeTheCallback(t *testing.T) {
	t.Run("registration limit", func(t *testing.T) {
		store := emptyStore()
		for index := 0; index < 32; index++ {
			store.Accounts[fmt.Sprintf("account-%d", index)] = oauthCredentials{}
		}

		err := newFakeOAuthProvider().client().login(context.Background(), testConfig(),
			"/nonexistent/chatgpt.json", store, io.Discard)
		assertError(t, err, "the credential file already contains the maximum of 32 registrations")
	})

	t.Run("existing account at the registration limit", func(t *testing.T) {
		store := testStore(testCredentials())
		for index := 0; index < 31; index++ {
			store.Accounts[fmt.Sprintf("account-%d", index)] = oauthCredentials{}
		}
		run := startLogin(t, newFakeOAuthProvider(), testConfig(), store)

		run.cancel()
		<-run.done
	})

	t.Run("discovery failure", func(t *testing.T) {
		provider := newFakeOAuthProvider()
		provider.discoveryStatus = http.StatusInternalServerError

		err := provider.client().login(context.Background(), testConfig(),
			"/nonexistent/chatgpt.json", emptyStore(), io.Discard)
		assertError(t, err, "OpenAI OAuth discovery failed; check service availability")
	})

	t.Run("invalid authorization endpoint", func(t *testing.T) {
		provider := newFakeOAuthProvider()
		provider.discovery = strings.Replace(trustedDiscovery, "oauth/authorize", "oauth/%zz", 1)

		err := provider.client().login(context.Background(), testConfig(),
			"/nonexistent/chatgpt.json", emptyStore(), io.Discard)
		if err == nil {
			t.Fatal("error = nil")
		}
	})
}

func TestRevoke(t *testing.T) {
	const notConfirmed = "OpenAI did not confirm ChatGPT session revocation"
	const noConfirmation = "could not confirm ChatGPT session revocation after two attempts"

	cases := []struct {
		name         string
		statuses     []int
		wantAttempts int
		want         string
	}{
		{name: "success", statuses: []int{200}, wantAttempts: 1},
		{name: "rejected", statuses: []int{400}, wantAttempts: 1, want: notConfirmed},
		{name: "retry succeeds", statuses: []int{500, 200}, wantAttempts: 2},
		{name: "retry is rejected", statuses: []int{503, 400}, wantAttempts: 2, want: notConfirmed},
		{name: "retry fails", statuses: []int{500, 500}, wantAttempts: 2, want: noConfirmation},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// The retry backoff is a real timer, so these cases run together.
			t.Parallel()
			provider := newFakeOAuthProvider()
			provider.revoke = func(attempt int) int { return testCase.statuses[attempt] }

			err := provider.client().revoke(context.Background(), testCredentials())
			if testCase.want != "" {
				assertError(t, err, testCase.want)
			} else if err != nil {
				t.Errorf("revoke: %v", err)
			}
			if len(provider.revokeCalls) != testCase.wantAttempts {
				t.Errorf("attempts = %d, want %d", len(provider.revokeCalls), testCase.wantAttempts)
			}
		})
	}
}

func TestRevokeSelectsTheToken(t *testing.T) {
	accessOnly := testCredentials()
	accessOnly.RefreshToken = ""

	cases := []struct {
		name        string
		credentials oauthCredentials
		want        url.Values
	}{
		{
			"refresh token is preferred", testCredentials(),
			url.Values{
				"token": {"refresh-1"}, "token_type_hint": {"refresh_token"},
				"client_id": {"app_client"},
			},
		},
		{
			"access token is the fallback", accessOnly,
			url.Values{
				"token": {"access-1"}, "token_type_hint": {"access_token"},
				"client_id": {"app_client"},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newFakeOAuthProvider()

			if err := provider.client().revoke(context.Background(), testCase.credentials); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			assertJSONEqual(t, marshal(t, provider.revokeCalls[0]), marshal(t, testCase.want))
		})
	}
}

func TestRevokeWithoutTokensNeedsNoNetwork(t *testing.T) {
	provider := newFakeOAuthProvider()
	provider.failedPaths[discoveryPath] = true

	err := provider.client().revoke(context.Background(), testCredentials().withoutTokens())
	if err != nil {
		t.Errorf("revoke: %v", err)
	}
}

func TestRevokeStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := newFakeOAuthProvider()
	provider.revoke = func(int) int {
		cancel()
		return http.StatusInternalServerError
	}

	err := provider.client().revoke(ctx, testCredentials())
	assertError(t, err, "ChatGPT session revocation was cancelled or timed out")
}

func TestListModels(t *testing.T) {
	provider := newFakeOAuthProvider()
	provider.models = func() (int, string) {
		return http.StatusOK, `{"models":[
			{"slug":"gpt-a","visibility":"list","display_name":"GPT A"},
			{"slug":"gpt-hidden","visibility":"hide","display_name":"Hidden"},
			{"slug":"","visibility":"list","display_name":"No slug"},
			{"slug":"gpt-b","visibility":"list"}]}`
	}
	var output bytes.Buffer

	if err := provider.client().listModels(context.Background(), "token", &output); err != nil {
		t.Fatalf("listModels: %v", err)
	}

	want := "gpt-a\t\"GPT A\"\ngpt-b\t\"\"\n"
	if output.String() != want {
		t.Errorf("output = %q, want %q", output.String(), want)
	}
	if provider.modelTokens[0] != "Bearer token" {
		t.Errorf("authorization = %q", provider.modelTokens[0])
	}
}

func TestListModelsFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			"HTTP error", 403, "",
			"ChatGPT model discovery returned HTTP 403; check account and plan permissions",
		},
		{"malformed", 200, "{", "ChatGPT model discovery returned invalid JSON"},
		{
			"field types", 200, `{"models":[{"slug":1}]}`,
			"ChatGPT model catalog contains unexpected field types",
		},
		{"no models", 200, `{}`, "OpenAI returned an unexpected ChatGPT model catalog shape"},
		{
			"deep", 200, strings.Repeat("[", 200),
			"decode ChatGPT model catalog: JSON nesting exceeds 128 levels",
		},
		{
			"over the limit", 200, strings.Repeat(" ", maxModelCatalogBytes+1),
			"ChatGPT model catalog exceeds the 8 MiB response limit",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newFakeOAuthProvider()
			provider.models = func() (int, string) { return testCase.status, testCase.body }

			err := provider.client().listModels(context.Background(), "token", io.Discard)
			assertError(t, err, testCase.want)
		})
	}
}

func TestListModelsNetworkFailures(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		provider := newFakeOAuthProvider()
		provider.failedPaths["/v1/models"] = true

		err := provider.client().listModels(context.Background(), "token", io.Discard)
		assertError(t, err, "could not retrieve ChatGPT models; check network and TLS settings")
	})

	t.Run("response body", func(t *testing.T) {
		upstream := &fakeUpstream{respond: func(*http.Request) (*http.Response, error) {
			response := upstreamReply(http.StatusOK, nil, "")
			response.Body = io.NopCloser(failingReader{})
			return response, nil
		}}
		client := oauthClient{client: &http.Client{Transport: roundTripWithoutBody{upstream}}}

		err := client.listModels(context.Background(), "token", io.Discard)
		assertError(t, err, "could not read the ChatGPT model catalog; "+
			"check the network and request timeout")
	})
}

// roundTripWithoutBody adapts fakeUpstream to requests that have no body.
type roundTripWithoutBody struct {
	upstream *fakeUpstream
}

func (transport roundTripWithoutBody) RoundTrip(request *http.Request) (*http.Response, error) {
	request.Body = http.NoBody
	return transport.upstream.RoundTrip(request)
}

func TestExchangeErrorHidesProviderDetails(t *testing.T) {
	provider := newFakeOAuthProvider()
	provider.exchange = func(url.Values) (int, string) {
		return http.StatusBadRequest, `{"error":"other","error_description":"secret-text"}`
	}

	_, err := provider.client().exchange(context.Background(),
		"https://auth.openai.com/oauth/token", url.Values{})

	var failure *oauthExchangeError
	if !errors.As(err, &failure) {
		t.Fatalf("error type = %T", err)
	}
	if failure.unusable() {
		t.Error("an unknown error code must not clear credentials")
	}
	if strings.Contains(err.Error(), "secret-text") {
		t.Errorf("error exposes provider text: %v", err)
	}
}

func authConfig(t *testing.T) config {
	t.Helper()

	cfg := testConfig()
	cfg.authFile = filepath.Join(credentialDirectory(t), "chatgpt.json")
	return cfg
}

func TestLoginStatus(t *testing.T) {
	t.Run("no store", func(t *testing.T) {
		err := runAuthCommand("login-status", authConfig(t), io.Discard)
		assertError(t, err, "no saved ChatGPT credentials; run ant2oai login")
	})

	t.Run("lists every account", func(t *testing.T) {
		cfg := authConfig(t)
		store := testStore(testCredentials())
		limited := testCredentials()
		limited.Scopes = []string{"openid"}
		store.Accounts["limited"] = limited
		store.Accounts["signed-out"] = testCredentials().withoutTokens()
		if err := writeOAuthStore(cfg.authFile, store); err != nil {
			t.Fatalf("write store: %v", err)
		}
		var output bytes.Buffer

		if err := runAuthCommand("login-status", cfg, &output); err != nil {
			t.Fatalf("login-status: %v", err)
		}

		want := `Selected account: "default"
Credential file: ` + cfg.authFile + `
"default": ChatGPT plan usage granted; email="person@example.com"; access expiry=2100-01-01T00:00:00Z
"limited": signed in; ChatGPT plan usage disabled; email="person@example.com"; access expiry=2100-01-01T00:00:00Z
"signed-out": signed out; email="person@example.com"; access expiry=0001-01-01T00:00:00Z
`
		if output.String() != want {
			t.Errorf("output:\n%s\nwant:\n%s", output.String(), want)
		}
	})

	t.Run("needs no lock", func(t *testing.T) {
		cfg := authConfig(t)
		if err := writeOAuthStore(cfg.authFile, testStore(testCredentials())); err != nil {
			t.Fatalf("write store: %v", err)
		}
		lock, err := lockCredentials(cfg.authFile)
		if err != nil {
			t.Fatalf("lock: %v", err)
		}
		defer lock.close()

		if err := runAuthCommand("login-status", cfg, io.Discard); err != nil {
			t.Errorf("login-status: %v", err)
		}
	})
}

func TestAuthCommandFailures(t *testing.T) {
	t.Run("logout without a store", func(t *testing.T) {
		err := runAuthCommand("logout", authConfig(t), io.Discard)
		if err == nil || !strings.HasPrefix(err.Error(), "load ChatGPT credentials:") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("logout of an unknown account", func(t *testing.T) {
		cfg := authConfig(t)
		cfg.account = "other"
		if err := writeOAuthStore(cfg.authFile, testStore(testCredentials())); err != nil {
			t.Fatalf("write store: %v", err)
		}

		err := runAuthCommand("logout", cfg, io.Discard)
		assertError(t, err, "the selected ChatGPT account has no registration")
	})

	t.Run("locked credentials", func(t *testing.T) {
		cfg := authConfig(t)
		lock, err := lockCredentials(cfg.authFile)
		if err != nil {
			t.Fatalf("lock: %v", err)
		}
		defer lock.close()

		err = runAuthCommand("logout", cfg, io.Discard)
		if err == nil || !strings.HasPrefix(err.Error(), "credential file is locked") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("unknown command releases the lock", func(t *testing.T) {
		cfg := authConfig(t)
		if err := writeOAuthStore(cfg.authFile, testStore(testCredentials())); err != nil {
			t.Fatalf("write store: %v", err)
		}

		err := runAuthCommand("unknown", cfg, io.Discard)
		assertError(t, err, "unknown authentication command")
		if _, err := os.Stat(cfg.authFile + ".lock"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("lock file: %v, want removed", err)
		}
	})
}

func TestLogoutOfSignedOutAccount(t *testing.T) {
	cfg := authConfig(t)
	store := testStore(testCredentials().withoutTokens())
	if err := writeOAuthStore(cfg.authFile, store); err != nil {
		t.Fatalf("write store: %v", err)
	}
	var output bytes.Buffer

	if err := runAuthCommand("logout", cfg, &output); err != nil {
		t.Fatalf("logout: %v", err)
	}

	want := "Signed out account \"default\". Registration and host identity were retained.\n"
	if output.String() != want {
		t.Errorf("output = %q, want %q", output.String(), want)
	}
	saved, err := readOAuthStore(cfg.authFile)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if saved.HostID != testHostID || saved.Accounts["default"].ClientID != "app_client" {
		t.Errorf("registration was lost: %+v", saved)
	}
}
