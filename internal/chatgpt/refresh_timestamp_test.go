package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRefreshTimestampRepresentations(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      int64
	}{
		{"seconds", `1794182400`, 1794182400},
		{"numeric string", `"1794182400"`, 1794182400},
		{"fraction", `1794182400.25`, 1794182401},
		{"sub-ULP fraction", `1794182400.0000001`, 1794182401},
		{"sub-ULP string", `"1794182400.0000001"`, 1794182401},
		{"tiny decimal remainder", `1794182400.0000000000000000000000000000000000000000000000000000000000000000000000000000000000001`, 1794182401},
		{"exponent", `1.7941824e9`, 1794182400},
		{"tiny exponent", `1e-999999999`, 1},
		{"huge exponent", `1e999999999`, 0},
		{"maximum", `253402300799`, 253402300799},
		{"past maximum fraction", `253402300799.0000000001`, 0},
		{"leading zeros", `"0001794182400.0001"`, 1794182401},
		{"UTC date", `"2026-11-09T00:00:00Z"`, 1794182400},
		{"offset date", `"2026-11-09T02:00:00+02:00"`, 1794182400},
		{"fractional date", `"2026-11-09T00:00:00.000000001Z"`, 1794182401},
		{"sub-nanosecond date", `"2026-11-09T00:00:00.0000000001Z"`, 1794182401},
		{"zero fractional date", `"2026-11-09T00:00:00.0000000000Z"`, 1794182400},
		{"absent", ``, 0},
		{"null", `null`, 0},
		{"empty", `""`, 0},
		{"malformed date", `"not-a-date"`, 0},
		{"object", `{}`, 0},
		{"array", `[]`, 0},
		{"boolean", `true`, 0},
		{"negative", `-10`, 0},
		{"infinity", `"Infinity"`, 0},
		{"NaN", `"NaN"`, 0},
		{"overflow", `1e999`, 0},
		{"huge integer", `9223372036854775807`, 0},
		{"out of range date", `"0000-01-01T00:00:00Z"`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := `{}`
			if tc.raw != "" {
				wire = `{"earliest_refresh_at":` + tc.raw + `}`
			}
			var token tokenResponse
			if err := json.Unmarshal([]byte(wire), &token); err != nil {
				t.Fatalf("optional metadata rejected response: %v", err)
			}
			if int64(token.EarliestRefreshAt) != tc.want {
				t.Fatalf("timestamp=%d want=%d", token.EarliestRefreshAt, tc.want)
			}
			stored, err := json.Marshal(token)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip tokenResponse
			if err := json.Unmarshal(stored, &roundTrip); err != nil || roundTrip.EarliestRefreshAt != token.EarliestRefreshAt {
				t.Fatal("normalized timestamp failed durable round trip")
			}
		})
	}
	for zeros := 1; zeros <= 1500; zeros++ {
		for _, last := range []string{"0", "1"} {
			raw := `"1794182400.` + strings.Repeat("0", zeros) + last + `"`
			var value refreshTimestamp
			want := refreshTimestamp(1794182400)
			if last == "1" {
				want++
			}
			if err := json.Unmarshal([]byte(raw), &value); err != nil || value != want {
				t.Fatalf("decimal precision lost at %d fractional zeros: got %d want %d", zeros, value, want)
			}
		}
	}

	var previous refreshTimestamp = 42
	if err := json.Unmarshal([]byte(`"invalid"`), &previous); err != nil || previous != 0 {
		t.Fatal("invalid hint retained stale scheduling metadata")
	}
}

func TestRefreshMetadataDoesNotDiscardRotatedCredentials(t *testing.T) {
	for _, hint := range []any{"2026-11-09T00:00:00Z", "1794182400", "not-a-date", map[string]any{"unexpected": true}, nil} {
		t.Run("metadata", func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			key := signingKey(t)
			var refreshes, fetches int
			c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case tokenURL:
					refreshes++
					return response(map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "id_token": signed(t, key, claims("")), "token_type": "Bearer", "expires_in": 3600, "earliest_refresh_at": hint}), nil
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
			if err != nil || v.Pending != nil || v.RefreshToken != "rotated-refresh" || v.Scopes != Scope {
				t.Fatal("rotated credentials were not committed")
			}
		})
	}
}

func TestOptionalMetadataDoesNotRelaxCredentialParsing(t *testing.T) {
	for _, wire := range []string{
		`{"access_token":123,"earliest_refresh_at":"invalid"}`,
		`{"refresh_token":{},"earliest_refresh_at":null}`,
		`{"scope":[],"earliest_refresh_at":"invalid"}`,
		`{"expires_in":"3600","earliest_refresh_at":"invalid"}`,
		`{"earliest_refresh_at":`,
	} {
		var token tokenResponse
		if err := json.Unmarshal([]byte(wire), &token); err == nil {
			t.Fatal("invalid credential response accepted")
		}
	}
}
