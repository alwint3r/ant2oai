package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Fixed claim times keep these tests independent of the time when they run.
const (
	testIssuedAt  int64 = 1_000_000_000 // September 2001
	testExpiresAt int64 = 4_102_444_800 // January 2100

	testClientID = "app_client"
	testNonce    = "test-nonce"

	discoveryPath = "/.well-known/openid-configuration"

	trustedDiscovery = `{"issuer":"https://auth.openai.com",
		"authorization_endpoint":"https://auth.openai.com/oauth/authorize",
		"token_endpoint":"https://auth.openai.com/oauth/token",
		"revocation_endpoint":"https://auth.openai.com/oauth/revoke",
		"jwks_uri":"https://auth.openai.com/jwks"}`
)

// testSigningKey is generated once, because RSA key generation is slow.
var testSigningKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generate RSA key: " + err.Error())
	}
	return key
})

func encodeSegment(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func signPayload(payload string) string {
	digest := sha256.Sum256([]byte(payload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, testSigningKey(), crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(signature)
}

func signToken(header, claims map[string]any) string {
	payload := encodeSegment(header) + "." + encodeSegment(claims)
	return payload + "." + signPayload(payload)
}

func validHeader() map[string]any {
	return map[string]any{"alg": "RS256", "kid": "key-1"}
}

func validClaims() map[string]any {
	return map[string]any{
		"iss": chatGPTIssuer, "sub": "subject-1", "aud": testClientID, "exp": testExpiresAt,
		"iat": testIssuedAt, "nonce": testNonce, "email": "person@example.com",
	}
}

// withClaims returns the valid claims after applying overrides. A nil override
// removes the claim.
func withClaims(overrides map[string]any) map[string]any {
	claims := validClaims()
	for name, value := range overrides {
		if value == nil {
			delete(claims, name)
			continue
		}
		claims[name] = value
	}
	return claims
}

// testJWK returns the public signing key after applying overrides. A nil
// override removes the field.
func testJWK(overrides map[string]any) map[string]any {
	public := testSigningKey().PublicKey
	jwk := map[string]any{
		"kty": "RSA", "kid": "key-1", "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(public.E)).Bytes()),
	}
	for name, value := range overrides {
		if value == nil {
			delete(jwk, name)
			continue
		}
		jwk[name] = value
	}
	return jwk
}

func keySetJSON(keys ...map[string]any) string {
	data, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		panic(err)
	}
	return string(data)
}

func decodeKeySet(t *testing.T, source string) oidcSigningKeys {
	t.Helper()

	var keys oidcSigningKeys
	if err := json.Unmarshal([]byte(source), &keys); err != nil {
		t.Fatalf("decode key set: %v", err)
	}
	return keys
}

func verifyToken(t *testing.T, keys, token string) (oidcClaims, error) {
	t.Helper()

	metadata := oauthMetadata{Issuer: chatGPTIssuer}
	return verifyIDTokenWithKeys(metadata, decodeKeySet(t, keys), token, testClientID, testNonce)
}

func TestVerifyIDToken(t *testing.T) {
	keys := keySetJSON(testJWK(nil))

	t.Run("valid token", func(t *testing.T) {
		claims, err := verifyToken(t, keys, signToken(validHeader(), validClaims()))
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if claims.Subject != "subject-1" || claims.Email != "person@example.com" {
			t.Errorf("claims = %+v", claims)
		}
	})

	t.Run("refresh tokens skip the nonce check", func(t *testing.T) {
		token := signToken(validHeader(), withClaims(map[string]any{"nonce": nil}))
		metadata := oauthMetadata{Issuer: chatGPTIssuer}

		_, err := verifyIDTokenWithKeys(metadata, decodeKeySet(t, keys), token, testClientID, "")
		if err != nil {
			t.Errorf("verify: %v", err)
		}
	})
}

func TestVerifyIDTokenChecksClaims(t *testing.T) {
	const invalidIdentity = "OpenAI ID token identity or validity period is invalid"
	const wrongAudience = "OpenAI ID token audience does not match this registration"
	const nextYear int64 = 4_133_980_800

	cases := []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{name: "longest email", claims: map[string]any{"email": strings.Repeat("a", 320)}},
		{name: "longest subject", claims: map[string]any{"sub": strings.Repeat("a", 256)}},
		{name: "audience list", claims: map[string]any{"aud": []string{testClientID}}},
		{name: "matching authorized party", claims: map[string]any{"azp": testClientID}},
		{
			name: "several audiences with an authorized party",
			claims: map[string]any{
				"aud": []string{testClientID, "other"}, "azp": testClientID,
			},
		},
		{
			name:   "several audiences without an authorized party",
			claims: map[string]any{"aud": []string{testClientID, "other"}}, want: wrongAudience,
		},
		{name: "other audience", claims: map[string]any{"aud": "other"}, want: wrongAudience},
		{
			name:   "audience list without this client",
			claims: map[string]any{"aud": []string{"other"}}, want: wrongAudience,
		},
		{name: "other authorized party", claims: map[string]any{"azp": "other"}, want: wrongAudience},
		{
			name: "audience type", claims: map[string]any{"aud": 1},
			want: "OpenAI ID token audience is invalid",
		},
		{name: "other issuer", claims: map[string]any{"iss": "https://other"}, want: invalidIdentity},
		{name: "no subject", claims: map[string]any{"sub": ""}, want: invalidIdentity},
		{
			name: "subject over the limit", claims: map[string]any{"sub": strings.Repeat("a", 257)},
			want: invalidIdentity,
		},
		{
			name: "expired", claims: map[string]any{"exp": testIssuedAt + 1}, want: invalidIdentity,
		},
		{name: "no issue time", claims: map[string]any{"iat": 0}, want: invalidIdentity},
		{
			name:   "issued in the future",
			claims: map[string]any{"iat": testExpiresAt - 1}, want: invalidIdentity,
		},
		{
			name: "not valid yet", claims: map[string]any{"nbf": testExpiresAt - 1},
			want: invalidIdentity,
		},
		{
			name:   "expires before it was issued",
			claims: map[string]any{"iat": nextYear, "exp": testExpiresAt}, want: invalidIdentity,
		},
		{
			name: "other nonce", claims: map[string]any{"nonce": "other"},
			want: "OpenAI ID token nonce does not match the sign-in attempt",
		},
		{
			name: "no nonce", claims: map[string]any{"nonce": nil},
			want: "OpenAI ID token nonce does not match the sign-in attempt",
		},
		{
			name: "email over the limit", claims: map[string]any{"email": strings.Repeat("a", 321)},
			want: "OpenAI ID token email exceeds the supported limit",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			token := signToken(validHeader(), withClaims(testCase.claims))

			_, err := verifyToken(t, keySetJSON(testJWK(nil)), token)
			if testCase.want != "" {
				assertError(t, err, testCase.want)
				return
			}
			if err != nil {
				t.Errorf("verify: %v", err)
			}
		})
	}
}

