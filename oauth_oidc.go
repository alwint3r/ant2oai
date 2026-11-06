package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	oauthRequestTimeout   = 30 * time.Second
	maxOAuthResponseBytes = 256 << 10
	maxIDTokenBytes       = 64 << 10
	maxSigningKeys        = 64
	maxEmailBytes         = 320
)

type oauthMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

type oauthClient struct {
	client *http.Client
}

func (oauth oauthClient) metadata(ctx context.Context) (oauthMetadata, error) {
	var metadata oauthMetadata
	err := oauth.getPublicJSON(ctx, chatGPTIssuer+"/.well-known/openid-configuration", &metadata)
	if err != nil {
		return metadata, err
	}
	if metadata.Issuer != chatGPTIssuer {
		return metadata, errors.New("OpenAI discovery returned an unexpected issuer")
	}
	for _, endpoint := range []string{metadata.AuthorizationEndpoint, metadata.TokenEndpoint,
		metadata.RevocationEndpoint, metadata.JWKSURI} {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host != "auth.openai.com" ||
			parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
			return metadata, errors.New("OpenAI discovery returned an untrusted OAuth endpoint")
		}
	}
	return metadata, nil
}

func (oauth oauthClient) getPublicJSON(ctx context.Context, endpoint string, target any) error {
	ctx, cancel := context.WithTimeout(ctx, oauthRequestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("could not create the OpenAI discovery request")
	}
	request.Header.Set("Accept", "application/json")

	response, err := oauth.client.Do(request)
	if err != nil {
		return errors.New("could not reach OpenAI OAuth discovery; check network and TLS settings")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("OpenAI OAuth discovery failed; check service availability")
	}
	return decodeOAuthJSON(response.Body, target)
}

// OAuth bodies may contain reusable credentials. Never include their contents
// or decoder errors in diagnostics, including errors returned by OpenAI.
func decodeOAuthJSON(body io.Reader, target any) error {
	raw, err := io.ReadAll(io.LimitReader(body, maxOAuthResponseBytes+1))
	if err != nil || len(raw) > maxOAuthResponseBytes || checkJSONDepth(raw) != nil ||
		json.Unmarshal(raw, target) != nil {
		return errors.New("OpenAI returned invalid OAuth data or exceeded its size limit")
	}
	return nil
}

type oidcSigningKey struct {
	Type       string   `json:"kty"`
	ID         string   `json:"kid"`
	Algorithm  string   `json:"alg"`
	Use        string   `json:"use"`
	Operations []string `json:"key_ops"`
	Modulus    string   `json:"n"`
	Exponent   string   `json:"e"`
}

type oidcSigningKeys struct {
	Keys []oidcSigningKey `json:"keys"`
}

func (oauth oauthClient) signingKeys(
	ctx context.Context, metadata oauthMetadata,
) (oidcSigningKeys, error) {
	// Resolve keys only through trusted discovery. Token-supplied URLs are ignored.
	var keys oidcSigningKeys
	if err := oauth.getPublicJSON(ctx, metadata.JWKSURI, &keys); err != nil {
		return oidcSigningKeys{}, err
	}
	if len(keys.Keys) == 0 {
		return oidcSigningKeys{}, errors.New("OpenAI JWKS contains no signing keys")
	}
	if len(keys.Keys) > maxSigningKeys {
		return oidcSigningKeys{}, errors.New("OpenAI JWKS exceeds the supported key limit")
	}
	return keys, nil
}

type oidcClaims struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Audience  json.RawMessage `json:"aud"`
	ExpiresAt int64           `json:"exp"`
	IssuedAt  int64           `json:"iat"`
	NotBefore int64           `json:"nbf"`
	Nonce     string          `json:"nonce"`
	Party     string          `json:"azp"`
	Email     string          `json:"email"`
}

func (oauth oauthClient) verifyIDToken(
	ctx context.Context, metadata oauthMetadata, token, clientID, nonce string,
) (oidcClaims, error) {
	keys, err := oauth.signingKeys(ctx, metadata)
	if err != nil {
		return oidcClaims{}, err
	}
	return verifyIDTokenWithKeys(metadata, keys, token, clientID, nonce)
}

// verifyIDTokenWithKeys checks a token in security order: the signature first,
// and the claims only after they are known to come from OpenAI.
func verifyIDTokenWithKeys(
	metadata oauthMetadata, keys oidcSigningKeys, token, clientID, nonce string,
) (oidcClaims, error) {
	parsed, err := parseIDToken(token)
	if err != nil {
		return oidcClaims{}, err
	}

	publicKey, err := findSigningKey(keys, parsed.keyID)
	if err != nil {
		return oidcClaims{}, err
	}
	digest := sha256.Sum256([]byte(parsed.signedContent))
	if rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], parsed.signature) != nil {
		return oidcClaims{}, errors.New("OpenAI ID token signature validation failed")
	}

	claims := parsed.claims
	if err := checkAudience(claims, clientID); err != nil {
		return oidcClaims{}, err
	}

	now := time.Now().Unix()
	if claims.Issuer != metadata.Issuer || claims.Subject == "" ||
		len(claims.Subject) > maxIDBytes || claims.ExpiresAt <= now || claims.IssuedAt <= 0 ||
		claims.IssuedAt > now+maxClockSkewSeconds ||
		claims.NotBefore > now+maxClockSkewSeconds || claims.ExpiresAt <= claims.IssuedAt {
		return oidcClaims{}, errors.New("OpenAI ID token identity or validity period is invalid")
	}
	if nonce != "" && claims.Nonce != nonce {
		return oidcClaims{}, errors.New("OpenAI ID token nonce does not match the sign-in attempt")
	}
	if len(claims.Email) > maxEmailBytes {
		return oidcClaims{}, errors.New("OpenAI ID token email exceeds the supported limit")
	}
	return claims, nil
}

