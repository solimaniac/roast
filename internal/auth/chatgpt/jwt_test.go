package chatgpt

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestJWTValidation(t *testing.T) {
	k := signingKey(t)
	c := testClient(t)
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != jwksURL {
			t.Fatalf("wrong JWKS URL %s", r.URL)
		}
		return response(jwks(k)), nil
	})
	tests := []struct {
		name   string
		mutate func(map[string]any)
		nonce  string
		ok     bool
	}{{"valid", func(map[string]any) {}, "nonce", true}, {"issuer", func(v map[string]any) { v["iss"] = "https://evil.invalid" }, "nonce", false}, {"audience", func(v map[string]any) { v["aud"] = "other" }, "nonce", false}, {"expired", func(v map[string]any) { v["exp"] = time.Now().Add(-time.Minute).Unix() }, "nonce", false}, {"future", func(v map[string]any) { v["nbf"] = time.Now().Add(time.Hour).Unix() }, "nonce", false}, {"nonce", func(map[string]any) {}, "wrong", false}, {"no subject", func(v map[string]any) { v["sub"] = "" }, "nonce", false}, {"multi aud", func(v map[string]any) { v["aud"] = []string{"oaiapp_test", "other"} }, "nonce", false}, {"multi aud azp", func(v map[string]any) { v["aud"] = []string{"oaiapp_test", "other"}; v["azp"] = "oaiapp_test" }, "nonce", true}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := claims("nonce")
			tt.mutate(v)
			_, e := c.provider.ValidateID(context.Background(), signed(t, k, v), "oaiapp_test", tt.nonce, c.now)
			if (e == nil) != tt.ok {
				t.Fatalf("error=%v want valid=%t", e, tt.ok)
			}
		})
	}
	other := signingKey(t)
	if _, e := c.provider.ValidateID(context.Background(), signed(t, other, claims("nonce")), "oaiapp_test", "nonce", c.now); e == nil {
		t.Fatal("accepted wrong signature")
	}
	if _, e := c.provider.ValidateID(context.Background(), "eyJhbGciOiJub25lIn0.e30.", "oaiapp_test", "nonce", c.now); e == nil {
		t.Fatal("accepted unsigned token")
	}
}