func TestVerifyIDTokenChecksHeader(t *testing.T) {
	const unsupported = "OpenAI ID token uses an unsupported JWT header or claims encoding"

	cases := []struct {
		name   string
		header map[string]any
		want   string
	}{
		{"no signature algorithm", map[string]any{"alg": "none", "kid": "key-1"}, unsupported},
		{"HMAC algorithm", map[string]any{"alg": "HS256", "kid": "key-1"}, unsupported},
		{"no key ID", map[string]any{"alg": "RS256"}, unsupported},
		{
			"key ID over the limit",
			map[string]any{"alg": "RS256", "kid": strings.Repeat("k", 257)}, unsupported,
		},
		{
			"critical extension", map[string]any{"alg": "RS256", "kid": "key-1", "crit": []string{"x"}},
			unsupported,
		},
		{
			"unknown key", map[string]any{"alg": "RS256", "kid": "other"},
			"OpenAI ID token references an unknown signing key",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			token := signToken(testCase.header, validClaims())

			_, err := verifyToken(t, keySetJSON(testJWK(nil)), token)
			assertError(t, err, testCase.want)
		})
	}
}

func TestVerifyIDTokenChecksSigningKey(t *testing.T) {
	const unsupportedKey = "OpenAI ID token references an unsupported signing key"
	const invalidParameters = "OpenAI signing key contains invalid RSA parameters"
	const outsideLimits = "OpenAI signing key is outside the supported RSA limits"

	encoded := func(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

	cases := []struct {
		name string
		keys string
		want string
	}{
		{name: "optional fields are absent", keys: keySetJSON(testJWK(map[string]any{
			"alg": nil, "use": nil,
		}))},
		{
			name: "key operations include verify",
			keys: keySetJSON(testJWK(map[string]any{"key_ops": []string{"sign", "verify"}})),
		},
		{
			name: "other keys are ignored",
			keys: keySetJSON(testJWK(map[string]any{"kid": "other", "kty": "EC"}), testJWK(nil)),
		},
		{
			name: "duplicate key ID", keys: keySetJSON(testJWK(nil), testJWK(nil)),
			want: unsupportedKey,
		},
		{
			name: "key type", keys: keySetJSON(testJWK(map[string]any{"kty": "EC"})),
			want: unsupportedKey,
		},
		{
			name: "key algorithm", keys: keySetJSON(testJWK(map[string]any{"alg": "RS512"})),
			want: unsupportedKey,
		},
		{
			name: "key use", keys: keySetJSON(testJWK(map[string]any{"use": "enc"})),
			want: unsupportedKey,
		},
		{
			name: "key operations without verify",
			keys: keySetJSON(testJWK(map[string]any{"key_ops": []string{"sign"}})),
			want: "OpenAI signing key does not permit verification",
		},
		{
			name: "modulus encoding", keys: keySetJSON(testJWK(map[string]any{"n": "***"})),
			want: invalidParameters,
		},
		{
			name: "exponent encoding", keys: keySetJSON(testJWK(map[string]any{"e": "***"})),
			want: invalidParameters,
		},
		{
			name: "empty exponent", keys: keySetJSON(testJWK(map[string]any{"e": ""})),
			want: invalidParameters,
		},
		{
			name: "long exponent",
			keys: keySetJSON(testJWK(map[string]any{"e": encoded([]byte{1, 0, 0, 0, 1})})),
			want: invalidParameters,
		},
		{
			name: "small modulus",
			keys: keySetJSON(testJWK(map[string]any{"n": encoded(bytes.Repeat([]byte{1}, 64))})),
			want: outsideLimits,
		},
		{
			name: "large modulus",
			keys: keySetJSON(testJWK(map[string]any{"n": encoded(bytes.Repeat([]byte{255}, 1100))})),
			want: outsideLimits,
		},
		{
			name: "small exponent",
			keys: keySetJSON(testJWK(map[string]any{"e": encoded([]byte{1})})),
			want: outsideLimits,
		},
		{
			name: "even exponent",
			keys: keySetJSON(testJWK(map[string]any{"e": encoded([]byte{4})})),
			want: outsideLimits,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			token := signToken(validHeader(), validClaims())

			_, err := verifyToken(t, testCase.keys, token)
			if testCase.want != "" {
				assertError(t, err, testCase.want)
				return
			}
			if err != nil {
				t.Errorf("verify: %v", err)
			}
		})
	}
}

