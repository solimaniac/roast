package chatgpt

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

func scopeValue(scope string) *string { return &scope }

func TestRefreshScopePresence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scope   any
		present bool
		want    string
	}{
		{"omitted", nil, false, oauth.Scope},
		{"null", nil, true, oauth.Scope},
		{"unchanged", oauth.Scope, true, oauth.Scope},
		{"reduced", "openid profile email", true, "openid profile email"},
		{"empty", "", true, ""},
		{"whitespace", " \t ", true, " \t "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			var calls int
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != tokenURL {
					t.Fatal("unexpected request")
				}
				data := map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "token_type": "Bearer", "expires_in": 3600}
				if tc.present {
					data["scope"] = tc.scope
				}
				return jsonResponse(http.StatusOK, data)
			})
			token, err := c.Token(context.Background())
			if tc.want == oauth.Scope {
				if err != nil || token != "rotated-access" {
					t.Fatalf("unchanged grant lost: %v", err)
				}
			} else if !errors.Is(err, ErrAuth) || token != "" || !strings.Contains(err.Error(), "permission was removed") {
				t.Fatalf("removed grant accepted: %v", err)
			}
			v, err := c.loadCredential(c.selected)
			if err != nil || v.Scopes != tc.want || v.RefreshToken != "rotated-refresh" || v.Pending != nil || calls != 1 {
				t.Fatalf("refresh result not committed correctly: %v", err)
			}
		})
	}
}

func TestOmittedRefreshScopeSurvivesPendingRecovery(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	key := signingKey(t)
	var refreshes, fetches int
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case tokenURL:
			refreshes++
			return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", IDToken: signed(t, key, claims("")), TokenType: "Bearer", ExpiresIn: 3600})
		case jwksURL:
			fetches++
			if fetches == 1 {
				return nil, errors.New("temporary failure")
			}
			return jsonResponse(http.StatusOK, jwks(key))
		default:
			t.Fatal("unexpected request")
			return nil, nil
		}
	})
	if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrNetwork) {
		t.Fatalf("pending result escaped: %v", err)
	}
	v, err := c.loadCredential(c.selected)
	if err != nil || v.Pending == nil || v.Pending.Response.Scope != nil || v.Scopes != oauth.Scope || v.Pending.Response.RefreshToken != "rotated-refresh" {
		t.Fatal("pending response lost scope absence or rotation")
	}
	resumed := *c
	if token, err := resumed.Token(context.Background()); err != nil || token != "rotated-access" || refreshes != 1 {
		t.Fatalf("pending recovery failed: %v", err)
	}
	v, err = c.loadCredential(c.selected)
	if err != nil || v.Scopes != oauth.Scope || v.Pending != nil || v.RefreshToken != "rotated-refresh" {
		t.Fatal("pending scope was not preserved")
	}
}

func TestInitialTokenDoesNotInheritScopes(t *testing.T) {
	c := testClient(t)
	v := credential{Scopes: oauth.Scope}
	if err := v.applyTokenResponse(oauth.TokenResponse{IDToken: "identity-only"}, c.now()); err != nil || v.Scopes != "" {
		t.Fatalf("initial authorization inherited an old grant: %v", err)
	}
}

func TestRefreshMetadataDoesNotDiscardRotatedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		hint any
	}{
		{"RFC3339", "2026-11-09T00:00:00Z"},
		{"numeric string", "1794182400"},
		{"invalid date", "not-a-date"},
		{"unexpected object", map[string]any{"unexpected": true}},
		{"null", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			key := signingKey(t)
			var refreshes, fetches int
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case tokenURL:
					refreshes++
					return jsonResponse(http.StatusOK, map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "id_token": signed(t, key, claims("")), "token_type": "Bearer", "expires_in": 3600, "earliest_refresh_at": tc.hint})
				case jwksURL:
					fetches++
					if fetches == 1 {
						return nil, errors.New("temporary JWKS failure")
					}
					return jsonResponse(http.StatusOK, jwks(key))
				default:
					t.Fatal("unexpected request")
					return nil, nil
				}
			})
			if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrNetwork) {
				t.Fatalf("pending credentials escaped or response lost: %v", err)
			}
			v, err := c.loadCredential(c.selected)
			if err != nil || v.Pending == nil || v.Pending.Response.RefreshToken != "rotated-refresh" || v.AccessToken != "access" {
				t.Fatal("rotated credentials were not checkpointed")
			}
			resumed := *c
			if token, err := resumed.Token(context.Background()); err != nil || token != "rotated-access" || refreshes != 1 {
				t.Fatalf("could not recover rotated credentials: %v", err)
			}
			v, err = c.loadCredential(c.selected)
			if err != nil || v.Pending != nil || v.RefreshToken != "rotated-refresh" || v.Scopes != oauth.Scope {
				t.Fatal("rotated credentials were not committed")
			}
		})
	}
}
