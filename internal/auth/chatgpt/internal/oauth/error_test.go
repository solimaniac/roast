package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOAuthErrorsExposeOnlySafeDetails(t *testing.T) {
	for _, tc := range []struct {
		name             string
		code             any
		headers          []string
		wantCode, wantID string
	}{
		{"known", "invalid_grant", []string{"req_12345678abcd"}, "invalid_grant", "req_12345678abcd"},
		{"nested", map[string]any{"code": "invalid_client", "message": "private-message-marker"}, nil, "invalid_client", ""},
		{"unknown", "private-code-marker", []string{"private-header-marker"}, "oauth_error", ""},
		{"unsafe header", "access_denied", []string{"req_12345678\nprivate-marker"}, "access_denied", ""},
		{"URL header", "access_denied", []string{"https://example.invalid/?private-marker"}, "access_denied", ""},
		{"too long", nil, []string{"req_" + strings.Repeat("x", 125)}, "oauth_error", ""},
		{"duplicate", "invalid_grant", []string{"req_12345678abcd", "req_abcdefgh1234"}, "invalid_grant", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient()
			c.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				body, err := json.Marshal(map[string]any{"error": tc.code, "error_description": "private-description-marker", "request_id": "private-body-marker"})
				if err != nil {
					return nil, err
				}
				r := &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
				for _, id := range tc.headers {
					r.Header.Add("X-Request-Id", id)
				}
				return r, nil
			})
			err := c.request(context.Background(), http.MethodPost, tokenURL, nil, "", nil)
			var oe *oauthError
			if !errors.As(err, &oe) || oe.code != tc.wantCode || oe.requestID != tc.wantID || !errors.Is(err, ErrAuth) {
				t.Fatalf("unexpected safe error: %v", err)
			}
			message := err.Error()
			if strings.Contains(message, "private-") || strings.Contains(message, "https://") || strings.ContainsAny(message, "\r\n") {
				t.Fatal("error exposed provider data")
			}
			if !strings.Contains(message, tc.wantCode) || !strings.Contains(message, "HTTP 400") {
				t.Fatal("safe category or status missing")
			}
			if (tc.wantID != "") != strings.Contains(message, "request ID") {
				t.Fatal("request ID presentation mismatch")
			}
		})
	}
}

func TestOAuthErrorConstructionSanitizesDetails(t *testing.T) {
	// Validate at the output boundary too, not merely during network parsing.
	unsafe := &oauthError{code: "private-code-marker", status: 400, requestID: "private-header-marker"}
	if strings.Contains(unsafe.Error(), "private-") {
		t.Fatal("direct error construction bypassed sanitization")
	}
}

func TestMalformedOAuthErrorDoesNotRetainTerminalCategory(t *testing.T) {
	for _, raw := range []string{`{"code":"invalid_grant","code":123}`, `{"code":"invalid_grant","code":null}`, `{"code":"invalid_grant"`} {
		if code := oauthResponseCategory(json.RawMessage(raw)); code != "oauth_error" {
			t.Fatalf("oauthResponseCategory(%s) = %q, want oauth_error", raw, code)
		}
	}
}

func TestRequestIDForError(t *testing.T) {
	for _, tt := range []struct {
		name string
		id   string
		want bool
	}{
		{"empty", "", false},
		{"11 bytes", "req_" + strings.Repeat("a", 7), false},
		{"12 bytes", "req_" + strings.Repeat("a", 8), true},
		{"128 bytes", "req_" + strings.Repeat("a", 124), true},
		{"129 bytes", "req_" + strings.Repeat("a", 125), false},
		{"ASCII letters digits and separators", "req_aZ09_-ab", true},
		{"wrong prefix", "REQ_12345678", false},
		{"unicode", "req_1234567é", false},
		{"space", "req_1234 5678", false},
		{"newline", "req_12345678\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := ""
			if tt.want {
				want = tt.id
			}
			if got := requestIDForError(tt.id); got != want {
				t.Errorf("requestIDForError(%q) = %q, want %q", tt.id, got, want)
			}
			message := (&oauthError{status: http.StatusBadRequest, code: "invalid_grant", requestID: tt.id}).Error()
			if strings.Contains(message, "; request ID ") != tt.want {
				t.Errorf("error presentation bypassed request ID validation: %q", message)
			}
		})
	}
}