func TestVerifyIDTokenChecksEncoding(t *testing.T) {
	const invalidEncoding = "OpenAI ID token has an invalid JWT encoding"
	const unsupported = "OpenAI ID token uses an unsupported JWT header or claims encoding"
	const invalidSignature = "OpenAI ID token signature validation failed"

	token := signToken(validHeader(), validClaims())
	segments := strings.Split(token, ".")
	header, claims, signature := segments[0], segments[1], segments[2]
	deepSegment := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("[", 200)))
	tamperedClaims := encodeSegment(withClaims(map[string]any{"sub": "someone-else"}))

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	digest := sha256.Sum256([]byte(header + "." + claims))
	otherSignature, err := rsa.SignPKCS1v15(rand.Reader, otherKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"two segments", header + "." + claims, invalidEncoding},
		{"four segments", token + ".x", invalidEncoding},
		{"token over the limit", token + strings.Repeat("a", 64<<10), invalidEncoding},
		{"header encoding", "***." + claims + "." + signature, unsupported},
		{"claims encoding", header + ".***." + signature, unsupported},
		{"signature encoding", header + "." + claims + ".***", unsupported},
		{"deep header", deepSegment + "." + claims + "." + signature, unsupported},
		{"deep claims", header + "." + deepSegment + "." + signature, unsupported},
		{"changed claims", header + "." + tamperedClaims + "." + signature, invalidSignature},
		{
			"signature from another key",
			header + "." + claims + "." + base64.RawURLEncoding.EncodeToString(otherSignature),
			invalidSignature,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := verifyToken(t, keySetJSON(testJWK(nil)), testCase.token)
			assertError(t, err, testCase.want)
		})
	}
}

// fakeOAuthProvider replaces OpenAI's discovery, key, token, revocation, and
// model endpoints. Tests set only the replies that they need.
type fakeOAuthProvider struct {
	discoveryStatus int
	discovery       string
	keysStatus      int
	keys            string
	exchange        func(form url.Values) (int, string)
	revoke          func(attempt int) int
	models          func() (int, string)
	failedPaths     map[string]bool

	mutex       sync.Mutex
	tokenForms  []url.Values
	revokeCalls []url.Values
	modelTokens []string
}

func newFakeOAuthProvider() *fakeOAuthProvider {
	return &fakeOAuthProvider{
		discoveryStatus: http.StatusOK, discovery: trustedDiscovery,
		keysStatus: http.StatusOK, keys: keySetJSON(testJWK(nil)),
		exchange: func(url.Values) (int, string) { return http.StatusInternalServerError, "" },
		revoke:   func(int) int { return http.StatusOK },
		models: func() (int, string) {
			return http.StatusOK, `{"models":[]}`
		},
		failedPaths: map[string]bool{},
	}
}

func (provider *fakeOAuthProvider) client() oauthClient {
	return oauthClient{client: &http.Client{Transport: provider}}
}