const maxClockSkewSeconds = 60

// parsedIDToken holds a decoded token whose signature is not verified yet.
type parsedIDToken struct {
	keyID         string
	claims        oidcClaims
	signature     []byte
	signedContent string
}

func parseIDToken(token string) (parsedIDToken, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > maxIDTokenBytes {
		return parsedIDToken{}, errors.New("OpenAI ID token has an invalid JWT encoding")
	}

	encoding := base64.RawURLEncoding.Strict()
	headerJSON, headerError := encoding.DecodeString(parts[0])
	claimsJSON, claimsError := encoding.DecodeString(parts[1])
	signature, signatureError := encoding.DecodeString(parts[2])

	var claims oidcClaims
	var header struct {
		Algorithm string          `json:"alg"`
		KeyID     string          `json:"kid"`
		Critical  json.RawMessage `json:"crit"`
	}
	if headerError != nil || claimsError != nil || signatureError != nil ||
		checkJSONDepth(headerJSON) != nil || checkJSONDepth(claimsJSON) != nil ||
		json.Unmarshal(headerJSON, &header) != nil || json.Unmarshal(claimsJSON, &claims) != nil ||
		header.Algorithm != "RS256" || header.KeyID == "" || len(header.KeyID) > maxIDBytes ||
		hasValue(header.Critical) {
		return parsedIDToken{}, errors.New("OpenAI ID token uses an unsupported JWT header " +
			"or claims encoding")
	}

	return parsedIDToken{
		keyID: header.KeyID, claims: claims, signature: signature,
		signedContent: parts[0] + "." + parts[1],
	}, nil
}

// findSigningKey returns the only usable key with this ID. A repeated ID is an
// error, because the choice between two keys would be arbitrary.
func findSigningKey(keys oidcSigningKeys, keyID string) (*rsa.PublicKey, error) {
	var publicKey *rsa.PublicKey
	for _, key := range keys.Keys {
		if key.ID != keyID {
			continue
		}
		if publicKey != nil || key.Type != "RSA" ||
			key.Algorithm != "" && key.Algorithm != "RS256" || key.Use != "" && key.Use != "sig" {
			return nil, errors.New("OpenAI ID token references an unsupported signing key")
		}

		parsed, err := key.rsaPublicKey()
		if err != nil {
			return nil, err
		}
		publicKey = parsed
	}

	if publicKey == nil {
		return nil, errors.New("OpenAI ID token references an unknown signing key")
	}
	return publicKey, nil
}

func (key oidcSigningKey) rsaPublicKey() (*rsa.PublicKey, error) {
	if len(key.Operations) != 0 {
		canVerify := false
		for _, operation := range key.Operations {
			canVerify = canVerify || operation == "verify"
		}
		if !canVerify {
			return nil, errors.New("OpenAI signing key does not permit verification")
		}
	}

	encoding := base64.RawURLEncoding.Strict()
	modulus, modulusError := encoding.DecodeString(key.Modulus)
	exponent, exponentError := encoding.DecodeString(key.Exponent)
	if modulusError != nil || exponentError != nil || len(exponent) < 1 || len(exponent) > 4 {
		return nil, errors.New("OpenAI signing key contains invalid RSA parameters")
	}
	exponentValue := 0
	for _, value := range exponent {
		exponentValue = exponentValue<<8 | int(value)
	}

	publicKey := &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: exponentValue}
	if publicKey.N.BitLen() < 2048 || publicKey.N.BitLen() > 8192 ||
		publicKey.E < 3 || publicKey.E > 1<<31-1 || publicKey.E%2 != 1 {
		return nil, errors.New("OpenAI signing key is outside the supported RSA limits")
	}
	return publicKey, nil
}

func checkAudience(claims oidcClaims, clientID string) error {
	var audiences []string
	var singleAudience string
	if json.Unmarshal(claims.Audience, &singleAudience) == nil {
		audiences = []string{singleAudience}
	} else if json.Unmarshal(claims.Audience, &audiences) != nil {
		return errors.New("OpenAI ID token audience is invalid")
	}

	matchesAudience := false
	for _, audience := range audiences {
		matchesAudience = matchesAudience || audience == clientID
	}
	if !matchesAudience || claims.Party != "" && claims.Party != clientID ||
		len(audiences) > 1 && claims.Party != clientID {
		return errors.New("OpenAI ID token audience does not match this registration")
	}
	return nil
}
