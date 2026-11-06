package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	chatGPTIssuer             = "https://auth.openai.com"
	chatGPTResource           = "https://api.openai.com/v1"
	chatGPTScope              = "chatgpt.tokens.use.direct"
	maxModelCatalogBytes      = 8 << 20
	maxCallbackQueryBytes     = 16 << 10
	maxAuthorizationCodeBytes = 8192
	authCommandTimeout        = 5 * time.Minute
	revocationRetryDelay      = 250 * time.Millisecond
)

func runAuthCommand(command string, cfg config, output io.Writer) error {
	path, err := credentialPath(cfg)
	if err != nil {
		return err
	}
	if command == "login-status" {
		return printLoginStatus(path, cfg.account, output)
	}

	lock, err := lockCredentials(path)
	if err != nil {
		return err
	}
	defer func() {
		if err := lock.close(); err != nil {
			slog.Warn("could not remove credential lock", "reason", err.Error())
		}
	}()

	store, err := readOAuthStore(path)
	if errors.Is(err, os.ErrNotExist) && command == "login" {
		store, err = newOAuthStore()
		if err == nil {
			// The host identity must survive an interrupted first login.
			err = writeOAuthStore(path, store)
		}
	}
	if err != nil {
		return fmt.Errorf("load ChatGPT credentials: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, authCommandTimeout)
	defer cancel()
	client := newUpstreamClient(cfg)
	defer client.CloseIdleConnections()
	oauth := oauthClient{client: client}

	switch command {
	case "login":
		return oauth.login(ctx, cfg, path, store, output)
	case "logout":
		return oauth.logout(ctx, cfg.account, path, store, output)
	case "models":
		session := &oauthSession{path: path, account: cfg.account, store: store, oauth: oauth}
		token, err := session.accessToken(ctx)
		if err != nil {
			return err
		}
		return oauth.listModels(ctx, token, output)
	default:
		return errors.New("unknown authentication command")
	}
}

func printLoginStatus(path, selectedAccount string, output io.Writer) error {
	store, err := readOAuthStore(path)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no saved ChatGPT credentials; run ant2oai login")
	}
	if err != nil {
		return err
	}

	labels := make([]string, 0, len(store.Accounts))
	for label := range store.Accounts {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	fmt.Fprintf(output, "Selected account: %q\nCredential file: %s\n", selectedAccount, path)
	for _, label := range labels {
		account := store.Accounts[label]
		state := "signed out"
		if account.AccessToken != "" {
			state = "signed in; ChatGPT plan usage disabled"
			if account.hasPlanUsage() {
				state = "ChatGPT plan usage granted"
			}
		}
		fmt.Fprintf(output, "%q: %s; email=%q; access expiry=%s\n",
			label, state, account.Email, account.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// logout removes local tokens even when OpenAI cannot confirm the revocation.
func (oauth oauthClient) logout(
	ctx context.Context, account, path string, store oauthStore, output io.Writer,
) error {
	credentials, exists := store.Accounts[account]
	if !exists {
		return errors.New("the selected ChatGPT account has no registration")
	}

	if credentials.RefreshToken != "" || credentials.AccessToken != "" {
		if err := oauth.revoke(ctx, credentials); err != nil {
			fmt.Fprintln(output, "Remote revocation was not confirmed. "+
				"Disconnect ant2oai in ChatGPT Settings to end remote access.")
		}
	}

	err := writeOAuthStore(path, store.withAccount(account, credentials.withoutTokens()))
	if err != nil {
		return fmt.Errorf("clear local ChatGPT tokens: %w", err)
	}
	fmt.Fprintf(output, "Signed out account %q. Registration and host identity were retained.\n",
		account)
	return nil
}

// oauthCallback is the result of the browser redirect at the end of a sign-in.
type oauthCallback struct {
	code     string
	clientID string
	failed   bool
}

func (oauth oauthClient) login(
	ctx context.Context, cfg config, path string, store oauthStore, output io.Writer,
) error {
	selected, exists := store.Accounts[cfg.account]
	if !exists && len(store.Accounts) >= maxOAuthAccounts {
		return errors.New("the credential file already contains the maximum of 32 registrations")
	}
	metadata, err := oauth.metadata(ctx)
	if err != nil {
		return err
	}

	// Bind first so another local process cannot take the callback port.
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return errors.New("could not start the loopback OAuth callback listener")
	}
	defer listener.Close()
	callbackHost := listener.Addr().String()
	callbackURI := "http://" + callbackHost + "/auth/callback"

	// Authorization link.
	state, nonce, verifier, err := newOAuthSecrets()
	if err != nil {
		return err
	}
	challenge := sha256.Sum256([]byte(verifier))
	authorization, err := url.Parse(metadata.AuthorizationEndpoint)
	if err != nil {
		return errors.New("could not parse the discovered authorization endpoint")
	}
	parameters := url.Values{
		"response_type": {"code"}, "redirect_uri": {callbackURI},
		"scope":    {"openid profile email offline_access resource.invoke " + chatGPTScope},
		"resource": {chatGPTResource}, "ext_agent_host_id": {store.HostID},
		"state": {state}, "nonce": {nonce}, "code_challenge_method": {"S256"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
	}

	if exists {
		parameters.Set("client_id", selected.ClientID)
	} else {
		parameters.Set("client_id", "dynamic_agent_client")
		parameters.Set("agent_name_hint", "ant2oai")
	}
	if cfg.requestConsent {
		parameters.Set("prompt", "consent")
	}
	// Leave ID-token hints out of the printed URL. Returning users choose their
	// account in the browser, and the new signed identity is checked below.
	authorization.RawQuery = parameters.Encode()

	// The server must outlive the token exchange below. It stops at the end of
	// this function, so that the browser still receives the callback reply.
	callbacks := make(chan oauthCallback, 1)
	server := &http.Server{
		Handler:           newCallbackHandler(callbackHost, state, callbacks),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second, WriteTimeout: 10 * time.Second,
		IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8 << 10,
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	defer server.Close()
	fmt.Fprintf(output, "Continue with ChatGPT\nOpen this URL in your browser:\n%s\n"+
		"Waiting up to five minutes for %s\n", authorization.String(), callbackURI)

	var callback oauthCallback
	select {
	case callback = <-callbacks:
	case <-ctx.Done():
		return errors.New("ChatGPT sign-in was cancelled or exceeded five minutes")
	case <-serverErrors:
		return errors.New("the OAuth callback listener stopped before sign-in completed")
	}
	if callback.failed {
		return errors.New("ChatGPT sign-in was declined or returned an incomplete callback")
	}

	// Client registration.
	clientID := callback.clientID
	if exists {
		if clientID != "" && clientID != selected.ClientID {
			return errors.New("ChatGPT returned a different client ID for the selected account")
		}
		clientID = selected.ClientID
	}
	if !validIssuedClientID(clientID) {
		return errors.New("ChatGPT registration did not return an issued client ID")
	}
	if !exists {
		// Retain the registration even if code exchange fails. It remains signed
		// out until its ID token has passed signature and identity validation.
		selected = oauthCredentials{Issuer: chatGPTIssuer, ClientID: clientID}
		store = store.withAccount(cfg.account, selected)
		if err := writeOAuthStore(path, store); err != nil {
			return fmt.Errorf("save ChatGPT registration: %w", err)
		}
	}

	// Token exchange and identity check.
	tokens, err := oauth.exchange(ctx, metadata.TokenEndpoint, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {clientID},
		"code": {callback.code}, "code_verifier": {verifier},
		"redirect_uri": {callbackURI}, "resource": {chatGPTResource},
	})
	if err != nil {
		return err
	}
	if tokens.IDToken == "" {
		return errors.New("ChatGPT sign-in returned no ID token")
	}
	identity, err := oauth.verifyIDToken(ctx, metadata, tokens.IDToken, clientID, nonce)
	if err != nil {
		return err
	}
	if selected.Subject != "" && selected.Subject != identity.Subject {
		return errors.New("ChatGPT identity changed; " +
			"sign in with the original account or use a new -account label")
	}

	// OAuth permits an omitted scope response when the grant matches this
	// authorization request. Explicit scopes replace these requested scopes.
	selected.Scopes = strings.Fields(parameters.Get("scope"))
	credentials, err := credentialsFromTokens(selected, tokens)
	if err != nil {
		return err
	}
	credentials.Subject, credentials.Email = identity.Subject, identity.Email
	if err := writeOAuthStore(path, store.withAccount(cfg.account, credentials)); err != nil {
		return fmt.Errorf("save ChatGPT credentials: %w", err)
	}

	fmt.Fprintf(output, "Signed in account %q (%q).\n", cfg.account, credentials.Email)
	if !credentials.hasPlanUsage() {
		fmt.Fprintln(output, "ChatGPT plan usage is disabled by the granted permissions. "+
			"Run login -reconsent to request it, or explicitly configure api-key mode.")
	} else {
		fmt.Fprintln(output, "ChatGPT plan usage is enabled. "+
			"Use the models command to list available models.")
	}
	return nil
}

// newOAuthSecrets generates the values that bind one sign-in attempt together.
func newOAuthSecrets() (state, nonce, verifier string, err error) {
	values := make([]string, 3)
	for index := range values {
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", "", "", errors.New("could not generate OAuth transaction secrets")
		}
		values[index] = base64.RawURLEncoding.EncodeToString(random[:])
	}
	return values[0], values[1], values[2], nil
}

// newCallbackHandler accepts one valid browser redirect and sends its result to
// callbacks. The channel needs room for one value, so that the handler can always
// finish its reply to the browser.
func newCallbackHandler(
	expectedHost, state string, callbacks chan<- oauthCallback,
) http.Handler {
	var consumed atomic.Bool
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")

		if request.Method != http.MethodGet || request.URL.Path != "/auth/callback" ||
			request.Host != expectedHost || len(request.URL.RawQuery) > maxCallbackQueryBytes {
			http.Error(writer, "Invalid callback.", http.StatusBadRequest)
			return
		}

		query, err := url.ParseQuery(request.URL.RawQuery)
		if err != nil || len(query["state"]) != 1 ||
			subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 {
			http.Error(writer, "Invalid OAuth state. Return to the original sign-in link.",
				http.StatusBadRequest)
			return
		}

		// A validated callback can complete this transaction only once.
		if !consumed.CompareAndSwap(false, true) {
			http.Error(writer, "This callback was already used.", http.StatusConflict)
			return
		}
		callback := oauthCallback{code: query.Get("code"), clientID: query.Get("client_id")}
		callback.failed = query.Get("error") != "" || len(query["code"]) != 1 ||
			len(query["client_id"]) > 1 || callback.code == "" ||
			len(callback.code) > maxAuthorizationCodeBytes
		callbacks <- callback
		fmt.Fprintln(writer, "Sign-in callback received. Return to the ant2oai terminal.")
	})
}

func (oauth oauthClient) revoke(ctx context.Context, credentials oauthCredentials) error {
	token, tokenTypeHint := credentials.RefreshToken, "refresh_token"
	if token == "" {
		token, tokenTypeHint = credentials.AccessToken, "access_token"
	}
	if token == "" {
		return nil
	}

	metadata, err := oauth.metadata(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, oauthRequestTimeout)
	defer cancel()

	form := url.Values{
		"token": {token}, "token_type_hint": {tokenTypeHint},
		"client_id": {credentials.ClientID},
	}
	for attempt := 0; attempt < 2; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost,
			metadata.RevocationEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return errors.New("could not create the ChatGPT revocation request")
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := oauth.client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
			if response.StatusCode < 500 {
				return errors.New("OpenAI did not confirm ChatGPT session revocation")
			}
		}

		// Revocation is idempotent. Retry one temporary failure with bounded
		// backoff while the local token is still available.
		if attempt == 0 {
			timer := time.NewTimer(revocationRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errors.New("ChatGPT session revocation was cancelled or timed out")
			case <-timer.C:
			}
		}
	}
	return errors.New("could not confirm ChatGPT session revocation after two attempts")
}

