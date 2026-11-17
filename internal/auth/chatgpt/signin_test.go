package chatgpt

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

func TestSignInCallbackAndPKCE(t *testing.T) {
	c := testClient(t)
	k := signingKey(t)
	var nonce, redirect, challenge string
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case jwksURL:
			return jsonResponse(http.StatusOK, jwks(k))
		case tokenURL:
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("client_id") != "oaiapp_test" || r.Form.Get("redirect_uri") != redirect || r.Form.Get("resource") != oauth.APIBaseURL {
				t.Error("incorrect exchange")
			}
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				t.Error("PKCE mismatch")
			}
			return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "access", RefreshToken: "refresh", IDToken: signed(t, k, claims(nonce)), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600})
		default:
			t.Fatal("unexpected destination")
			return nil, nil
		}
	})
	e := c.SignIn(context.Background(), "", func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("client_id") != "dynamic_agent_client" || q.Get("scope") != oauth.Scope || q.Get("agent_name_hint") != "Roast" || q.Get("ext_agent_host_id") == "" {
			t.Error("incorrect authorization")
		}
		redirect = q.Get("redirect_uri")
		nonce = q.Get("nonce")
		challenge = q.Get("code_challenge")
		if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
			t.Error("not loopback")
		}
		callback, err := url.Parse(redirect)
		if err != nil {
			return err
		}
		bad := url.Values{"state": {"wrong"}, "code": {"bad"}}
		callback.RawQuery = bad.Encode()
		res, e := http.Get(callback.String())
		if e != nil {
			return e
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Error("accepted bad state")
		}
		callback.RawQuery = url.Values{"state": {q.Get("state")}, "code": {"code"}, "client_id": {"oaiapp_test"}}.Encode()
		res, e = http.Get(callback.String())
		if e == nil {
			res.Body.Close()
		}
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	i, e := c.loadIndex()
	if e != nil || i.Active != "oaiapp_test" {
		t.Fatal("registration not activated")
	}
	v, e := c.load(i.Active)
	if e != nil || v.Subject != "subject" || v.IDToken == "" {
		t.Fatal("identity not stored")
	}
}

func TestIdentityOnlyAndExplicitConsent(t *testing.T) {
	c := testClient(t)
	key := signingKey(t)
	var nonce string
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			return jsonResponse(http.StatusOK, jwks(key))
		}
		if r.URL.String() != tokenURL {
			t.Fatal("wrong target")
		}
		return jsonResponse(http.StatusOK, oauth.TokenResponse{IDToken: signed(t, key, claims(nonce)), Scope: scopeValue("openid profile email")})
	})
	run := func(enable bool) {
		t.Helper()
		show := func(raw string) error {
			u, err := url.Parse(raw)
			if err != nil {
				return err
			}
			q := u.Query()
			nonce = q.Get("nonce")
			if (q.Get("prompt") == "consent") != enable {
				t.Error("forced consent on wrong action")
			}
			if q.Get("scope") != oauth.Scope {
				t.Error("incomplete requested scopes")
			}
			if enable && (q.Get("client_id") != "oaiapp_test" || q.Get("agent_name_hint") != "") {
				t.Error("reauthorization recreated client")
			}
			callback, err := url.Parse(q.Get("redirect_uri"))
			if err != nil {
				return err
			}
			callback.RawQuery = url.Values{"code": {"code"}, "state": {q.Get("state")}, "client_id": {"oaiapp_test"}}.Encode()
			r, e := http.Get(callback.String())
			if e == nil {
				r.Body.Close()
			}
			return e
		}
		var err error
		if enable {
			err = c.EnablePlan(context.Background(), "oaiapp_test", show)
		} else {
			err = c.SignIn(context.Background(), "", show)
		}
		if err == nil || !strings.Contains(err.Error(), "plan usage is disabled") {
			t.Fatalf("missing disabled explanation: %v", err)
		}
	}
	run(false)
	v, err := c.load("oaiapp_test")
	if err != nil || v.IDToken == "" || v.Subject != "subject" {
		t.Fatal("identity-only sign-in was lost")
	}
	accounts, err := c.Status(context.Background())
	if err != nil || len(accounts) != 1 || !accounts[0].SignedIn || accounts[0].PlanEnabled {
		t.Fatal("identity-only status incorrect")
	}
	c.selected = "oaiapp_test"
	if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrAuth) {
		t.Fatal("identity-only token allowed inference")
	}
	run(true)
}

