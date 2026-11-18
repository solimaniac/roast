package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

func TestModelsCatalog(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != oauth.APIBaseURL+"/models" || r.Header.Get("Authorization") != "Bearer access" {
			t.Error("incorrect catalog request")
		}
		return jsonResponse(http.StatusOK, map[string]any{"models": []Model{{Slug: "model", DisplayName: "Example model", Visibility: "list"}}})
	})
	models, err := c.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].Slug != "model" {
		t.Fatal("catalog not parsed")
	}
}

func TestNewDoesNotCreateCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Token(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatal("missing login not classified")
	}
	if _, err = os.Stat(c.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only action created credential directory")
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// jsonResponse returns encoding failures through the mock transport, including
// when the request runs on a sign-in goroutine.
func jsonResponse(status int, value any) (*http.Response, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data)))}, nil
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
	if e := c.saveCredential(&credential{Account: Account{ClientID: c.selected, Subject: "subject", ExpiresAt: exp}, AccessToken: "access", RefreshToken: "refresh", Scopes: oauth.Scope}); e != nil {
		t.Fatal(e)
	}
}

const (
	tokenURL     = "https://auth.openai.com/api/accounts/oauth/token"
	jwksURL      = "https://auth.openai.com/.well-known/jwks.json"
	discoveryURL = "https://auth.openai.com/.well-known/openid-configuration"
)

func claims(nonce string) map[string]any {
	return map[string]any{"iss": oauth.Issuer, "sub": "subject", "aud": "oaiapp_test", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce}
}
