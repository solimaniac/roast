package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var ErrAuth = errors.New("ChatGPT authentication required")
var ErrNetwork = errors.New("ChatGPT service unavailable")

const (
	Issuer       = "https://auth.openai.com"
	APIBaseURL   = "https://api.openai.com/v1"
	Scope        = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	authorizeURL = Issuer + "/api/accounts/authorize"
	tokenURL     = Issuer + "/api/accounts/oauth/token"
	jwksURL      = Issuer + "/.well-known/jwks.json"
	discoveryURL = Issuer + "/.well-known/openid-configuration"
)

type Client struct {
	signInDiagnostic func(string)
	dir              string
	selected         string
	http             *http.Client
	now              func() time.Time
}

// New opens Roast's own registration store under the operating-system config directory.
func New() (*Client, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("direct ChatGPT authentication is currently supported only on Linux and macOS; Windows requires protected credential ACL support. Use another Roast backend on Windows")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	c := &Client{dir: filepath.Join(dir, "roast", "chatgpt"), http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: time.Now}
	i, e := c.loadIndex()
	if e != nil {
		return nil, e
	}
	c.selected = i.Active
	return c, nil
}

type tokenResponse struct {
	AccessToken       string `json:"access_token"`
	RefreshToken      string `json:"refresh_token"`
	IDToken           string `json:"id_token"`
	TokenType         string `json:"token_type"`
	ExpiresIn         int64  `json:"expires_in"`
	Scope             string `json:"scope"`
	EarliestRefreshAt int64  `json:"earliest_refresh_at"`
}
type oauthError struct {
	code   string
	status int
}

func (e *oauthError) Is(target error) bool {
	if e.status >= 500 || e.status == 429 {
		return target == ErrNetwork
	}
	return target == ErrAuth
}
func (e *oauthError) Error() string {
	return fmt.Sprintf("ChatGPT OAuth request failed (HTTP %d); sign in again if access was revoked", e.status)
}
func (c *Client) request(ctx context.Context, method, target string, form url.Values, bearer string, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r, e := http.NewRequestWithContext(ctx, method, target, body)
	if e != nil {
		return errors.New("cannot prepare ChatGPT request")
	}
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, e := c.http.Do(r)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: check connectivity and retry", ErrNetwork)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var v struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v)
		return &oauthError{v.Error, resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out) != nil {
		return errors.New("invalid ChatGPT response")
	}
	return nil
}
func hasScope(scope, s string) bool {
	for _, v := range strings.Fields(scope) {
		if v == s {
			return true
		}
	}
	return false
}
func (c *Client) apply(v *credential, t tokenResponse) error {
	if t.AccessToken == "" {
		if t.IDToken == "" {
			return errors.New("incomplete ChatGPT token response")
		}
	} else if !strings.EqualFold(t.TokenType, "Bearer") || t.ExpiresIn <= 0 || t.ExpiresIn > 86400 {
		return errors.New("incomplete ChatGPT token response")
	}
	if hasScope(t.Scope, "chatgpt.tokens.use.direct") && (t.AccessToken == "" || t.RefreshToken == "") {
		return errors.New("incomplete ChatGPT plan token response")
	}
	v.AccessToken = t.AccessToken
	v.RefreshToken = t.RefreshToken
	v.Scopes = t.Scope
	v.ExpiresAt = c.now().Add(time.Duration(t.ExpiresIn) * time.Second)
	v.EarliestRefreshAt = t.EarliestRefreshAt
	if t.IDToken != "" {
		v.IDToken = t.IDToken
	}
	return nil
}
func (c *Client) Token(ctx context.Context) (string, error) {
	if c.selected == "" {
		return "", fmt.Errorf("%w: no account selected; run roast chatgpt signin", ErrAuth)
	}
	var token string
	err := c.locked(ctx, func() error {
		if c.selected == "" {
			return errors.New("no ChatGPT account selected; run roast chatgpt signin")
		}
		v, e := c.load(c.selected)
		if e != nil {
			return e
		}
		if v.Pending != nil {
			if e = c.acceptPending(ctx, &v); e != nil {
				return e
			}
		}
		if v.AccessToken == "" {
			return errors.New("ChatGPT account is signed out; run roast chatgpt signin --account CLIENT_ID")
		}
		if !hasScope(v.Scopes, "chatgpt.tokens.use.direct") || !hasScope(v.Scopes, "resource.invoke") {
			return errors.New("ChatGPT plan usage scope is missing; sign in again")
		}
		if c.now().Before(v.ExpiresAt.Add(-time.Minute)) || c.now().Unix() < v.EarliestRefreshAt {
			if !c.now().Before(v.ExpiresAt) {
				return errors.New("ChatGPT token expired before refresh is permitted; retry after earliest_refresh_at")
			}
			token = v.AccessToken
			return nil
		}
		var t tokenResponse
		e = c.request(ctx, http.MethodPost, tokenURL, url.Values{"grant_type": {"refresh_token"}, "client_id": {v.ClientID}, "refresh_token": {v.RefreshToken}, "resource": {APIBaseURL}}, "", &t)
		if e != nil {
			var oe *oauthError
			if errors.As(e, &oe) {
				switch oe.code {
				case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
					v.AccessToken = ""
					v.RefreshToken = ""
					v.IDToken = ""
					if se := c.save(v); se != nil {
						return se
					}
				}
			}
			return e
		}
		// Rotation has consumed the old refresh token. Persist the response before
		// any fallible JWKS fetch so the next call resumes validation, not refresh.
		v.Pending = &pendingRefresh{Response: t, ReceivedAt: c.now()}
		if e = c.save(v); e != nil {
			return e
		}
		if e = c.acceptPending(ctx, &v); e != nil {
			return e
		}

		if !hasScope(v.Scopes, "chatgpt.tokens.use.direct") || !hasScope(v.Scopes, "resource.invoke") {
			return errors.New("ChatGPT plan usage permission was removed; enable it explicitly before inference")
		}
		// Validation may outlast a short-lived access token. Keep the rotated
		// credentials for the next attempt, but never send an expired token to
		// inference or refresh repeatedly within this call.
		if !c.now().Before(v.ExpiresAt) {
			return fmt.Errorf("%w: refreshed token expired during validation; retry", ErrNetwork)
		}
		token = v.AccessToken
		return nil
	})
	if err != nil && !errors.Is(err, ErrNetwork) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("%w: %w", ErrAuth, err)
	}
	return token, err
}

