package chatgpt

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func scopeValue(scope string) *string { return &scope }

func TestRefreshScopePresence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scope   any
		present bool
		want    string
	}{
		{"omitted", nil, false, Scope},
		{"null", nil, true, Scope},
		{"unchanged", Scope, true, Scope},
		{"reduced", "openid profile email", true, "openid profile email"},
		{"empty", "", true, ""},
		{"whitespace", " \t ", true, " \t "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			var calls int
			c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != tokenURL {
					t.Fatal("unexpected request")
				}
				data := map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "token_type": "Bearer", "expires_in": 3600}
				if tc.present {
					data["scope"] = tc.scope
				}
				return response(data), nil
			})
			token, err := c.Token(context.Background())
			if tc.want == Scope {
				if err != nil || token != "rotated-access" {
					t.Fatalf("unchanged grant lost: %v", err)
				}
			} else if !errors.Is(err, ErrAuth) || token != "" || !strings.Contains(err.Error(), "permission was removed") {
				t.Fatalf("removed grant accepted: %v", err)
			}
			v, err := c.load(c.selected)
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
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case tokenURL:
			refreshes++
			return response(tokenResponse{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", IDToken: signed(t, key, claims("")), TokenType: "Bearer", ExpiresIn: 3600}), nil
		case jwksURL:
			fetches++
			if fetches == 1 {
				return nil, errors.New("temporary failure")
			}
			return response(jwks(key)), nil
		default:
			t.Fatal("unexpected request")
			return nil, nil
		}
	})
	if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrNetwork) {
		t.Fatalf("pending result escaped: %v", err)
	}
	v, err := c.load(c.selected)
	if err != nil || v.Pending == nil || v.Pending.Response.Scope != nil || v.Scopes != Scope || v.Pending.Response.RefreshToken != "rotated-refresh" {
		t.Fatal("pending response lost scope absence or rotation")
	}
	resumed := *c
	if token, err := resumed.Token(context.Background()); err != nil || token != "rotated-access" || refreshes != 1 {
		t.Fatalf("pending recovery failed: %v", err)
	}
	v, err = c.load(c.selected)
	if err != nil || v.Scopes != Scope || v.Pending != nil || v.RefreshToken != "rotated-refresh" {
		t.Fatal("pending scope was not preserved")
	}
}

func TestInitialTokenDoesNotInheritScopes(t *testing.T) {
	c := testClient(t)
	v := credential{Scopes: Scope}
	if err := c.apply(&v, tokenResponse{IDToken: "identity-only"}); err != nil || v.Scopes != "" {
		t.Fatalf("initial authorization inherited an old grant: %v", err)
	}
}
