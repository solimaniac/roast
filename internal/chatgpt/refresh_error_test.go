package chatgpt

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTransientOAuthFailurePreservesRefreshCredentials(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
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
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
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
		c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}, nil
		})
		if _, err := c.Token(context.Background()); !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "oauth_error") {
			t.Fatalf("malformed error retained a terminal category: %v", err)
		}
		v, err := c.load(c.selected)
		if err != nil || v.AccessToken != "access" || v.RefreshToken != "refresh" {
			t.Fatal("malformed error erased credentials")
		}
	}
}
