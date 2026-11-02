package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type oauthTokenResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	IDToken      string  `json:"id_token"`
	TokenType    string  `json:"token_type"`
	ExpiresIn    int64   `json:"expires_in"`
	Scope        *string `json:"scope"`
}

type oauthExchangeError struct {
	code           string
	status         int
	invalidSuccess bool
}

func (failure *oauthExchangeError) Error() string {
	if failure.invalidSuccess {
		return "OpenAI returned an invalid token set; sign in again"
	}
	if failure.unusable() {
		return "ChatGPT credentials expired or were revoked; run ant2oai login for the selected account"
	}
	if failure.code == "invalid_client" {
		return "OpenAI rejected this client registration; check the saved account registration"
	}
	return fmt.Sprintf("ChatGPT token exchange returned HTTP %d; check OpenAI availability and sign in again if needed",
		failure.status)
}

func (failure *oauthExchangeError) unusable() bool {
	switch failure.code {
	case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired",
		"refresh_token_invalidated", "refresh_token_reused":
		return true
	}
	return failure.invalidSuccess
}

func (oauth oauthClient) exchange(
	ctx context.Context, endpoint string, form url.Values,
) (oauthTokenResponse, error) {
	var tokens oauthTokenResponse
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return tokens, errors.New("could not create the ChatGPT token exchange")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := oauth.client.Do(request)
	if err != nil {
		return tokens, errors.New("could not reach OpenAI for token exchange; check network and TLS settings")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		// An unreadable error body still has a useful numeric HTTP status.
		if decodeOAuthJSON(response.Body, &failure) != nil {
			failure.Error = ""
		}
		return tokens, &oauthExchangeError{code: failure.Error, status: response.StatusCode}
	}
	if decodeOAuthJSON(response.Body, &tokens) != nil || tokens.AccessToken == "" ||
		!validOAuthSecret(tokens.AccessToken) || !validOAuthSecret(tokens.RefreshToken) ||
		!validOAuthSecret(tokens.IDToken) || !strings.EqualFold(tokens.TokenType, "Bearer") ||
		tokens.ExpiresIn <= 0 || tokens.ExpiresIn > 24*60*60 {
		return oauthTokenResponse{}, &oauthExchangeError{invalidSuccess: true}
	}
	return tokens, nil
}

func credentialsFromTokens(
	previous oauthCredentials, tokens oauthTokenResponse,
) (oauthCredentials, error) {
	updated := previous
	updated.AccessToken = tokens.AccessToken
	updated.RefreshToken = tokens.RefreshToken
	updated.ExpiresAt = time.Now().UTC().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	if tokens.IDToken != "" {
		updated.IDToken = tokens.IDToken
	}
	if tokens.Scope != nil {
		if len(*tokens.Scope) > 8192 {
			return previous, errors.New("OpenAI token response contains an oversized scope set")
		}
		updated.Scopes = strings.Fields(*tokens.Scope)
		if len(updated.Scopes) > 64 {
			return previous, errors.New("OpenAI token response exceeds the supported scope limit")
		}
	}
	if updated.hasScope("offline_access") && updated.RefreshToken == "" {
		return previous, errors.New("OpenAI granted offline access without a refresh token; sign in again")
	}
	return updated, nil
}

type oauthRefresh struct {
	done chan struct{}
	err  error
}

type oauthSession struct {
	path    string
	account string
	oauth   oauthClient
	lock    *credentialLock

	mutex      sync.Mutex
	store      oauthStore
	refreshing *oauthRefresh
	isClosed   bool
}

func openOAuthSession(cfg config, client *http.Client) (*oauthSession, error) {
	path, err := credentialPath(cfg)
	if err != nil {
		return nil, err
	}
	lock, err := lockCredentials(path)
	if err != nil {
		return nil, err
	}
	store, err := readOAuthStore(path)
	if errors.Is(err, os.ErrNotExist) {
		err = errors.New("no ChatGPT credentials found; run ant2oai login " +
			"with the same config, account, and auth-file settings")
	}
	if err == nil {
		selected := store.Accounts[cfg.account]
		if selected.AccessToken == "" {
			err = errors.New("no tokens for the selected account; run ant2oai login")
		} else if !selected.hasScope(chatGPTScope) || !selected.hasScope("resource.invoke") {
			err = errors.New("ChatGPT plan usage is disabled; run ant2oai login -reconsent " +
				"or explicitly select api-key mode")
		}
	}
	if err != nil {
		return nil, errors.Join(err, lock.close())
	}
	return &oauthSession{path: path, account: cfg.account, store: store,
		oauth: oauthClient{client: client}, lock: lock}, nil
}