func TestFailedInitialExchangePreservesPendingRegistration(t *testing.T) {
	c := testClient(t)
	key := signingKey(t)
	if err := c.saveIndex(index{HostID: "stable-host", Active: "oaiapp_existing", Clients: []string{"oaiapp_existing"}}); err != nil {
		t.Fatal(err)
	}
	existing := credential{Account: Account{ClientID: "oaiapp_existing", Subject: "existing-subject"}, AccessToken: "existing-access", Scopes: oauth.Scope}
	if err := c.save(existing); err != nil {
		t.Fatal(err)
	}
	var nonce string
	exchanges := 0
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			return jsonResponse(http.StatusOK, jwks(key))
		}
		if r.URL.String() != tokenURL {
			t.Fatal("unexpected OAuth destination")
		}
		exchanges++
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		if r.Form.Get("client_id") != "oaiapp_test" {
			t.Error("exchange did not retain issued client ID")
		}
		if exchanges == 1 {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		}
		return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "access", RefreshToken: "refresh", IDToken: signed(t, key, claims(nonce)), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600})
	})
	show := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		nonce = q.Get("nonce")
		expected := "dynamic_agent_client"
		if exchanges > 0 {
			expected = "oaiapp_test"
			if q.Get("agent_name_hint") != "" {
				t.Error("retry used dynamic registration metadata")
			}
		}
		if q.Get("client_id") != expected || q.Get("ext_agent_host_id") != "stable-host" {
			t.Error("retry changed registration or host")
		}
		callback, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			return err
		}
		callback.RawQuery = url.Values{"code": {"code"}, "state": {q.Get("state")}, "client_id": {"oaiapp_test"}}.Encode()
		res, err := http.Get(callback.String())
		if err == nil {
			res.Body.Close()
		}
		return err
	}
	if err := c.SignIn(context.Background(), "", show); err == nil || !strings.Contains(err.Error(), "--account oaiapp_test") {
		t.Fatalf("failed exchange must explain retry: %v", err)
	}
	pending, err := c.load("oaiapp_test")
	if err != nil || !pending.PendingRegistration || pending.Subject != "" || pending.Email != "" || pending.AccessToken != "" || pending.RefreshToken != "" || pending.IDToken != "" {
		t.Fatal("pending callback was lost or treated as verified identity")
	}
	i, err := c.loadIndex()
	if err != nil || i.Active != "oaiapp_existing" || len(i.Clients) != 2 {
		t.Fatal("failed registration displaced active account")
	}
	if err := c.Select(context.Background(), "oaiapp_test"); err == nil {
		t.Fatal("pending registration became selectable")
	}
	if err := c.SignIn(context.Background(), "oaiapp_test", show); err != nil {
		t.Fatal(err)
	}
	verified, err := c.load("oaiapp_test")
	if err != nil || verified.PendingRegistration || verified.Subject != "subject" || verified.AccessToken == "" {
		t.Fatal("pending registration did not complete identity validation")
	}
	i, err = c.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if i.Active != "oaiapp_test" || i.HostID != "stable-host" || exchanges != 2 {
		t.Fatal("retry did not retain registration and host")
	}
}

func TestIssuedClientIDOpaqueCallback(t *testing.T) {
	for _, tt := range []struct {
		id    string
		valid bool
	}{{"issued-client-v2", true}, {"oaiapp_example", true}, {"dynamic_agent_client", false}, {"", false}, {"has space", false}, {"line\nbreak", false}, {strings.Repeat("a", 257), false}} {
		t.Run(tt.id, func(t *testing.T) {
			c := testClient(t)
			exchanged := false
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != tokenURL {
					t.Fatal("unexpected destination")
				}
				exchanged = true
				if err := r.ParseForm(); err != nil {
					return nil, err
				}
				if r.Form.Get("client_id") != tt.id {
					t.Error("issued ID was modified")
				}
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			})
			err := c.SignIn(context.Background(), "", func(raw string) error {
				u, err := url.Parse(raw)
				if err != nil {
					return err
				}
				q := u.Query()
				callback, err := url.Parse(q.Get("redirect_uri"))
				if err != nil {
					return err
				}
				callback.RawQuery = url.Values{"state": {q.Get("state")}, "code": {"code"}, "client_id": {tt.id}}.Encode()
				res, e := http.Get(callback.String())
				if e == nil {
					res.Body.Close()
				}
				return e
			})
			if err == nil {
				t.Fatal("expected rejected callback or mocked exchange failure")
			}
			if exchanged != tt.valid {
				t.Fatalf("exchange=%t, valid=%t", exchanged, tt.valid)
			}
			if tt.valid {
				pending, e := c.load(tt.id)
				if e != nil || !pending.PendingRegistration || pending.ClientID != tt.id {
					t.Fatal("opaque issued ID not retained")
				}
			}
		})
	}
}
