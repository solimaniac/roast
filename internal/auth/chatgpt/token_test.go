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
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
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
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "refresh" || r.Form.Get("client_id") != c.selected || r.Form.Get("scope") != "" {
			t.Error("bad refresh form")
		}
		return response(oauth.TokenResponse{AccessToken: "next-access", RefreshToken: "next-refresh", TokenType: "Bearer", ExpiresIn: 3600, Scope: scopeValue(oauth.Scope)}), nil
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
	v, e := c.load(c.selected)
	if e != nil || v.RefreshToken != "next-refresh" {
		t.Fatal("rotation not saved")
	}
	var saved credential
	if err := storage.ReadJSON(c.path(c.selected), &saved); err != nil {
		t.Fatal(err)
	}
}
func TestEarliestRefresh(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(20*time.Second))
	v, _ := c.load(c.selected)
	v.EarliestRefreshAt = time.Now().Add(30 * time.Second).Unix()
	_ = c.save(v)
	if _, e := c.Token(context.Background()); e != nil {
		t.Fatal(e)
	}
	v.ExpiresAt = time.Now().Add(-time.Second)
	_ = c.save(v)
	if _, e := c.Token(context.Background()); !errors.Is(e, ErrAuth) {
		t.Fatalf("want auth failure: %v", e)
	}
}
func TestTerminalRefreshClearsOnlyTokens(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		r := response(map[string]string{"error": "invalid_grant", "error_description": "secret should not appear"})
		r.StatusCode = 400
		return r, nil
	})
	_, e := c.Token(context.Background())
	if !errors.Is(e, ErrAuth) || strings.Contains(e.Error(), "secret") {
		t.Fatal("unsafe or unclassified error")
	}
	v, _ := c.load(c.selected)
	if v.AccessToken != "" || v.RefreshToken != "" || v.Subject != "subject" {
		t.Fatal("did not preserve registration while clearing tokens")
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
	v, _ := c.load(c.selected)
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
			return response(oauth.TokenResponse{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", IDToken: signed(t, key, claims("")), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600}), nil
		case jwksURL:
			fetches++
			if fetches == 1 {
				return nil, errors.New("temporary JWKS failure")
			}
			return response(jwks(key)), nil
		default:
			t.Fatal("unexpected request")
			return nil, nil
		}
	})
	if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrNetwork) {
		t.Fatalf("pending tokens must not escape: %v", err)
	}
	saved, err := c.load(c.selected)
	if err != nil || saved.Pending == nil || saved.Pending.Response.RefreshToken != "rotated-refresh" || saved.AccessToken != "access" {
		t.Fatal("rotation not durably staged")
	}
	resumed := *c
	token, err := resumed.Token(context.Background())
	if err != nil || token != "rotated-access" || refreshes != 1 {
		t.Fatalf("did not resume validation: refreshes=%d err=%v", refreshes, err)
	}
	saved, _ = c.load(c.selected)
	if saved.Pending != nil || saved.RefreshToken != "rotated-refresh" {
		t.Fatal("pending token not committed")
	}
}
func TestPendingWrongIdentityNeverExposed(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	key := signingKey(t)
	v, _ := c.load(c.selected)
	cl := claims("")
	cl["sub"] = "different"
	v.Pending = &pendingRefresh{Response: oauth.TokenResponse{AccessToken: "wrong-account", RefreshToken: "new-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600}, ReceivedAt: time.Now()}
	_ = c.save(v)
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return response(jwks(key)), nil })
	if token, err := c.Token(context.Background()); err == nil || token != "" {
		t.Fatal("unvalidated identity escaped")
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
			v, _ := c.load(c.selected)
			v.Pending = &pendingRefresh{Response: oauth.TokenResponse{AccessToken: "pending-access", RefreshToken: "pending-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600}, ReceivedAt: received}
			if err := c.save(v); err != nil {
				t.Fatal(err)
			}
			refreshes := 0
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case jwksURL:
					return response(jwks(key)), nil
				case tokenURL:
					refreshes++
					_ = r.ParseForm()
					if r.Form.Get("refresh_token") != "pending-refresh" {
						t.Error("reused consumed refresh token")
					}
					return response(oauth.TokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600}), nil
				default:
					t.Fatal("unexpected request")
					return nil, nil
				}
			})
			token, err := c.Token(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			saved, err := c.load(c.selected)
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
func TestPendingRefreshRejectsInvalidReceiptOrExpiredIdentity(t *testing.T) {
	for _, name := range []string{"zero receipt", "pre-epoch receipt", "future receipt", "expired at receipt"} {
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
			v, _ := c.load(c.selected)
			v.Pending = &pendingRefresh{Response: oauth.TokenResponse{AccessToken: "pending-access", RefreshToken: "pending-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600}, ReceivedAt: received}
			if err := c.save(v); err != nil {
				t.Fatal(err)
			}
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != jwksURL {
					t.Fatal("invalid pending record caused refresh")
				}
				return response(jwks(key)), nil
			})
			if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrAuth) {
				t.Fatalf("invalid receipt/identity accepted: %v", err)
			}
			saved, _ := c.load(c.selected)
			if saved.Pending == nil || saved.AccessToken != "access" {
				t.Fatal("invalid pending credentials became active")
			}
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
						return response(oauth.TokenResponse{AccessToken: "short-access", RefreshToken: "rotated-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", ExpiresIn: 1, Scope: scopeValue(oauth.Scope)}), nil
					}
					return response(oauth.TokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", TokenType: "Bearer", ExpiresIn: 3600, Scope: scopeValue(oauth.Scope)}), nil
				case jwksURL:
					now = received.Add(delay)
					return response(jwks(key)), nil
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
			saved, err := c.load(c.selected)
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
