package chatgpt

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
)

type identity struct {
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

// Only RS256 is accepted; token-supplied key URLs and algorithms are never followed.
func (c *Client) validateID(ctx context.Context, raw, clientID, nonce string) (identity, error) {
	var claims identity
	bad := errors.New("ChatGPT ID token validation failed")
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return claims, bad
	}
	decode := base64.RawURLEncoding.DecodeString
	h, e := decode(parts[0])
	if e != nil {
		return claims, bad
	}
	var header struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	if json.Unmarshal(h, &header) != nil || header.Alg != "RS256" || header.Kid == "" || len(header.Crit) > 0 {
		return claims, bad
	}
	sig, e := decode(parts[2])
	if e != nil {
		return claims, bad
	}
	var keys struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if e = c.request(ctx, http.MethodGet, jwksURL, nil, "", &keys); e != nil {
		return claims, e
	}
	verified := false
	for _, k := range keys.Keys {
		if k.Kid != header.Kid || k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, e := decode(k.N)
		if e != nil {
			continue
		}
		eb, e := decode(k.E)
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
		hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if rsa.VerifyPKCS1v15(&key, crypto.SHA256, hash[:], sig) == nil {
			verified = true
			break
		}
	}
	if !verified {
		return claims, bad
	}
	payload, e := decode(parts[1])
	if e != nil || json.Unmarshal(payload, &claims) != nil {
		return claims, bad
	}
	now := c.now().Unix()
	if claims.Issuer != Issuer || claims.Subject == "" || claims.Exp <= now || claims.Iat <= 0 || claims.Iat > now+5 || claims.Nbf > now+5 || (nonce != "" && claims.Nonce != nonce) {
		return identity{}, bad
	}
	var aud string
	var audiences []string
	if json.Unmarshal(claims.Audience, &aud) == nil {
		audiences = []string{aud}
	} else if json.Unmarshal(claims.Audience, &audiences) != nil {
		return identity{}, bad
	}
	found := false
	for _, a := range audiences {
		if a == clientID {
			found = true
		}
	}
	if !found || (len(audiences) > 1 && claims.AuthorizedParty != clientID) || (claims.AuthorizedParty != "" && claims.AuthorizedParty != clientID) {
		return identity{}, bad
	}
	return claims, nil
}
