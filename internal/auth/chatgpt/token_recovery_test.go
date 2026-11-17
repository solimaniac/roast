package chatgpt

import (
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

func TestPendingRefreshRetainsCheckpointWhenKeysUnavailable(t *testing.T) {
	for _, name := range []string{"HTTP failure", "missing signing key"} {
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			now := time.Now().UTC().Truncate(time.Second)
			c.now = func() time.Time { return now }
			seed(t, c, now.Add(-time.Minute))
			key := signingKey(t)
			v, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			v.Pending = &pendingRefresh{
				Response: oauth.TokenResponse{
					AccessToken: "replacement", RefreshToken: "rotated",
					IDToken: signed(t, key, claims("")), TokenType: "Bearer",
					ExpiresIn: 3600, Scope: scopeValue(oauth.Scope),
				},
				ReceivedAt: now,
			}
			if err := c.saveCredential(&v); err != nil {
				t.Fatal(err)
			}
			before, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != jwksURL {
					t.Error("pending recovery attempted another refresh")
				}
				if name == "HTTP failure" {
					return jsonResponse(http.StatusForbidden, map[string]string{"error": "unavailable"})
				}
				return jsonResponse(http.StatusOK, map[string]any{"keys": []any{}})
			})

			token, err := c.Token(t.Context())

			if token != "" || !errors.Is(err, ErrNetwork) || errors.Is(err, ErrAuth) || calls != 1 {
				t.Fatalf("Token = %q, %v; requests = %d; want retryable key failure", token, err, calls)
			}
			after, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Error("temporary key failure changed the durable checkpoint")
			}
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != jwksURL {
					t.Error("recovery replayed the consumed refresh token")
				}
				return jsonResponse(http.StatusOK, jwks(key))
			})
			if token, err := c.Token(t.Context()); err != nil || token != "replacement" {
				t.Fatalf("recovery = %q, %v", token, err)
			}
		})
	}
}

func TestRefreshPersistenceFailureProvidesRecoveryGuidance(t *testing.T) {
	for _, stage := range []string{"checkpoint", "validated credentials"} {
		t.Run(stage, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			key := signingKey(t)
			path := c.credentialPath(c.selected)
			backup := path + ".backup"
			blocked := false
			restore := func() {
				if !blocked {
					return
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(backup, path); err != nil {
					t.Fatal(err)
				}
				blocked = false
			}
			t.Cleanup(restore)
			blockWrite := func() {
				if err := os.Rename(path, backup); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				blocked = true
			}
			refreshes := 0
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case tokenURL:
					refreshes++
					if stage == "checkpoint" {
						blockWrite()
					}
					return jsonResponse(http.StatusOK, oauth.TokenResponse{
						AccessToken: "replacement", RefreshToken: "rotated",
						IDToken: signed(t, key, claims("")), TokenType: "Bearer",
						ExpiresIn: 3600, Scope: scopeValue(oauth.Scope),
					})
				case jwksURL:
					if stage == "validated credentials" && !blocked {
						blockWrite()
					}
					return jsonResponse(http.StatusOK, jwks(key))
				default:
					t.Fatalf("unexpected request: %s", r.URL.Path)
					return nil, nil
				}
			})

			token, err := c.Token(t.Context())

			if token != "" || err == nil || !strings.Contains(err.Error(), "repair credential storage") {
				t.Fatalf("Token = %q, %v; want storage recovery guidance without a token", token, err)
			}
			if stage == "checkpoint" && !strings.Contains(err.Error(), "could not confirm local credential cleanup") {
				t.Fatalf("failure incorrectly implies durable cleanup: %v", err)
			}
			restore()
			saved, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "checkpoint" {
				// When both writes fail, the consumed old grant can remain on disk.
				if saved.RefreshToken != "refresh" || saved.Pending != nil {
					t.Error("unexpected state after failed checkpoint and cleanup")
				}
				return
			}
			if saved.Pending == nil || saved.Pending.Response.RefreshToken != "rotated" {
				t.Fatal("failed final write lost the durable replacement")
			}
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != jwksURL {
					t.Error("recovery replayed a refresh")
				}
				return jsonResponse(http.StatusOK, jwks(key))
			})
			if token, err := c.Token(t.Context()); err != nil || token != "replacement" || refreshes != 1 {
				t.Fatalf("recovery = %q, %v; refreshes = %d", token, err, refreshes)
			}
		})
	}
}
