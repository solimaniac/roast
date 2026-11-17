package chatgpt

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

func TestRefreshSerializedAndRotated(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	var calls atomic.Int32
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.String() != tokenURL {
			t.Error("wrong token target")
		}
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		if r.Form.Get("refresh_token") != "refresh" || r.Form.Get("client_id") != c.selected || r.Form.Get("scope") != "" {
			t.Error("bad refresh form")
		}
		return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "next-access", RefreshToken: "next-refresh", TokenType: "Bearer", ExpiresIn: 3600, Scope: scopeValue(oauth.Scope)})
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			token, e := c.Token(context.Background())
			if e != nil || token != "next-access" {
				t.Errorf("token refresh failed: %v", e)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh called %d times", calls.Load())
	}

	saved, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatalf("load rotated credential: %v", err)
	}
	if saved.AccessToken != "next-access" {
		t.Errorf("persisted access token = %q, want next-access", saved.AccessToken)
	}
	if saved.RefreshToken != "next-refresh" {
		t.Errorf("persisted refresh token = %q, want next-refresh", saved.RefreshToken)
	}
	if saved.Pending != nil {
		t.Error("successful refresh left a pending response")
	}
	if saved.Subject != "subject" || saved.ClientID != c.selected {
		t.Error("refresh changed registration identity")
	}

}

func TestEarliestRefresh(t *testing.T) {
	c := testClient(t)
	now := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	seed(t, c, now.Add(20*time.Second))
	v, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	v.EarliestRefreshAt = now.Add(30 * time.Second).Unix()
	if err := c.saveCredential(&v); err != nil {
		t.Fatal(err)
	}
	if _, e := c.Token(context.Background()); e != nil {
		t.Fatal(e)
	}
	v.ExpiresAt = now.Add(-time.Second)
	if err := c.saveCredential(&v); err != nil {
		t.Fatal(err)
	}
	if _, e := c.Token(context.Background()); !errors.Is(e, ErrAuth) {
		t.Fatalf("want auth failure: %v", e)
	}
}

func TestTerminalRefreshClearsGrantAndKeepsRegistration(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(400, map[string]string{"error": "invalid_grant", "error_description": "secret should not appear"})
	})
	_, e := c.Token(context.Background())
	if !errors.Is(e, ErrAuth) || strings.Contains(e.Error(), "secret") {
		t.Fatal("unsafe or unclassified error")
	}
	v, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	if v.RefreshToken != "" || v.Scopes != "" || !v.ExpiresAt.IsZero() || v.Subject != "subject" {
		t.Fatal("invalid grant retained credentials or lost its registration")
	}
	accounts, err := c.Status(t.Context())
	if err != nil || len(accounts) != 1 || accounts[0].SignedIn || accounts[0].PlanEnabled {
		t.Fatalf("status still reports a usable grant after invalid_grant: %v", err)
	}
}

func TestNetworkErrorDoesNotClearTokens(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret in transport error") })
	_, e := c.Token(context.Background())
	if !errors.Is(e, ErrNetwork) || strings.Contains(e.Error(), "secret") {
		t.Fatal("unsafe or unclassified error")
	}
	v, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	if v.RefreshToken == "" {
		t.Fatal("network failure erased refresh token")
	}
}

func TestRefreshPendingSurvivesJWKSFailure(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	key := signingKey(t)
	var refreshes, fetches int
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case tokenURL:
			refreshes++
			return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", IDToken: signed(t, key, claims("")), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600})
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
		t.Fatalf("pending tokens must not escape: %v", err)
	}
	saved, err := c.loadCredential(c.selected)
	if err != nil || saved.Pending == nil || saved.Pending.Response.RefreshToken != "rotated-refresh" || saved.AccessToken != "access" {
		t.Fatal("rotation not durably staged")
	}
	resumed := *c
	token, err := resumed.Token(context.Background())
	if err != nil || token != "rotated-access" || refreshes != 1 {
		t.Fatalf("did not resume validation: refreshes=%d err=%v", refreshes, err)
	}
	saved, err = c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Pending != nil || saved.RefreshToken != "rotated-refresh" {
		t.Fatal("pending token not committed")
	}
}