func (provider *fakeOAuthProvider) RoundTrip(request *http.Request) (*http.Response, error) {
	if provider.failedPaths[request.URL.Path] {
		return nil, io.ErrUnexpectedEOF
	}

	var body []byte
	if request.Body != nil {
		var err error
		if body, err = io.ReadAll(request.Body); err != nil {
			return nil, err
		}
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}

	switch request.URL.Path {
	case discoveryPath:
		return upstreamReply(provider.discoveryStatus, nil, provider.discovery), nil
	case "/jwks":
		return upstreamReply(provider.keysStatus, nil, provider.keys), nil
	case "/oauth/token":
		provider.mutex.Lock()
		provider.tokenForms = append(provider.tokenForms, form)
		provider.mutex.Unlock()
		status, reply := provider.exchange(form)
		return upstreamReply(status, nil, reply), nil
	case "/oauth/revoke":
		provider.mutex.Lock()
		provider.revokeCalls = append(provider.revokeCalls, form)
		attempt := len(provider.revokeCalls) - 1
		provider.mutex.Unlock()
		return upstreamReply(provider.revoke(attempt), nil, ""), nil
	case "/v1/models":
		provider.mutex.Lock()
		provider.modelTokens = append(provider.modelTokens, request.Header.Get("Authorization"))
		provider.mutex.Unlock()
		status, reply := provider.models()
		return upstreamReply(status, nil, reply), nil
	}
	return upstreamReply(http.StatusNotFound, nil, ""), nil
}

func (provider *fakeOAuthProvider) tokenCallCount() int {
	provider.mutex.Lock()
	defer provider.mutex.Unlock()
	return len(provider.tokenForms)
}

func TestDiscoveryRejectsUntrustedMetadata(t *testing.T) {
	const untrusted = "OpenAI discovery returned an untrusted OAuth endpoint"
	replace := func(old, updated string) string {
		return strings.Replace(trustedDiscovery, old, updated, 1)
	}

	cases := []struct {
		name      string
		discovery string
		want      string
	}{
		{
			"issuer", replace(`"issuer":"https://auth.openai.com"`, `"issuer":"https://other"`),
			"OpenAI discovery returned an unexpected issuer",
		},
		{"plain HTTP", replace("https://auth.openai.com/oauth/token", "http://auth.openai.com/t"), untrusted},
		{"other host", replace("https://auth.openai.com/jwks", "https://other.test/jwks"), untrusted},
		{"query", replace("oauth/revoke", "oauth/revoke?a=1"), untrusted},
		{"fragment", replace("oauth/authorize", "oauth/authorize#a"), untrusted},
		{
			"credentials",
			replace("https://auth.openai.com/oauth/authorize", "https://a@auth.openai.com/oauth/authorize"),
			untrusted,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newFakeOAuthProvider()
			provider.discovery = testCase.discovery

			_, err := provider.client().metadata(context.Background())
			assertError(t, err, testCase.want)
		})
	}
}

func TestDiscoveryFailures(t *testing.T) {
	const invalidData = "OpenAI returned invalid OAuth data or exceeded its size limit"

	cases := []struct {
		name    string
		prepare func(*fakeOAuthProvider)
		want    string
	}{
		{
			"HTTP error",
			func(provider *fakeOAuthProvider) { provider.discoveryStatus = http.StatusBadGateway },
			"OpenAI OAuth discovery failed; check service availability",
		},
		{
			"network failure",
			func(provider *fakeOAuthProvider) { provider.failedPaths[discoveryPath] = true },
			"could not reach OpenAI OAuth discovery; check network and TLS settings",
		},
		{
			"malformed reply",
			func(provider *fakeOAuthProvider) { provider.discovery = "{" }, invalidData,
		},
		{
			"reply over the limit",
			func(provider *fakeOAuthProvider) { provider.discovery = strings.Repeat(" ", 257<<10) },
			invalidData,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newFakeOAuthProvider()
			testCase.prepare(provider)

			_, err := provider.client().metadata(context.Background())
			assertError(t, err, testCase.want)
		})
	}
}

func TestSigningKeyDiscovery(t *testing.T) {
	metadata := oauthMetadata{JWKSURI: "https://auth.openai.com/jwks"}

	cases := []struct {
		name string
		keys string
		want string
	}{
		{name: "one key", keys: keySetJSON(testJWK(nil))},
		{name: "no keys", keys: `{"keys":[]}`, want: "OpenAI JWKS contains no signing keys"},
		{
			name: "too many keys",
			keys: `{"keys":[` + strings.Repeat(`{},`, 64) + `{}]}`,
			want: "OpenAI JWKS exceeds the supported key limit",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newFakeOAuthProvider()
			provider.keys = testCase.keys

			_, err := provider.client().signingKeys(context.Background(), metadata)
			if testCase.want != "" {
				assertError(t, err, testCase.want)
				return
			}
			if err != nil {
				t.Errorf("signingKeys: %v", err)
			}
		})
	}
}
