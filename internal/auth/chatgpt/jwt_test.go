package chatgpt

import (
	"net/http"
	"testing"
	"time"
)

func TestJWTValidation(t *testing.T) {
	now := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	key := signingKey(t)
	otherKey := signingKey(t)
	tests := []struct {
		name     string
		mutate   func(map[string]any)
		nonce    string
		wrongKey bool
		unsigned bool
		wantErr  bool
	}{
		{name: "valid", nonce: "nonce"},
		{name: "wrong issuer", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["iss"] = "https://evil.invalid" }},
		{name: "wrong audience", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["aud"] = "other" }},
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
		{name: "unsigned token", nonce: "nonce", unsigned: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t)
			c.now = func() time.Time { return now }
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != jwksURL {
					t.Errorf("JWKS URL = %q, want %q", r.URL, jwksURL)
				}
				return jsonResponse(http.StatusOK, jwks(key))
			})
			payload := claims("nonce")
			payload["iat"], payload["exp"] = now.Unix(), now.Add(time.Hour).Unix()
			if tt.mutate != nil {
				tt.mutate(payload)
			}
			signing := key
			if tt.wrongKey {
				signing = otherKey
			}
			token := signed(t, signing, payload)
			if tt.unsigned {
				token = "eyJhbGciOiJub25lIn0.e30."
			}

			identity, err := c.provider.ValidateID(t.Context(), token, "oaiapp_test", tt.nonce, c.now)

			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateID error = %v, want error = %t", err, tt.wantErr)
			}
			if !tt.wantErr && identity.Subject != "subject" {
				t.Errorf("validated subject = %q, want subject", identity.Subject)
			}
		})
	}
}