func TestPendingWrongIdentityNeverExposed(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	key := signingKey(t)
	v, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	cl := claims("")
	cl["sub"] = "different"
	v.Pending = &pendingRefresh{Response: oauth.TokenResponse{AccessToken: "wrong-account", RefreshToken: "new-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600}, ReceivedAt: time.Now()}
	if err := c.saveCredential(&v); err != nil {
		t.Fatal(err)
	}
	before, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	jwksCalls := 0
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != jwksURL {
			return nil, errors.New("unexpected request instead of JWKS validation")
		}
		jwksCalls++
		return jsonResponse(http.StatusOK, jwks(key))
	})

	token, err := c.Token(t.Context())

	if token != "" || !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("Token = %q, error = %v; want no token and identity-change authentication failure", token, err)
	}
	if jwksCalls != 1 {
		t.Errorf("JWKS calls = %d, want 1", jwksCalls)
	}
	after, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	assertClearedCredentials(t, after)
	if after.ClientID != before.ClientID || after.Subject != before.Subject {
		t.Error("rejection changed account identity")
	}
	if token, err := c.Token(t.Context()); token != "" || !errors.Is(err, ErrAuth) || jwksCalls != 1 {
		t.Fatalf("retry = %q, %v; JWKS calls = %d; want signed-out failure without network", token, err, jwksCalls)
	}
}

func TestPendingRefreshDelayedPastIDExpiry(t *testing.T) {
	for _, tt := range []struct {
		name        string
		delay       time.Duration
		wantRefresh bool
	}{{"access still valid", 20 * time.Minute, false}, {"access expired", 2 * time.Hour, true}} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t)
			received := time.Now().UTC().Truncate(time.Second)
			now := received.Add(tt.delay)
			c.now = func() time.Time { return now }
			seed(t, c, received.Add(-time.Minute))
			key := signingKey(t)
			cl := claims("")
			cl["iat"] = received.Unix()
			cl["exp"] = received.Add(10 * time.Minute).Unix()
			v, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			v.Pending = &pendingRefresh{Response: oauth.TokenResponse{AccessToken: "pending-access", RefreshToken: "pending-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600}, ReceivedAt: received}
			if err := c.saveCredential(&v); err != nil {
				t.Fatal(err)
			}
			refreshes := 0
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case jwksURL:
					return jsonResponse(http.StatusOK, jwks(key))
				case tokenURL:
					refreshes++
					if err := r.ParseForm(); err != nil {
						return nil, err
					}
					if r.Form.Get("refresh_token") != "pending-refresh" {
						t.Error("reused consumed refresh token")
					}
					return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600})
				default:
					t.Fatal("unexpected request")
					return nil, nil
				}
			})
			token, err := c.Token(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			saved, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantRefresh {
				if refreshes != 1 || token != "fresh-access" || saved.RefreshToken != "fresh-refresh" {
					t.Fatal("expired access was returned instead of refreshing replacement")
				}
			} else {
				if refreshes != 0 || token != "pending-access" || !saved.ExpiresAt.Equal(received.Add(time.Hour)) {
					t.Fatal("pending recovery extended actual access expiry")
				}
			}
			if saved.Pending != nil {
				t.Fatal("pending response was not committed")
			}
		})
	}
}

