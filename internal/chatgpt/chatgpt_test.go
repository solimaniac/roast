package chatgpt

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(b)))}
}
func testClient(t *testing.T) *Client {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("direct ChatGPT auth requires Windows protected ACL implementation")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return &Client{dir: dir, now: time.Now, http: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected network request")
		return nil, errors.New("unexpected")
	})}}
}
func seed(t *testing.T, c *Client, exp time.Time) {
	t.Helper()
	c.selected = "oaiapp_test"
	if e := c.saveIndex(index{HostID: "host", Active: c.selected, Clients: []string{c.selected}}); e != nil {
		t.Fatal(e)
	}
	if e := c.save(credential{Account: Account{ClientID: c.selected, Subject: "subject", ExpiresAt: exp}, AccessToken: "access", RefreshToken: "refresh", Scopes: Scope}); e != nil {
		t.Fatal(e)
	}
}
func signingKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	return k
}
func signed(t *testing.T, k *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test"})
	p, _ := json.Marshal(claims)
	input := enc(h) + "." + enc(p)
	sum := sha256.Sum256([]byte(input))
	sig, e := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if e != nil {
		t.Fatal(e)
	}
	return input + "." + enc(sig)
}
func jwks(k *rsa.PrivateKey) any {
	return map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes())}}}
}
func claims(nonce string) map[string]any {
	return map[string]any{"iss": Issuer, "sub": "subject", "aud": "oaiapp_test", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce}
}

