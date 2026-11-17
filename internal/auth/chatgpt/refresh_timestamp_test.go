package chatgpt

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

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
			v, err := c.load(c.selected)
			if err != nil || v.Pending == nil || v.Pending.Response.RefreshToken != "rotated-refresh" || v.AccessToken != "access" {
				t.Fatal("rotated credentials were not checkpointed")
			}
			resumed := *c
			if token, err := resumed.Token(context.Background()); err != nil || token != "rotated-access" || refreshes != 1 {
				t.Fatalf("could not recover rotated credentials: %v", err)
			}
			v, err = c.load(c.selected)
			if err != nil || v.Pending != nil || v.RefreshToken != "rotated-refresh" || v.Scopes != oauth.Scope {
				t.Fatal("rotated credentials were not committed")
			}
		})
	}
}
