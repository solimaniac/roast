package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
	HTTP *http.Client
}

func NewClient() Client {
	return Client{HTTP: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type TokenResponse struct {
	AccessToken       string           `json:"access_token"`
	RefreshToken      string           `json:"refresh_token"`
	IDToken           string           `json:"id_token"`
	TokenType         string           `json:"token_type"`
	ExpiresIn         int64            `json:"expires_in"`
	Scope             *string          `json:"scope,omitempty"`
	EarliestRefreshAt RefreshTimestamp `json:"earliest_refresh_at"`
}

func (c Client) request(ctx context.Context, method, target string, form url.Values, bearer string, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return errors.New("cannot prepare ChatGPT request")
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: check connectivity and retry", ErrNetwork)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var v struct {
			Error json.RawMessage `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v) != nil {
			v.Error = nil
		}
		requestID := ""
		if values := resp.Header.Values("X-Request-Id"); len(values) == 1 {
			requestID = requestIDForError(values[0])
		}
		return &oauthError{code: oauthResponseCategory(v.Error), status: resp.StatusCode, requestID: requestID}
	}
	if out == nil {
		return nil
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out) != nil {
		return errors.New("invalid ChatGPT response")
	}
	return nil
}

func (c Client) Exchange(ctx context.Context, clientID, code, verifier, redirect string) (TokenResponse, error) {
	return c.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirect},
		"resource":      {APIBaseURL},
	})
}

func (c Client) Refresh(ctx context.Context, clientID, refreshToken string) (TokenResponse, error) {
	return c.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
		"resource":      {APIBaseURL},
	})
}

func (c Client) token(ctx context.Context, form url.Values) (TokenResponse, error) {
	var token TokenResponse
	err := c.request(ctx, http.MethodPost, tokenURL, form, "", &token)
	return token, err
}

func (c Client) Revoke(ctx context.Context, clientID, refreshToken string) error {
	var discovery struct {
		Issuer     string `json:"issuer"`
		Revocation string `json:"revocation_endpoint"`
	}
	if err := c.request(ctx, http.MethodGet, discoveryURL, nil, "", &discovery); err != nil {
		return err
	}
	endpoint, err := url.Parse(discovery.Revocation)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host != "auth.openai.com" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || discovery.Issuer != Issuer {
		return errors.New("invalid OpenAI revocation discovery")
	}
	form := url.Values{
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {clientID},
	}
	for attempt := 1; ; attempt++ {
		err := c.request(ctx, http.MethodPost, discovery.Revocation, form, "", nil)
		if !errors.Is(err, ErrNetwork) || attempt == 3 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

type Model struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Visibility  string `json:"visibility"`
}

func (c Client) Models(ctx context.Context, token string) ([]Model, error) {
	var response struct {
		Models []Model `json:"models"`
	}
	err := c.request(ctx, http.MethodGet, APIBaseURL+"/models", nil, token, &response)
	return response.Models, err
}