func TestJWTValidation(t *testing.T) {
	k := signingKey(t)
	c := testClient(t)
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
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
			_, e := c.validateID(context.Background(), signed(t, k, v), "oaiapp_test", tt.nonce)
			if (e == nil) != tt.ok {
				t.Fatalf("error=%v want valid=%t", e, tt.ok)
			}
		})
	}
	other := signingKey(t)
	if _, e := c.validateID(context.Background(), signed(t, other, claims("nonce")), "oaiapp_test", "nonce"); e == nil {
		t.Fatal("accepted wrong signature")
	}
	if _, e := c.validateID(context.Background(), "eyJhbGciOiJub25lIn0.e30.", "oaiapp_test", "nonce"); e == nil {
		t.Fatal("accepted unsigned token")
	}
}
func TestRefreshSerializedAndRotated(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	var calls atomic.Int32
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.String() != tokenURL {
			t.Error("wrong token target")
		}
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "refresh" || r.Form.Get("client_id") != c.selected || r.Form.Get("scope") != "" {
			t.Error("bad refresh form")
		}
		return response(tokenResponse{AccessToken: "next-access", RefreshToken: "next-refresh", TokenType: "Bearer", ExpiresIn: 3600, Scope: Scope}), nil
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			token, e := c.Token(context.Background())
			if e != nil || token != "next-access" {
				t.Errorf("token refresh failed: %v", e)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh called %d times", calls.Load())
	}
	v, e := c.load(c.selected)
	if e != nil || v.RefreshToken != "next-refresh" {
		t.Fatal("rotation not saved")
	}
	st, _ := os.Stat(c.path(c.selected))
	if st.Mode().Perm() != 0600 {
		t.Fatal("credential file is not 0600")
	}
}
func TestEarliestRefresh(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(20*time.Second))
	v, _ := c.load(c.selected)
	v.EarliestRefreshAt = time.Now().Add(30 * time.Second).Unix()
	_ = c.save(v)
	if _, e := c.Token(context.Background()); e != nil {
		t.Fatal(e)
	}
	v.ExpiresAt = time.Now().Add(-time.Second)
	_ = c.save(v)
	if _, e := c.Token(context.Background()); !errors.Is(e, ErrAuth) {
		t.Fatalf("want auth failure: %v", e)
	}
}
func TestTerminalRefreshClearsOnlyTokens(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		r := response(map[string]string{"error": "invalid_grant", "error_description": "secret should not appear"})
		r.StatusCode = 400
		return r, nil
	})
	_, e := c.Token(context.Background())
	if !errors.Is(e, ErrAuth) || strings.Contains(e.Error(), "secret") {
		t.Fatal("unsafe or unclassified error")
	}
	v, _ := c.load(c.selected)
	if v.AccessToken != "" || v.RefreshToken != "" || v.Subject != "subject" {
		t.Fatal("did not preserve registration while clearing tokens")
	}
}
func TestNetworkErrorDoesNotClearTokens(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret in transport error") })
	_, e := c.Token(context.Background())
	if !errors.Is(e, ErrNetwork) || strings.Contains(e.Error(), "secret") {
		t.Fatal("unsafe or unclassified error")
	}
	v, _ := c.load(c.selected)
	if v.RefreshToken == "" {
		t.Fatal("network failure erased refresh token")
	}
}
func TestPrivateFilesAndLockCancellation(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	if e := os.Chmod(c.path(c.selected), 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Token(context.Background()); e == nil {
		t.Fatal("accepted public credential")
	}
	_ = os.Chmod(c.path(c.selected), 0600)
	lock, err := os.OpenFile(filepath.Join(c.dir, "session.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if ok, err := tryLock(lock); !ok || err != nil {
		t.Fatal("cannot acquire lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, e := c.Token(ctx); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("lock ignored cancellation: %v", e)
	}
}
func TestSignInCallbackAndPKCE(t *testing.T) {
	c := testClient(t)
	k := signingKey(t)
	var nonce, redirect, challenge string
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case jwksURL:
			return response(jwks(k)), nil
		case tokenURL:
			_ = r.ParseForm()
			if r.Form.Get("client_id") != "oaiapp_test" || r.Form.Get("redirect_uri") != redirect || r.Form.Get("resource") != APIBaseURL {
				t.Error("incorrect exchange")
			}
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				t.Error("PKCE mismatch")
			}
			return response(tokenResponse{AccessToken: "access", RefreshToken: "refresh", IDToken: signed(t, k, claims(nonce)), TokenType: "Bearer", Scope: Scope, ExpiresIn: 3600}), nil
		default:
			t.Fatal("unexpected destination")
			return nil, nil
		}
	})
	e := c.SignIn(context.Background(), "", func(raw string) error {
		u, _ := url.Parse(raw)
		q := u.Query()
		if q.Get("client_id") != "dynamic_agent_client" || q.Get("scope") != Scope || q.Get("agent_name_hint") != "Roast" || q.Get("ext_agent_host_id") == "" {
			t.Error("incorrect authorization")
		}
		redirect = q.Get("redirect_uri")
		nonce = q.Get("nonce")
		challenge = q.Get("code_challenge")
		if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
			t.Error("not loopback")
		}
		callback, _ := url.Parse(redirect)
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
func TestSignOutKeepsRegistrationOnNetworkFailure(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })
	e := c.SignOut(context.Background(), "")
	if e == nil || !strings.Contains(e.Error(), "remote revocation was not confirmed") {
		t.Fatal("must explain unconfirmed revocation")
	}
	v, _ := c.load(c.selected)
	if v.AccessToken != "" || v.RefreshToken != "" || v.Subject != "subject" {
		t.Fatal("wrong signout state")
	}
}

