package testutil

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"
)

// NewRSAKey generates an RSA key for a test token fixture.
func NewRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// SignRS256 signs claims for a test token with the fixture key ID.
func SignRS256(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	h, err := json.Marshal(map[string]string{"alg": "RS256", "kid": "test"})
	if err != nil {
		t.Errorf("encode JWT header: %v", err)
		return ""
	}
	p, err := json.Marshal(claims)
	if err != nil {
		t.Errorf("encode JWT claims: %v", err)
		return ""
	}
	input := enc(h) + "." + enc(p)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Errorf("sign fixture token: %v", err)
		return ""
	}
	return input + "." + enc(sig)
}

// RS256KeySet returns a JWKS response containing the fixture public key.
func RS256KeySet(key *rsa.PrivateKey) map[string][]map[string]string {
	encode := base64.RawURLEncoding.EncodeToString
	return map[string][]map[string]string{"keys": {{
		"kty": "RSA",
		"kid": "test",
		"alg": "RS256",
		"use": "sig",
		"n":   encode(key.N.Bytes()),
		"e":   encode(big.NewInt(int64(key.E)).Bytes()),
	}}}
}
