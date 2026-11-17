package oauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"
)

type identityClaims struct {
	Subject         string          `json:"sub"`
	Email           string          `json:"email"`
	Issuer          string          `json:"iss"`
	Audience        json.RawMessage `json:"aud"`
	AuthorizedParty string          `json:"azp"`
	Nonce           string          `json:"nonce"`
	Exp             int64           `json:"exp"`
	Iat             int64           `json:"iat"`
	Nbf             int64           `json:"nbf"`
}

type Identity struct {
	Subject string
	Email   string
}

// ErrInvalidID identifies a rejected token, not a failure to retrieve its keys.
var ErrInvalidID = errors.New("ChatGPT ID token validation failed")

// ValidateID verifies an RS256 ID token against the issuer's keys and the
// registration that requested it. Sign-in also requires the original nonce.
func (c Client) ValidateID(ctx context.Context, raw, clientID, nonce string, now func() time.Time) (Identity, error) {
	parts := strings.SplitN(raw, ".", 4)
	if len(parts) != 3 {
		return Identity{}, ErrInvalidID
	}
	var header struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	if !decodeTokenPart(parts[0], &header) || header.Alg != "RS256" || header.Kid == "" || len(header.Crit) > 0 {
		return Identity{}, ErrInvalidID
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, ErrInvalidID
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := c.verifyIDSignature(ctx, header.Kid, hash, sig); err != nil {
		return Identity{}, err
	}
	var claims identityClaims
	if !decodeTokenPart(parts[1], &claims) || !claims.validFor(clientID, nonce, now()) {
		return Identity{}, ErrInvalidID
	}
	return Identity{Subject: claims.Subject, Email: claims.Email}, nil
}

func decodeTokenPart(part string, dst any) bool {
	data, err := base64.RawURLEncoding.DecodeString(part)
	return err == nil && json.Unmarshal(data, dst) == nil
}

func (c Client) verifyIDSignature(ctx context.Context, keyID string, hash [sha256.Size]byte, sig []byte) error {
	var keys struct {
		Keys []jwk `json:"keys"`
	}
	if err := c.request(ctx, http.MethodGet, jwksURL, nil, "", &keys); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: cannot retrieve ChatGPT identity keys: %v", ErrNetwork, err)
	}
	// A rotated key may not be available yet. Only a signature checked against
	// a matching key can permanently invalidate a pending credential.
	var keyFound bool
	for _, record := range keys.Keys {
		key := record.signingKey(keyID)
		if key == nil {
			continue
		}
		keyFound = true
		if rsa.VerifyPKCS1v15(key, crypto.SHA256, hash[:], sig) == nil {
			return nil
		}
	}
	if keyFound {
		return ErrInvalidID
	}
	return fmt.Errorf("%w: ChatGPT identity signing key unavailable; retry later", ErrNetwork)
}

func (claims identityClaims) validFor(clientID, nonce string, now time.Time) bool {
	if claims.Issuer != Issuer || claims.Subject == "" || (nonce != "" && claims.Nonce != nonce) {
		return false
	}
	unix := now.Unix()
	if claims.Exp <= unix || claims.Iat <= 0 || claims.Iat > unix+5 || claims.Nbf > unix+5 {
		return false
	}
	// OIDC permits a single audience string or an array. An explicit azp
	// must match this registration and is required for multiple audiences.
	var audience string
	var audiences []string
	if json.Unmarshal(claims.Audience, &audience) == nil {
		audiences = []string{audience}
	} else if json.Unmarshal(claims.Audience, &audiences) != nil {
		return false
	}
	if len(audiences) > 1 || claims.AuthorizedParty != "" {
		if claims.AuthorizedParty != clientID {
			return false
		}
	}
	return clientID != "" && slices.Contains(audiences, clientID)
}

type jwk struct {
	Type     string `json:"kty"`
	ID       string `json:"kid"`
	Use      string `json:"use"`
	Alg      string `json:"alg"`
	Modulus  string `json:"n"`
	Exponent string `json:"e"`
}

func (k jwk) signingKey(id string) *rsa.PublicKey {
	if k.ID != id || k.Type != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
		return nil
	}
	modulus, err := base64.RawURLEncoding.DecodeString(k.Modulus)
	if err != nil {
		return nil
	}
	exponent, err := base64.RawURLEncoding.DecodeString(k.Exponent)
	if err != nil || len(exponent) > 4 {
		return nil
	}
	key := &rsa.PublicKey{N: new(big.Int).SetBytes(modulus)}
	for _, b := range exponent {
		key.E = key.E<<8 | int(b)
	}
	if key.N.BitLen() < 2048 || key.E < 3 || key.E%2 == 0 {
		return nil
	}
	return key
}
