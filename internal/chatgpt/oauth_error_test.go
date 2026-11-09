package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

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
			c := testClient(t)
			c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				r := response(map[string]any{"error": tc.code, "error_description": "private-description-marker", "request_id": "private-body-marker"})
				r.StatusCode = http.StatusBadRequest
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
	// Validate at the output boundary too, not merely during network parsing.
	unsafe := &oauthError{code: "private-code-marker", status: 400, requestID: "private-header-marker"}
	if strings.Contains(unsafe.Error(), "private-") {
		t.Fatal("direct error construction bypassed sanitization")
	}
	for _, raw := range []string{`{"code":"invalid_grant","code":123}`, `{"code":"invalid_grant","code":null}`, `{"code":"invalid_grant"`} {
		if code := oauthResponseCategory(json.RawMessage(raw)); code != "oauth_error" {
			t.Fatal("invalid nested error retained a partially decoded terminal category")
		}
	}
}

func TestTransientOAuthFailurePreservesRefreshCredentials(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				r := response(map[string]string{"error": "invalid_grant"})
				r.StatusCode = status
				return r, nil
			})
			if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrNetwork) {
				t.Fatalf("transient response misclassified: %v", err)
			}
			v, err := c.load(c.selected)
			if err != nil || v.AccessToken != "access" || v.RefreshToken != "refresh" || v.Pending != nil {
				t.Fatal("transient response erased credentials")
			}
		})
	}
}

func TestBareUnauthorizedDoesNotEraseCredentials(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		r := response(map[string]string{"error": "unknown"})
		r.StatusCode = http.StatusUnauthorized
		return r, nil
	})
	if _, err := c.Token(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong classification: %v", err)
	}
	v, err := c.load(c.selected)
	if err != nil || v.RefreshToken != "refresh" {
		t.Fatal("HTTP status alone erased refresh token")
	}
}

func TestMalformedOAuthErrorDoesNotEraseCredentials(t *testing.T) {
	for _, raw := range []string{
		`{"error":{"code":"invalid_grant","code":42}}`,
		`{"error":{"code":"invalid_grant","code":null}}`,
		`{"error":"invalid_grant","extra":`,
	} {
		c := testClient(t)
		seed(t, c, time.Now().Add(-time.Minute))
		c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}, nil
		})
		var oe *oauthError
		if _, err := c.Token(context.Background()); !errors.As(err, &oe) || oe.code != "oauth_error" {
			t.Fatalf("malformed error retained a terminal category: %v", err)
		}
		v, err := c.load(c.selected)
		if err != nil || v.AccessToken != "access" || v.RefreshToken != "refresh" {
			t.Fatal("malformed error erased credentials")
		}
	}
}
