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

// ValidateID accepts only RS256; token-supplied key URLs and algorithms are never followed.
func (c Client) ValidateID(ctx context.Context, raw, clientID, nonce string, now func() time.Time) (Identity, error) {
	bad := ErrInvalidID
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Identity{}, bad
	}
	decode := base64.RawURLEncoding.DecodeString
	h, e := decode(parts[0])
	if e != nil {
		return Identity{}, bad
	}
	var header struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	if json.Unmarshal(h, &header) != nil || header.Alg != "RS256" || header.Kid == "" || len(header.Crit) > 0 {
		return Identity{}, bad
	}
	sig, e := decode(parts[2])
	if e != nil {
		return Identity{}, bad
	}
	var keys struct {
		Keys []jwk `json:"keys"`
	}
	if e = c.request(ctx, http.MethodGet, jwksURL, nil, "", &keys); e != nil {
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			return Identity{}, e
		}
		return Identity{}, fmt.Errorf("%w: cannot retrieve ChatGPT identity keys: %v", ErrNetwork, e)
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	valid, keyFound := validSignature(keys.Keys, header.Kid, hash, sig)
	if !keyFound {
		return Identity{}, fmt.Errorf("%w: ChatGPT identity signing key unavailable; retry later", ErrNetwork)
	}
	if !valid {
		return Identity{}, bad
	}
	var claims identityClaims
	payload, e := decode(parts[1])
	if e != nil || json.Unmarshal(payload, &claims) != nil {
		return Identity{}, bad
	}
	unix := now().Unix()
	if claims.Issuer != Issuer || claims.Subject == "" || claims.Exp <= unix || claims.Iat <= 0 || claims.Iat > unix+5 || claims.Nbf > unix+5 || (nonce != "" && claims.Nonce != nonce) {
		return Identity{}, bad
	}
	var aud string
	var audiences []string
	if json.Unmarshal(claims.Audience, &aud) == nil {
		audiences = []string{aud}
	} else if json.Unmarshal(claims.Audience, &audiences) != nil {
		return Identity{}, bad
	}
	if !slices.Contains(audiences, clientID) || ((len(audiences) > 1 || claims.AuthorizedParty != "") && claims.AuthorizedParty != clientID) {
		return Identity{}, bad
	}
	return Identity{Subject: claims.Subject, Email: claims.Email}, nil
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func validSignature(keys []jwk, kid string, hash [sha256.Size]byte, sig []byte) (valid, keyFound bool) {
	for _, k := range keys {
		if k.Kid != kid || k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, e := base64.RawURLEncoding.DecodeString(k.N)
		if e != nil {
			continue
		}
		eb, e := base64.RawURLEncoding.DecodeString(k.E)
		if e != nil || len(eb) > 4 {
			continue
		}
		exp := 0
		for _, b := range eb {
			exp = exp*256 + int(b)
		}
		if exp < 3 || exp%2 == 0 {
			continue
		}
		key := rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
		if key.N.BitLen() < 2048 {
			continue
		}
		keyFound = true
		if rsa.VerifyPKCS1v15(&key, crypto.SHA256, hash[:], sig) == nil {
			return true, true
		}
	}
	return false, keyFound
}