// Stop new renewals before waiting for the current credential writer. If the
// caller's cleanup deadline expires, retain the file lock so a second process
// cannot write credentials while this process is still shutting down.
func (session *oauthSession) close(ctx context.Context) error {
	session.mutex.Lock()
	session.isClosed = true
	pending := session.refreshing
	session.mutex.Unlock()
	if pending != nil {
		select {
		case <-pending.done:
		case <-ctx.Done():
			return errors.New("credential renewal is still active; the lock was retained. " +
				"Verify this process has stopped before removing the .lock file")
		}
	}
	return session.lock.close()
}

// accessToken may block on one OAuth refresh. Network operations have 30-second
// deadlines and respect ctx. Concurrent callers share the refresh result. The
// process lock serializes refreshes against other proxy and login processes.
func (session *oauthSession) accessToken(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", errors.New("ChatGPT credential request was cancelled")
		}
		session.mutex.Lock()
		if session.isClosed {
			session.mutex.Unlock()
			return "", errors.New("the ChatGPT credential session is shutting down")
		}
		selected := session.store.Accounts[session.account]
		if selected.AccessToken == "" {
			session.mutex.Unlock()
			return "", errors.New("ChatGPT credentials are unavailable; stop the proxy and run ant2oai login")
		}
		if !selected.hasScope(chatGPTScope) || !selected.hasScope("resource.invoke") {
			session.mutex.Unlock()
			return "", errors.New("ChatGPT plan usage is disabled; stop the proxy and run login -reconsent")
		}
		if time.Until(selected.ExpiresAt) > time.Minute {
			session.mutex.Unlock()
			return selected.AccessToken, nil
		}
		if pending := session.refreshing; pending != nil {
			session.mutex.Unlock()
			select {
			case <-ctx.Done():
				return "", errors.New("ChatGPT credential request was cancelled")
			case <-pending.done:
				if pending.err != nil {
					return "", pending.err
				}
			}
			continue
		}

		pending := &oauthRefresh{done: make(chan struct{})}
		session.refreshing = pending
		snapshot := session.store
		session.mutex.Unlock()

		// The immutable snapshot allows network and disk I/O without the mutex.
		updated, refreshError := session.refresh(ctx, snapshot, selected)
		session.mutex.Lock()
		session.store = updated
		pending.err = refreshError
		session.refreshing = nil
		close(pending.done)
		session.mutex.Unlock()
		if refreshError != nil {
			return "", refreshError
		}
		fresh := updated.Accounts[session.account]
		if !fresh.hasScope(chatGPTScope) || !fresh.hasScope("resource.invoke") {
			return "", errors.New("ChatGPT plan permission was removed; stop the proxy and run login -reconsent")
		}
		return fresh.AccessToken, nil
	}
}

func (session *oauthSession) refresh(
	ctx context.Context, store oauthStore, selected oauthCredentials,
) (oauthStore, error) {
	if selected.RefreshToken == "" {
		return store, errors.New("this ChatGPT session cannot renew; stop the proxy and run ant2oai login")
	}
	metadata, err := session.oauth.metadata(ctx)
	if err != nil {
		return store, err
	}

	// Fetch keys before the exchange can rotate the refresh token. Verification
	// uses this snapshot so a later discovery outage cannot discard the new grant.
	keys, err := session.oauth.signingKeys(ctx, metadata)
	if err != nil {
		return store, err
	}

	tokens, err := session.oauth.exchange(ctx, metadata.TokenEndpoint, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {selected.ClientID},
		"refresh_token": {selected.RefreshToken}, "resource": {chatGPTResource},
	})
	if err != nil {
		var failure *oauthExchangeError
		if !errors.As(err, &failure) || !failure.unusable() {
			return store, err
		}
	} else {
		// A successful refresh rotates the refresh token. Any validation or
		// persistence failure below makes the previous token set unusable.
		var updated oauthCredentials
		updated, err = credentialsFromTokens(selected, tokens)
		if err == nil && tokens.RefreshToken == "" {
			err = errors.New("ChatGPT refresh returned no replacement refresh token; sign in again")
		}
		if err == nil && tokens.IDToken != "" {
			var identity oidcClaims
			identity, err = verifyIDTokenWithKeys(metadata, keys,
				tokens.IDToken, selected.ClientID, "")
			if err == nil && identity.Subject != selected.Subject {
				err = errors.New("ChatGPT refresh changed the verified identity; sign in again")
			}
		}
		if err == nil {
			replacement := store.withAccount(session.account, updated)
			if writeError := writeOAuthStore(session.path, replacement); writeError == nil {
				return replacement, nil
			}
			err = errors.New("could not save rotated ChatGPT credentials; stop the proxy and sign in again")
		}
	}

	cleared := store.withAccount(session.account, selected.withoutTokens())
	if writeError := writeOAuthStore(session.path, cleared); writeError != nil {
		return cleared, errors.New("could not clear unusable ChatGPT credentials; " +
			"stop the proxy, check credential storage, and sign in again")
	}
	return cleared, err
}