func TestSelectedRegistrationPinnedAndIndependent(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	other := credential{Account: Account{ClientID: "oaiapp_other", Subject: "another", ExpiresAt: time.Now().Add(time.Hour)}, AccessToken: "other-access", RefreshToken: "other-refresh", Scopes: Scope}
	if err := c.save(other); err != nil {
		t.Fatal(err)
	}
	i, _ := c.loadIndex()
	i.Clients = append(i.Clients, other.ClientID)
	_ = c.saveIndex(i)
	if err := c.Select(context.Background(), other.ClientID); err != nil {
		t.Fatal(err)
	}
	token, err := c.Token(context.Background())
	if err != nil || token != "access" {
		t.Fatal("account switch changed in-flight client")
	}
	accounts, err := c.Status(context.Background())
	if err != nil || len(accounts) != 2 || !accounts[1].Active || !accounts[1].PlanEnabled {
		t.Fatal("status did not reflect selected account")
	}
}
func TestModelsCatalog(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != APIBaseURL+"/models" || r.Header.Get("Authorization") != "Bearer access" {
			t.Error("incorrect catalog request")
		}
		return response(map[string]any{"models": []Model{{Slug: "model", DisplayName: "Example model", Visibility: "list"}}}), nil
	})
	models, err := c.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].Slug != "model" {
		t.Fatal("catalog not parsed")
	}
}
func TestNewDoesNotCreateCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	c, err := New()
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "ACL") {
			t.Fatal("missing Windows safety error")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Token(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatal("missing login not classified")
	}
	if _, err = os.Stat(c.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only action created credential directory")
	}
}