func (oauth oauthClient) listModels(ctx context.Context, token string, output io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, oauthRequestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, chatGPTResource+"/models", nil)
	if err != nil {
		return errors.New("could not create the ChatGPT models request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")

	response, err := oauth.client.Do(request)
	if err != nil {
		return errors.New("could not retrieve ChatGPT models; check network and TLS settings")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("ChatGPT model discovery returned HTTP %d; "+
			"check account and plan permissions", response.StatusCode)
	}

	// Catalog entries include instruction and capability metadata. Keep their
	// response limit independent from credential-bearing OAuth responses.
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxModelCatalogBytes+1))
	if err != nil {
		return errors.New("could not read the ChatGPT model catalog; " +
			"check the network and request timeout")
	}
	if len(raw) > maxModelCatalogBytes {
		return errors.New("ChatGPT model catalog exceeds the 8 MiB response limit")
	}
	if err := checkJSONDepth(raw); err != nil {
		return fmt.Errorf("decode ChatGPT model catalog: %w", err)
	}

	var catalog struct {
		Models []struct {
			Slug        string `json:"slug"`
			Visibility  string `json:"visibility"`
			DisplayName string `json:"display_name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &typeError) {
			return errors.New("ChatGPT model catalog contains unexpected field types")
		}
		return errors.New("ChatGPT model discovery returned invalid JSON")
	}
	if catalog.Models == nil {
		return errors.New("OpenAI returned an unexpected ChatGPT model catalog shape")
	}
	for _, model := range catalog.Models {
		if model.Visibility == "list" && model.Slug != "" {
			fmt.Fprintf(output, "%s\t%q\n", model.Slug, model.DisplayName)
		}
	}
	return nil
}
