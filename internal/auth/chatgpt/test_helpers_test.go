package chatgpt

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(b)))}
}
func testClient(t *testing.T) *Client {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := storage.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	provider := oauth.NewClient()
	provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected network request")
		return nil, errors.New("unexpected")
	})
	return &Client{dir: dir, now: time.Now, provider: provider}
}
func seed(t *testing.T, c *Client, exp time.Time) {
	t.Helper()
	c.selected = "oaiapp_test"
	if e := c.saveIndex(index{HostID: "host", Active: c.selected, Clients: []string{c.selected}}); e != nil {
		t.Fatal(e)
	}
	if e := c.save(credential{Account: Account{ClientID: c.selected, Subject: "subject", ExpiresAt: exp}, AccessToken: "access", RefreshToken: "refresh", Scopes: oauth.Scope}); e != nil {
		t.Fatal(e)
	}
}
func signingKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	return k
}
func signed(t *testing.T, k *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test"})
	p, _ := json.Marshal(claims)
	input := enc(h) + "." + enc(p)
	sum := sha256.Sum256([]byte(input))
	sig, e := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if e != nil {
		t.Fatal(e)
	}
	return input + "." + enc(sig)
}
func jwks(k *rsa.PrivateKey) any {
	return map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes())}}}
}

const (
	tokenURL     = "https://auth.openai.com/api/accounts/oauth/token"
	jwksURL      = "https://auth.openai.com/.well-known/jwks.json"
	discoveryURL = "https://auth.openai.com/.well-known/openid-configuration"
)

func claims(nonce string) map[string]any {
	return map[string]any{"iss": oauth.Issuer, "sub": "subject", "aud": "oaiapp_test", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce}
}