func TestRefreshPendingSurvivesJWKSFailure(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	key := signingKey(t)
	var refreshes, fetches int
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case tokenURL:
			refreshes++
			return response(tokenResponse{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", IDToken: signed(t, key, claims("")), TokenType: "Bearer", Scope: Scope, ExpiresIn: 3600}), nil
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
		t.Fatalf("pending tokens must not escape: %v", err)
	}
	saved, err := c.load(c.selected)
	if err != nil || saved.Pending == nil || saved.Pending.Response.RefreshToken != "rotated-refresh" || saved.AccessToken != "access" {
		t.Fatal("rotation not durably staged")
	}
	resumed := *c
	token, err := resumed.Token(context.Background())
	if err != nil || token != "rotated-access" || refreshes != 1 {
		t.Fatalf("did not resume validation: refreshes=%d err=%v", refreshes, err)
	}
	saved, _ = c.load(c.selected)
	if saved.Pending != nil || saved.RefreshToken != "rotated-refresh" {
		t.Fatal("pending token not committed")
	}
}
func TestPendingWrongIdentityNeverExposed(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	key := signingKey(t)
	v, _ := c.load(c.selected)
	cl := claims("")
	cl["sub"] = "different"
	v.Pending = &pendingRefresh{Response: tokenResponse{AccessToken: "wrong-account", RefreshToken: "new-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", Scope: Scope, ExpiresIn: 3600}, ReceivedAt: time.Now()}
	_ = c.save(v)
	c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return response(jwks(key)), nil })
	if token, err := c.Token(context.Background()); err == nil || token != "" {
		t.Fatal("unvalidated identity escaped")
	}
}
func TestLockReleaseAndStablePreparedHost(t *testing.T) {
	c := testClient(t)
	id, dir, err := c.PrepareHost(context.Background())
	if err != nil || id == "" || dir != c.dir {
		t.Fatal("host not prepared")
	}
	second, _, err := c.PrepareHost(context.Background())
	if err != nil || id != second {
		t.Fatal("host identity changed")
	}
	i, _ := c.loadIndex()
	if len(i.Clients) != 0 || i.Active != "" {
		t.Fatal("prepare-host created registration")
	}
	f, err := os.OpenFile(filepath.Join(c.dir, "session.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := tryLock(f)
	if !ok || err != nil {
		t.Fatal("cannot obtain released lock")
	}
	_ = f.Close()
	if err = c.locked(context.Background(), func() error { return nil }); err != nil {
		t.Fatal("closing file did not release lock")
	}
}
func TestIdentityOnlyAndExplicitConsent(t *testing.T) {
	c := testClient(t)
	key := signingKey(t)
	var nonce string
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			return response(jwks(key)), nil
		}
		if r.URL.String() != tokenURL {
			t.Fatal("wrong target")
		}
		return response(tokenResponse{IDToken: signed(t, key, claims(nonce)), Scope: "openid profile email"}), nil
	})
	run := func(enable bool) {
		t.Helper()
		show := func(raw string) error {
			u, _ := url.Parse(raw)
			q := u.Query()
			nonce = q.Get("nonce")
			if (q.Get("prompt") == "consent") != enable {
				t.Error("forced consent on wrong action")
			}
			if q.Get("scope") != Scope {
				t.Error("incomplete requested scopes")
			}
			if enable && (q.Get("client_id") != "oaiapp_test" || q.Get("agent_name_hint") != "") {
				t.Error("reauthorization recreated client")
			}
			callback, _ := url.Parse(q.Get("redirect_uri"))
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
	existing := credential{Account: Account{ClientID: "oaiapp_existing", Subject: "existing-subject"}, AccessToken: "existing-access", Scopes: Scope}
	if err := c.save(existing); err != nil {
		t.Fatal(err)
	}
	var nonce string
	exchanges := 0
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			return response(jwks(key)), nil
		}
		if r.URL.String() != tokenURL {
			t.Fatal("unexpected OAuth destination")
		}
		exchanges++
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "oaiapp_test" {
			t.Error("exchange did not retain issued client ID")
		}
		if exchanges == 1 {
			res := response(map[string]string{"error": "invalid_grant"})
			res.StatusCode = http.StatusBadRequest
			return res, nil
		}
		return response(tokenResponse{AccessToken: "access", RefreshToken: "refresh", IDToken: signed(t, key, claims(nonce)), TokenType: "Bearer", Scope: Scope, ExpiresIn: 3600}), nil
	})
	show := func(raw string) error {
		u, _ := url.Parse(raw)
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
		callback, _ := url.Parse(q.Get("redirect_uri"))
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
	st, err := os.Stat(c.path("oaiapp_test"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("pending record is not owner-only")
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
	i, _ = c.loadIndex()
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
			c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != tokenURL {
					t.Fatal("unexpected destination")
				}
				exchanged = true
				_ = r.ParseForm()
				if r.Form.Get("client_id") != tt.id {
					t.Error("issued ID was modified")
				}
				res := response(map[string]string{"error": "invalid_grant"})
				res.StatusCode = http.StatusBadRequest
				return res, nil
			})
			err := c.SignIn(context.Background(), "", func(raw string) error {
				u, _ := url.Parse(raw)
				q := u.Query()
				callback, _ := url.Parse(q.Get("redirect_uri"))
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

func TestPendingRefreshDelayedPastIDExpiry(t *testing.T) {
	for _, tt := range []struct {
		name        string
		delay       time.Duration
		wantRefresh bool
	}{{"access still valid", 20 * time.Minute, false}, {"access expired", 2 * time.Hour, true}} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t)
			received := time.Now().UTC().Truncate(time.Second)
			now := received.Add(tt.delay)
			c.now = func() time.Time { return now }
			seed(t, c, received.Add(-time.Minute))
			key := signingKey(t)
			cl := claims("")
			cl["iat"] = received.Unix()
			cl["exp"] = received.Add(10 * time.Minute).Unix()
			v, _ := c.load(c.selected)
			v.Pending = &pendingRefresh{Response: tokenResponse{AccessToken: "pending-access", RefreshToken: "pending-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", Scope: Scope, ExpiresIn: 3600}, ReceivedAt: received}
			if err := c.save(v); err != nil {
				t.Fatal(err)
			}
			refreshes := 0
			c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case jwksURL:
					return response(jwks(key)), nil
				case tokenURL:
					refreshes++
					_ = r.ParseForm()
					if r.Form.Get("refresh_token") != "pending-refresh" {
						t.Error("reused consumed refresh token")
					}
					return response(tokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", TokenType: "Bearer", Scope: Scope, ExpiresIn: 3600}), nil
				default:
					t.Fatal("unexpected request")
					return nil, nil
				}
			})
			token, err := c.Token(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			saved, err := c.load(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantRefresh {
				if refreshes != 1 || token != "fresh-access" || saved.RefreshToken != "fresh-refresh" {
					t.Fatal("expired access was returned instead of refreshing replacement")
				}
			} else {
				if refreshes != 0 || token != "pending-access" || !saved.ExpiresAt.Equal(received.Add(time.Hour)) {
					t.Fatal("pending recovery extended actual access expiry")
				}
			}
			if saved.Pending != nil {
				t.Fatal("pending response was not committed")
			}
		})
	}
}
func TestPendingRefreshRejectsInvalidReceiptOrExpiredIdentity(t *testing.T) {
	for _, name := range []string{"zero receipt", "pre-epoch receipt", "future receipt", "expired at receipt"} {
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			now := time.Now().UTC().Truncate(time.Second)
			c.now = func() time.Time { return now }
			seed(t, c, now.Add(-time.Minute))
			received := now.Add(-time.Hour)
			key := signingKey(t)
			cl := claims("")
			cl["iat"] = received.Add(-time.Minute).Unix()
			cl["exp"] = received.Add(time.Minute).Unix()
			switch name {
			case "zero receipt":
				received = time.Time{}
			case "pre-epoch receipt":
				received = time.Unix(-1, 0)
			case "future receipt":
				received = now.Add(time.Hour)
			case "expired at receipt":
				cl["exp"] = received.Unix()
			}
			v, _ := c.load(c.selected)
			v.Pending = &pendingRefresh{Response: tokenResponse{AccessToken: "pending-access", RefreshToken: "pending-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", Scope: Scope, ExpiresIn: 3600}, ReceivedAt: received}
			if err := c.save(v); err != nil {
				t.Fatal(err)
			}
			c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != jwksURL {
					t.Fatal("invalid pending record caused refresh")
				}
				return response(jwks(key)), nil
			})
			if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrAuth) {
				t.Fatalf("invalid receipt/identity accepted: %v", err)
			}
			saved, _ := c.load(c.selected)
			if saved.Pending == nil || saved.AccessToken != "access" {
				t.Fatal("invalid pending credentials became active")
			}
		})
	}
}