func (c *Client) acceptPending(ctx context.Context, v *credential) error {
	p := v.Pending
	if p == nil {
		return nil
	}
	if p.ReceivedAt.IsZero() || p.ReceivedAt.Unix() <= 0 || p.ReceivedAt.After(c.now().Add(5*time.Second)) {
		return errors.New("invalid pending ChatGPT token receipt time")
	}
	// The response was durably received from the fixed token endpoint before
	// JWKS validation. Validate temporal identity claims at that trusted receipt
	// time, allowing recovery after an outage outlasts the ID token. Access-token
	// expiry below remains anchored to receipt and is checked against real time.
	validator := *c
	validator.now = func() time.Time { return p.ReceivedAt }
	if p.Response.IDToken != "" {
		identity, err := validator.validateID(ctx, p.Response.IDToken, v.ClientID, "")
		if err != nil {
			return err
		}
		if identity.Subject != v.Subject {
			return errors.New("refreshed ChatGPT identity changed")
		}
	}
	if err := c.apply(v, p.Response); err != nil {
		return err
	}
	v.ExpiresAt = p.ReceivedAt.Add(time.Duration(p.Response.ExpiresIn) * time.Second)
	v.Pending = nil
	return c.save(*v)
}

type Model struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Visibility  string `json:"visibility"`
}

func (c *Client) Models(ctx context.Context) ([]Model, error) {
	token, e := c.Token(ctx)
	if e != nil {
		return nil, e
	}
	var v struct {
		Models []Model `json:"models"`
	}
	e = c.request(ctx, http.MethodGet, APIBaseURL+"/models", nil, token, &v)
	return v.Models, e
}
func (c *Client) SignOut(ctx context.Context, id string) error {
	return c.locked(ctx, func() error {
		i, e := c.loadIndex()
		if e != nil {
			return e
		}
		if id == "" {
			id = i.Active
		}
		if id == "" {
			return errors.New("no active ChatGPT account")
		}
		v, e := c.load(id)
		if e != nil {
			return e
		}
		var remoteErr error
		if v.Pending != nil && v.Pending.Response.RefreshToken != "" {
			v.RefreshToken = v.Pending.Response.RefreshToken
		}
		if v.RefreshToken != "" {
			var d struct {
				Issuer     string `json:"issuer"`
				Revocation string `json:"revocation_endpoint"`
			}
			remoteErr = c.request(ctx, http.MethodGet, discoveryURL, nil, "", &d)
			if remoteErr == nil {
				u, e := url.Parse(d.Revocation)
				if e != nil || u.Scheme != "https" || u.Host != "auth.openai.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || d.Issuer != Issuer {
					remoteErr = errors.New("invalid OpenAI revocation discovery")
				} else {
					for attempt := 0; attempt < 3; attempt++ {
						remoteErr = c.request(ctx, http.MethodPost, d.Revocation, url.Values{"token": {v.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {id}}, "", nil)
						if remoteErr == nil || !errors.Is(remoteErr, ErrNetwork) || attempt == 2 {
							break
						}
						select {
						case <-ctx.Done():
							remoteErr = ctx.Err()
						case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
						}
						if ctx.Err() != nil {
							break
						}
					}
				}
			}
		}
		v.AccessToken = ""
		v.RefreshToken = ""
		v.IDToken = ""
		v.Scopes = ""
		v.Pending = nil
		if e = c.save(v); e != nil {
			return e
		}
		if i.Active == id {
			if e = i.setActive(""); e != nil {
				return e
			}
			if e = c.saveIndex(i); e != nil {
				return e
			}
		}
		if remoteErr != nil {
			return errors.New("signed out locally; remote revocation was not confirmed. Disconnect Roast in ChatGPT Settings")
		}
		return nil
	})
}
