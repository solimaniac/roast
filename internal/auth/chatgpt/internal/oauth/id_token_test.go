package oauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/testutil"
)

func TestIDTokenValidation(t *testing.T) {
	now := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	key := testutil.NewRSAKey(t)
	otherKey := testutil.NewRSAKey(t)
	tests := []struct {
		name     string
		mutate   func(map[string]any)
		nonce    string
		wrongKey bool
		wantErr  bool
	}{
		{name: "valid", nonce: "nonce"},
		{name: "wrong issuer", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["iss"] = "https://evil.invalid" }},
		{name: "wrong audience", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["aud"] = "other" }},
		{name: "wrong authorized party", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["azp"] = "other" }},
		{name: "expired", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["exp"] = now.Add(-time.Minute).Unix() }},
		{name: "not yet valid", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["nbf"] = now.Add(time.Hour).Unix() }},
		{name: "wrong nonce", nonce: "wrong", wantErr: true},
		{name: "missing subject", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["sub"] = "" }},
		{name: "multiple audiences without authorized party", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["aud"] = []string{"oaiapp_test", "other"} }},
		{name: "multiple audiences with authorized party", nonce: "nonce", mutate: func(v map[string]any) {
			v["aud"] = []string{"oaiapp_test", "other"}
			v["azp"] = "oaiapp_test"
		}},
		{name: "wrong signature", nonce: "nonce", wrongKey: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient()
			c.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				const wantURL = "https://auth.openai.com/.well-known/jwks.json"
				if r.URL.String() != wantURL {
					t.Errorf("JWKS URL = %q, want %q", r.URL, wantURL)
				}
				body, err := json.Marshal(testutil.RS256KeySet(key))
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})
			payload := idTokenClaims(now)
			if tt.mutate != nil {
				tt.mutate(payload)
			}
			signing := key
			if tt.wrongKey {
				signing = otherKey
			}
			token := testutil.SignRS256(t, signing, payload)

			identity, err := c.ValidateID(t.Context(), token, "oaiapp_test", tt.nonce, func() time.Time { return now })

			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateID error = %v, want error = %t", err, tt.wantErr)
			}
			if !tt.wantErr && identity.Subject != "subject" {
				t.Errorf("validated subject = %q, want subject", identity.Subject)
			}
		})
	}
}

func TestIDTokenRejectsUnsignedBeforeFetchingKeys(t *testing.T) {
	c := NewClient()
	c.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("requested signing keys for an unsigned token")
		return nil, errors.New("unexpected key request")
	})
	payload, err := json.Marshal(idTokenClaims(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	token := encode([]byte(`{"alg":"none","kid":"test"}`)) + "." + encode(payload) + "."
	_, err = c.ValidateID(t.Context(), token, "oaiapp_test", "nonce", time.Now)
	if !errors.Is(err, ErrInvalidID) {
		t.Fatalf("ValidateID returned %v for an unsigned token", err)
	}
}

func idTokenClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss":   Issuer,
		"sub":   "subject",
		"aud":   "oaiapp_test",
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
		"nonce": "nonce",
	}
}