func TestRefreshRechecksExpiryAfterValidation(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Second, 2 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			c := testClient(t)
			received := time.Now().UTC().Truncate(time.Second)
			now := received
			c.now = func() time.Time { return now }
			seed(t, c, received.Add(-time.Minute))
			key := signingKey(t)
			cl := claims("")
			cl["iat"] = received.Unix()
			cl["exp"] = received.Add(time.Hour).Unix()
			refreshes := 0
			c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case tokenURL:
					refreshes++
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					want := "refresh"
					if refreshes > 1 {
						want = "rotated-refresh"
					}
					if r.Form.Get("refresh_token") != want {
						t.Fatal("reused consumed refresh token")
					}
					if refreshes == 1 {
						return response(tokenResponse{AccessToken: "short-access", RefreshToken: "rotated-refresh", IDToken: signed(t, key, cl), TokenType: "Bearer", ExpiresIn: 1, Scope: Scope}), nil
					}
					return response(tokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", TokenType: "Bearer", ExpiresIn: 3600, Scope: Scope}), nil
				case jwksURL:
					now = received.Add(delay)
					return response(jwks(key)), nil
				default:
					t.Fatal("unexpected request")
					return nil, nil
				}
			})
			token, err := c.Token(context.Background())
			if delay == 0 {
				if err != nil || token != "short-access" {
					t.Fatalf("valid token rejected: %v", err)
				}
				return
			}
			if token != "" || !errors.Is(err, ErrNetwork) || errors.Is(err, ErrAuth) {
				t.Fatalf("expired refresh must return a retryable error and no token: %v", err)
			}
			if refreshes != 1 {
				t.Fatal("retried refresh within the same call")
			}
			saved, err := c.load(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Pending != nil || saved.RefreshToken != "rotated-refresh" || !saved.ExpiresAt.Equal(received.Add(time.Second)) {
				t.Fatal("rotated credentials were not preserved with their original expiry")
			}
			token, err = c.Token(context.Background())
			if err != nil || token != "fresh-access" || refreshes != 2 {
				t.Fatalf("next call failed to recover using rotated credentials: %v", err)
			}
		})
	}
}