func TestPendingRefreshRejectsPermanentFailures(t *testing.T) {
	for _, name := range []string{"zero receipt", "pre-epoch receipt", "future receipt", "expired at receipt", "malformed ID token", "invalid signature", "incomplete response"} {
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			now := time.Now().UTC().Truncate(time.Second)
			c.now = func() time.Time { return now }
			seed(t, c, now.Add(-time.Minute))
			received := now.Add(-time.Hour)
			key := signingKey(t)
			cl := claims("")
			cl["iat"] = received.Add(-time.Minute).Unix()
			cl["exp"] = received.Add(time.Minute).Unix()
			switch name {
			case "zero receipt":
				received = time.Time{}
			case "pre-epoch receipt":
				received = time.Unix(-1, 0)
			case "future receipt":
				received = now.Add(time.Hour)
			case "expired at receipt":
				cl["exp"] = received.Unix()
			}
			v, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			v.Pending = &pendingRefresh{Response: oauth.TokenResponse{AccessToken: "pending-access", RefreshToken: "pending-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600}, ReceivedAt: received}
			switch name {
			case "malformed ID token":
				v.Pending.Response.IDToken = "malformed"
			case "invalid signature":
				v.Pending.Response.IDToken = signed(t, signingKey(t), cl)
			case "incomplete response":
				v.Pending.Response.TokenType = "unsupported"
			}
			if err := c.saveCredential(&v); err != nil {
				t.Fatal(err)
			}
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != jwksURL {
					t.Fatal("invalid pending record caused refresh")
				}
				return jsonResponse(http.StatusOK, jwks(key))
			})
			if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrAuth) {
				t.Fatalf("invalid receipt/identity accepted: %v", err)
			}
			saved, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			assertClearedCredentials(t, saved)
		})
	}
}

func TestRefreshRechecksExpiryAfterValidation(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Second, 2 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			c := testClient(t)
			received := time.Now().UTC().Truncate(time.Second)
			now := received
			c.now = func() time.Time { return now }
			seed(t, c, received.Add(-time.Minute))
			key := signingKey(t)
			cl := claims("")
			cl["iat"] = received.Unix()
			cl["exp"] = received.Add(time.Hour).Unix()
			refreshes := 0
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case tokenURL:
					refreshes++
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					want := "refresh"
					if refreshes > 1 {
						want = "rotated-refresh"
					}
					if r.Form.Get("refresh_token") != want {
						t.Fatal("reused consumed refresh token")
					}
					if refreshes == 1 {
						return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "short-access", RefreshToken: "rotated-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", ExpiresIn: 1, Scope: scopeValue(oauth.Scope)})
					}
					return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", TokenType: "Bearer", ExpiresIn: 3600, Scope: scopeValue(oauth.Scope)})
				case jwksURL:
					now = received.Add(delay)
					return jsonResponse(http.StatusOK, jwks(key))
				default:
					t.Fatal("unexpected request")
					return nil, nil
				}
			})
			token, err := c.Token(context.Background())
			if delay == 0 {
				if err != nil || token != "short-access" {
					t.Fatalf("valid token rejected: %v", err)
				}
				return
			}
			if token != "" || !errors.Is(err, ErrNetwork) || errors.Is(err, ErrAuth) {
				t.Fatalf("expired refresh must return a retryable error and no token: %v", err)
			}
			if refreshes != 1 {
				t.Fatal("retried refresh within the same call")
			}
			saved, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Pending != nil || saved.RefreshToken != "rotated-refresh" || !saved.ExpiresAt.Equal(received.Add(time.Second)) {
				t.Fatal("rotated credentials were not preserved with their original expiry")
			}
			token, err = c.Token(context.Background())
			if err != nil || token != "fresh-access" || refreshes != 2 {
				t.Fatalf("next call failed to recover using rotated credentials: %v", err)
			}
		})
	}
}

func assertClearedCredentials(t *testing.T, saved credential) {
	t.Helper()
	if saved.Pending != nil || saved.AccessToken != "" || saved.RefreshToken != "" || saved.IDToken != "" || saved.Scopes != "" || !saved.ExpiresAt.IsZero() || saved.EarliestRefreshAt != 0 {
		t.Error("rejected credentials or refresh metadata remain stored")
	}
}
