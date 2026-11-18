package chatgpt

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestSignInCallbackAndPKCE(t *testing.T) {
	c := testClient(t)
	k := testutil.NewRSAKey(t)
	var nonce, redirect, challenge string
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case jwksURL:
			return jsonResponse(http.StatusOK, testutil.RS256KeySet(k))
		case tokenURL:
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("client_id") != "oaiapp_test" || r.Form.Get("redirect_uri") != redirect || r.Form.Get("resource") != oauth.APIBaseURL {
				t.Error("incorrect exchange")
			}
			verifier := r.Form.Get("code_verifier")
			if len(verifier) < 43 || len(verifier) > 128 {
				t.Error("PKCE verifier length is outside the RFC 7636 range")
			}
			sum := sha256.Sum256([]byte(verifier))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				t.Error("PKCE mismatch")
			}
			return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "access", RefreshToken: "refresh", IDToken: testutil.SignRS256(t, k, claims(nonce)), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600})
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
		if q.Get("code_challenge_method") != "S256" || nonce == "" || q.Get("state") == "" || nonce == q.Get("state") {
			t.Error("authorization must use S256 and independent state and nonce values")
		}
		if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
			t.Error("not loopback")
		}
		callback, err := url.Parse(redirect)
		if err != nil {
			return err
		}
		bad := url.Values{"state": {"wrong"}, "code": {"bad"}}
		callback.RawQuery = bad.Encode()
		callbackGET(t, callback, http.StatusBadRequest)
		callback.RawQuery = url.Values{"state": {q.Get("state")}, "code": {"code"}, "client_id": {"oaiapp_test"}}.Encode()
		callbackGET(t, callback, http.StatusOK)
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	i, e := c.loadIndex()
	if e != nil || i.Active != "oaiapp_test" {
		t.Fatal("registration not activated")
	}
	v, e := c.loadCredential(i.Active)
	if e != nil || v.Subject != "subject" || v.IDToken == "" {
		t.Fatal("identity not stored")
	}
}

func TestIdentityOnlyAndExplicitConsent(t *testing.T) {
	c := testClient(t)
	key := testutil.NewRSAKey(t)
	var nonce string
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
		}
		if r.URL.String() != tokenURL {
			t.Fatal("wrong target")
		}
		return jsonResponse(http.StatusOK, oauth.TokenResponse{IDToken: testutil.SignRS256(t, key, claims(nonce)), Scope: scopeValue("openid profile email")})
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
			callbackGET(t, callback, http.StatusOK)

			return nil
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
	v, err := c.loadCredential("oaiapp_test")
	if err != nil || v.IDToken == "" || v.Subject != "subject" {
		t.Fatal("identity-only sign-in was lost")
	}
	accounts, err := c.Status(context.Background())
	if err != nil || len(accounts) != 1 || !accounts[0].SignedIn || accounts[0].PlanEnabled {
		t.Fatal("identity-only status incorrect")
	}
	c.selected = "oaiapp_test"
	if token, err := c.Token(t.Context()); token != "" || !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "--enable-plan") {
		t.Fatalf("identity-only account should require plan consent: %v", err)
	}
	run(true)
}

func TestFailedInitialExchangePreservesPendingRegistration(t *testing.T) {
	c := testClient(t)
	key := testutil.NewRSAKey(t)
	if err := c.saveIndex(index{HostID: "stable-host", Active: "oaiapp_existing", Clients: []string{"oaiapp_existing"}}); err != nil {
		t.Fatal(err)
	}
	existing := credential{Account: Account{ClientID: "oaiapp_existing", Subject: "existing-subject"}, AccessToken: "existing-access", Scopes: oauth.Scope}
	if err := c.saveCredential(&existing); err != nil {
		t.Fatal(err)
	}
	var nonce string
	exchanges := 0
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
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
		return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "access", RefreshToken: "refresh", IDToken: testutil.SignRS256(t, key, claims(nonce)), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600})
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
		callbackGET(t, callback, http.StatusOK)

		return nil
	}
	if err := c.SignIn(context.Background(), "", show); err == nil || !strings.Contains(err.Error(), "--account oaiapp_test") {
		t.Fatalf("failed exchange must explain retry: %v", err)
	}
	pending, err := c.loadCredential("oaiapp_test")
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
	verified, err := c.loadCredential("oaiapp_test")
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
		name  string
		id    string
		valid bool
	}{
		{"opaque ID", "issued-client-v2", true},
		{"example prefix", "oaiapp_example", true},
		{"registration placeholder", "dynamic_agent_client", false},
		{"empty", "", false},
		{"whitespace", "has space", false},
		{"control character", "line\nbreak", false},
		{"too long", strings.Repeat("a", 257), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
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
				callbackGET(t, callback, http.StatusOK)

				return nil
			})
			if err == nil {
				t.Fatal("expected rejected callback or mocked exchange failure")
			}
			if exchanged != tt.valid {
				t.Fatalf("exchange=%t, valid=%t", exchanged, tt.valid)
			}
			if tt.valid {
				pending, e := c.loadCredential(tt.id)
				if e != nil || !pending.PendingRegistration || pending.ClientID != tt.id {
					t.Fatal("opaque issued ID not retained")
				}
			}
		})
	}
}

func TestSignInContextInteractionWindow(t *testing.T) {
	before := time.Now()
	ctx, cancel := signInContext(context.Background())
	defer cancel()
	after := time.Now()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(30*time.Minute)) || deadline.After(after.Add(30*time.Minute)) {
		t.Fatal("sign-in interaction deadline is not 30 minutes")
	}
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("sign-in context ignored cancellation")
	}
}

func TestSignInContextPreservesCallerDeadlineAndCancellation(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	parent, cancelParent := context.WithDeadline(context.Background(), deadline)
	defer cancelParent()
	ctx, cancel := signInContext(parent)
	defer cancel()
	actual, ok := ctx.Deadline()
	if !ok || !actual.Equal(deadline) {
		t.Fatal("sign-in extended an earlier caller deadline")
	}
	cancelParent()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("sign-in ignored parent cancellation")
	}
}

func startAsyncSignIn(t *testing.T, c *Client, ctx context.Context) (*url.URL, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("sign-in goroutine did not stop during cleanup")
		}
	})
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		defer close(stopped)
		done <- c.SignIn(ctx, "oaiapp_test", func(raw string) error { ready <- raw; return nil })
	}()
	select {
	case raw := <-ready:
		auth, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		cb, err := url.Parse(auth.Query().Get("redirect_uri"))
		if err != nil {
			t.Fatal(err)
		}
		cb.RawQuery = url.Values{"state": {auth.Query().Get("state")}, "code": {"private-code-marker"}}.Encode()
		return cb, done
	case err := <-done:
		t.Fatalf("sign-in exited early: %v", err)
	case <-ctx.Done():
		t.Fatal("sign-in did not start")
	}
	return nil, nil
}

func callbackGET(t *testing.T, cb *url.URL, status int) string {
	t.Helper()
	_, body := callbackGETResponse(t, cb, status)
	return body
}

func callbackGETResponse(t *testing.T, cb *url.URL, status int) (http.Header, string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(cb.String())
	if err != nil {
		t.Fatalf("callback HTTP response failed: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("incomplete callback response: %v", err)
	}
	if res.StatusCode != status {
		t.Fatalf("callback status %d, want %d", res.StatusCode, status)
	}
	if status == http.StatusOK {
		assertCallbackCompletePage(t, res.Header, string(body))
	}
	return res.Header, string(body)
}

func TestAsyncCallbackCompleteResponseOnFailure(t *testing.T) {
	for _, kind := range []string{"callback", "exchange"} {
		t.Run(kind, func(t *testing.T) {
			// Repeat the response/close race, with each attempt isolated and named.
			for attempt := range 20 {
				t.Run(strconv.Itoa(attempt), func(t *testing.T) {
					c := testClient(t)
					seed(t, c, time.Now().Add(time.Hour))
					c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("private-network-marker") })
					diagnostics := newSignInDiagnosticLog()
					c.SetSignInDiagnostics(diagnostics)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					cb, done := startAsyncSignIn(t, c, ctx)
					q := cb.Query()
					q.Set("error_description", `<script>alert("private-description-marker")</script>`)
					q.Set("error_uri", "https://private-error-uri.invalid/")
					q.Set("user_content", "private-user-content-marker")
					if kind == "callback" {
						q.Del("code")
						q.Set("error", "private-error-marker")
					}
					cb.RawQuery = q.Encode()
					header, body := callbackGETResponse(t, cb, 200)
					var response bytes.Buffer
					response.WriteString(body)
					if err := header.Write(&response); err != nil {
						t.Fatal(err)
					}
					for _, values := range q {
						for _, value := range values {
							if strings.Contains(response.String(), value) {
								t.Fatal("callback response exposed request data")
							}
						}
					}
					select {
					case err := <-done:
						if err == nil {
							t.Fatal("expected failure")
						}
					case <-ctx.Done():
						t.Fatal("sign-in stuck")
					}
					cancel()
					diagnostics.waitFor(t, "connection_closed")
					text := diagnostics.String()
					previous := -1
					for _, event := range []string{"connection_opened", "connection_active", "callback_received", "connection_closed"} {
						index := strings.Index(text, "chatgpt_signin:"+event+"\n")
						if index <= previous {
							t.Fatalf("missing or out-of-order phase: %s", event)
						}
						previous = index
					}
					for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
						switch line {
						case "chatgpt_signin:listener_ready", "chatgpt_signin:connection_opened", "chatgpt_signin:connection_active", "chatgpt_signin:connection_closed", "chatgpt_signin:callback_received", "chatgpt_signin:callback_failed", "chatgpt_signin:exchange_started":
						default:
							t.Fatalf("non-enum diagnostic: %q", line)
						}
					}
				})
			}
		})
	}
}

func TestAsyncCallbackOAuthErrorCategories(t *testing.T) {
	for _, tc := range []struct{ name, code, category string }{
		{"denied", "access_denied", "access_denied"},
		{"server", "server_error", "server_error"},
		{"unknown", "private-error-marker", "oauth_error"},
		{"injected", "access_denied\nprivate-error-marker", "oauth_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				t.Error("OAuth error callback started a token exchange")
				return nil, errors.New("unexpected exchange")
			})
			diagnostics := newSignInDiagnosticLog()
			c.SetSignInDiagnostics(diagnostics)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cb, done := startAsyncSignIn(t, c, ctx)
			q := cb.Query()
			q.Set("error", tc.code)
			q.Set("error_description", "private-description-marker")
			q.Set("error_uri", "https://private-error-uri.invalid/")
			q.Set("request_id", "req_private_callback_marker")
			cb.RawQuery = q.Encode()
			header, body := callbackGETResponse(t, cb, http.StatusOK)
			var err error
			select {
			case err = <-done:
			case <-ctx.Done():
				t.Fatal("sign-in stuck after OAuth error callback")
			}
			if err == nil || err.Error() != "ChatGPT authorization failed ("+tc.category+")" {
				t.Fatal("callback did not return the fixed OAuth error category")
			}
			var output bytes.Buffer
			output.WriteString(err.Error())
			output.WriteString(body)
			output.WriteString(diagnostics.String())
			if err := header.Write(&output); err != nil {
				t.Fatal(err)
			}
			for key, values := range q {
				if key == "error" && tc.code == tc.category {
					continue // Only a recognized fixed error category may be shown.
				}
				for _, value := range values {
					if strings.Contains(output.String(), value) {
						t.Fatal("OAuth error callback exposed request data")
					}
				}
			}
		})
	}
}

func TestAsyncCallbackRejectionsAndDuplicate(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	exchange := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		close(exchange)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return nil, errors.New("mock failure")
	})
	diagnostics := newSignInDiagnosticLog()
	c.SetSignInDiagnostics(diagnostics)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb, done := startAsyncSignIn(t, c, ctx)
	for _, rejection := range []struct{ method, host, event string }{
		{http.MethodPost, cb.Host, "rejected_method"},
		{http.MethodGet, "wrong-host.invalid", "rejected_host"},
	} {
		req, err := http.NewRequest(rejection.method, cb.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = rejection.host
		req.Header.Set("Authorization", "Bearer private-auth-marker")
		req.Header.Set("Cookie", "session=private-cookie-marker")
		res, err := (&http.Client{Timeout: time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("accepted %s", rejection.event)
		}
	}
	invalid := *cb
	invalid.Path = "/private-path-marker"
	callbackGET(t, &invalid, 404)
	invalid = *cb
	q := invalid.Query()
	q.Set("state", "private-state-marker")
	invalid.RawQuery = q.Encode()
	callbackGET(t, &invalid, 400)
	callbackGET(t, cb, 200)
	select {
	case <-exchange:
	case <-ctx.Done():
		t.Fatal("exchange not started")
	}
	callbackGET(t, cb, 400)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation stuck")
	}
	text := diagnostics.String()
	for _, event := range []string{"rejected_method", "rejected_host", "rejected_path", "rejected_state", "callback_received", "rejected_duplicate", "exchange_started"} {
		if !strings.Contains(text, "chatgpt_signin:"+event+"\n") {
			t.Errorf("missing %s", event)
		}
	}
	if strings.Contains(text, "private-") || strings.Contains(text, "127.0.0.1") || strings.Contains(text, "oaiapp_test") {
		t.Fatal("diagnostics exposed request data")
	}
}

func TestAsyncSignInCancellationReleasesLock(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, done := startAsyncSignIn(t, c, ctx)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sign-in stuck")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.withSessionLock(ctx, func() error { return nil }); err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	// Browser waiting holds the per-account authorization lock, not the store
	// lock above. A second sign-in must reach its browser callback for the same
	// account; otherwise the canceled operation leaked its authorization lock.
	retryCtx, retryCancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer retryCancel()
	_, retryDone := startAsyncSignIn(t, c, retryCtx)
	retryCancel()
	if err := awaitMockSignIn(t, retryDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("second sign-in cancellation = %v, want context.Canceled", err)
	}

}

// ConnState close hooks can run just after SignIn returns, and tests also read
// diagnostics while the server is waiting for request bytes.
type signInDiagnosticLog struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	changed chan struct{}
}

func newSignInDiagnosticLog() *signInDiagnosticLog {
	return &signInDiagnosticLog{changed: make(chan struct{}, 1)}
}

func (d *signInDiagnosticLog) Write(p []byte) (int, error) {
	d.mu.Lock()
	n, err := d.buffer.Write(p)
	d.mu.Unlock()
	select {
	case d.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (d *signInDiagnosticLog) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buffer.String()
}

func (d *signInDiagnosticLog) waitFor(t *testing.T, event string) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for !strings.Contains(d.String(), "chatgpt_signin:"+event+"\n") {
		select {
		case <-d.changed:
		case <-timer.C:
			t.Fatalf("missing diagnostic: %s", event)
		}
	}
}

func TestAsyncCallbackConnectionBeforeHTTP(t *testing.T) {
	for _, kind := range []string{"no request", "incomplete request", "malformed request"} {
		t.Run(kind, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("unexpected exchange")
			})
			diagnostics := newSignInDiagnosticLog()
			c.SetSignInDiagnostics(diagnostics)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cb, done := startAsyncSignIn(t, c, ctx)
			conn, err := net.DialTimeout("tcp4", cb.Host, time.Second)
			if err != nil {
				t.Fatal("cannot connect to test callback")
			}
			defer conn.Close()
			diagnostics.waitFor(t, "connection_opened")
			want := "chatgpt_signin:listener_ready\nchatgpt_signin:connection_opened\n"
			if diagnostics.String() != want {
				t.Fatal("unexpected phase before sending request bytes")
			}
			if kind != "no request" {
				// These are synthetic sentinels, never real credentials. Neither
				// incomplete headers nor a malformed protocol reaches the handler.
				raw := "GET /private-path-marker?code=private-code-marker&state=private-state-marker HTTP/1.1\r\nHost: " + cb.Host + "\r\nAuthorization: Bearer private-auth-marker\r\nCookie: session=private-cookie-marker\r\n"
				if kind == "malformed request" {
					raw = strings.Replace(raw, "HTTP/1.1", "HTTP/private-protocol-marker", 1) + "\r\n"
				}
				if _, err := io.WriteString(conn, raw); err != nil {
					t.Fatal("cannot write synthetic request")
				}
				want += "chatgpt_signin:connection_active\n"
			}
			_ = conn.Close()
			diagnostics.waitFor(t, "connection_closed")
			want += "chatgpt_signin:connection_closed\n"
			if diagnostics.String() != want {
				t.Fatal("unexpected phase or request data in connection diagnostics")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("unexpected sign-in result")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("sign-in did not stop")
			}
			if diagnostics.String() != want+"chatgpt_signin:canceled\n" {
				t.Fatal("unexpected phase or request data after cancellation")
			}
		})
	}
}

func TestAsyncCallbackConnectionClosedOnCancellation(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	diagnostics := newSignInDiagnosticLog()
	c.SetSignInDiagnostics(diagnostics)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb, done := startAsyncSignIn(t, c, ctx)
	conn, err := net.DialTimeout("tcp4", cb.Host, time.Second)
	if err != nil {
		t.Fatal("cannot connect to test callback")
	}
	defer conn.Close()
	diagnostics.waitFor(t, "connection_opened")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("unexpected sign-in result")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("open connection prevented bounded shutdown")
	}
	diagnostics.waitFor(t, "connection_closed")
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal("cannot set test read deadline")
	}
	var b [1]byte
	if _, err := conn.Read(b[:]); !errors.Is(err, io.EOF) {
		t.Fatal("callback connection was not closed")
	}
	if late, err := net.DialTimeout("tcp4", cb.Host, time.Second); err == nil {
		late.Close()
		t.Fatal("callback listener remained open")
	}
	want := "chatgpt_signin:listener_ready\nchatgpt_signin:connection_opened\nchatgpt_signin:canceled\nchatgpt_signin:connection_closed\n"
	if diagnostics.String() != want {
		t.Fatal("unexpected cleanup diagnostic")
	}
}

func assertCallbackCompletePage(t *testing.T, header http.Header, body string) {
	t.Helper()
	const wantBody = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Authorization received</title>
<script>history.replaceState(null,'','/auth/complete');</script>
</head>
<body><p>Authorization received. Return to Roast to check the result.</p></body>
</html>
`
	if body != wantBody {
		t.Fatal("callback completion page changed or included request data")
	}
	_, scriptAndRest, ok := strings.Cut(body, "<script>")
	if !ok {
		t.Fatal("callback URL cleanup script missing")
	}
	script, _, ok := strings.Cut(scriptAndRest, "</script>")
	if !ok || script != "history.replaceState(null,'','/auth/complete');" {
		t.Fatal("callback script does not replace the URL with the fixed completion path")
	}
	hash := sha256.Sum256([]byte(script))
	policy := "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	for name, want := range map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"Content-Security-Policy": policy,
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
	} {
		if got := header.Values(name); len(got) != 1 || got[0] != want {
			t.Errorf("unexpected callback %s header", name)
		}
	}
	if header.Get("Location") != "" || header.Get("Set-Cookie") != "" {
		t.Fatal("callback completion page redirects or sets cookies")
	}
}

// All OAuth traffic uses a synthetic transport; only the callback uses loopback.
func mockSignInTransport(t *testing.T, key *rsa.PrivateKey) transportFunc {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case tokenURL:
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("grant_type") != "authorization_code" {
				t.Error("unexpected grant type")
				return nil, errors.New("unexpected grant type")
			}
			id := r.Form.Get("client_id")
			cl := claims(r.Form.Get("code")) // The fake code carries the test nonce.
			cl["aud"] = id
			return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "authorized-" + id, RefreshToken: "authorized-refresh-" + id, IDToken: testutil.SignRS256(t, key, cl), TokenType: "Bearer", ExpiresIn: 3600, Scope: scopeValue(oauth.Scope)})
		case jwksURL:
			return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
		case discoveryURL:
			return jsonResponse(http.StatusOK, map[string]string{"issuer": oauth.Issuer, "revocation_endpoint": oauth.Issuer + "/revoke"})
		case oauth.Issuer + "/revoke":
			return jsonResponse(http.StatusOK, nil)
		default:
			t.Error("unexpected mock OAuth request")
			return nil, errors.New("unexpected request")
		}
	}
}

func beginMockSignIn(t *testing.T, c *Client, id string) (url.Values, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	stopped := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("sign-in goroutine did not stop during cleanup")
		}
	})
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		defer close(stopped)
		done <- c.SignIn(ctx, id, func(raw string) error { ready <- raw; return nil })
	}()
	select {
	case raw := <-ready:
		auth, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return auth.Query(), done
	case err := <-done:
		t.Fatalf("sign-in exited before browser wait: %v", err)
	case <-ctx.Done():
		t.Fatal("sign-in did not reach browser wait")
	}
	return nil, nil
}

func completeMockCallback(t *testing.T, auth url.Values, id string) {
	t.Helper()
	cb, err := url.Parse(auth.Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	cb.RawQuery = url.Values{"state": {auth.Get("state")}, "code": {auth.Get("nonce")}, "client_id": {id}}.Encode()
	callbackGET(t, cb, http.StatusOK)
}

func awaitMockSignIn(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("sign-in did not finish")
	}
	return nil
}

func addMockAccount(t *testing.T, c *Client, id string) {
	t.Helper()
	if err := c.withSessionLock(context.Background(), func() error {
		i, err := c.loadIndex()
		if err != nil {
			return err
		}
		if err = c.saveCredential(&credential{Account: Account{ClientID: id, Subject: "subject", ExpiresAt: time.Now().Add(time.Hour)}, AccessToken: "access-" + id, RefreshToken: "refresh-" + id, Scopes: oauth.Scope}); err != nil {
			return err
		}
		i.Clients = append(i.Clients, id)
		return c.saveIndex(i)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingSignInLeavesAccountOperationsAvailable(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new registration"
		if existing {
			name = "saved registration"
		}
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			c.provider.HTTP.Transport = mockSignInTransport(t, testutil.NewRSAKey(t))
			id := ""
			if existing {
				id = "account-b"
				addMockAccount(t, c, id)
			}
			auth, done := beginMockSignIn(t, c, id)
			// A separate client shares only the persisted store, as another CLI does.
			other := *c
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if token, err := other.Token(ctx); err != nil || token != "access" {
				t.Fatalf("pending sign-in blocked token access: %v", err)
			}
			if _, err := other.Status(ctx); err != nil {
				t.Fatalf("pending sign-in blocked status: %v", err)
			}
			if err := other.Select(ctx, c.selected); err != nil {
				t.Fatalf("pending sign-in blocked selection: %v", err)
			}
			if err := other.SignOut(ctx, c.selected); err != nil {
				t.Fatalf("pending sign-in blocked sign-out: %v", err)
			}
			completeMockCallback(t, auth, "account-b")
			if err := awaitMockSignIn(t, done); err != nil {
				t.Fatal(err)
			}
			i, err := c.loadIndex()
			if err != nil || i.Active != "" || len(i.Clients) != 2 {
				t.Fatalf("sign-in overwrote sign-out or lost registrations: %+v, %v", i, err)
			}
			v, err := c.loadCredential(c.selected)
			if err != nil || v.AccessToken != "" || v.RefreshToken != "" {
				t.Fatal("sign-in resurrected the signed-out account")
			}
			v, err = c.loadCredential("account-b")
			if err != nil || v.AccessToken != "authorized-account-b" {
				t.Fatal("independent sign-in did not persist")
			}
		})
	}
}

func TestPendingSignInPreservesExplicitReselection(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.provider.HTTP.Transport = mockSignInTransport(t, testutil.NewRSAKey(t))
	auth, done := beginMockSignIn(t, c, "")
	if err := c.Select(context.Background(), c.selected); err != nil {
		t.Fatal(err)
	}
	completeMockCallback(t, auth, "account-b")
	if err := awaitMockSignIn(t, done); err != nil {
		t.Fatal(err)
	}
	i, err := c.loadIndex()
	if err != nil || i.Active != c.selected {
		t.Fatalf("sign-in displaced a newer explicit re-selection: %+v, %v", i, err)
	}
}

func TestConcurrentSignInsMergeRegistrationsAndSelection(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	addMockAccount(t, c, "account-b")
	c.provider.HTTP.Transport = mockSignInTransport(t, testutil.NewRSAKey(t))
	first, firstDone := beginMockSignIn(t, c, "account-b")
	other := *c
	second, secondDone := beginMockSignIn(t, &other, "")
	completeMockCallback(t, second, "account-c")
	if err := awaitMockSignIn(t, secondDone); err != nil {
		t.Fatal(err)
	}
	completeMockCallback(t, first, "account-b")
	if err := awaitMockSignIn(t, firstDone); err != nil {
		t.Fatal(err)
	}
	i, err := c.loadIndex()
	if err != nil || i.Active != "account-c" || len(i.Clients) != 3 || !slices.Contains(i.Clients, "account-b") || !slices.Contains(i.Clients, "account-c") {
		t.Fatalf("concurrent sign-in lost index updates: %+v, %v", i, err)
	}
}

func TestSignInRejectsConcurrentCredentialChange(t *testing.T) {
	for _, phase := range []string{"browser wait", "exchange"} {
		for _, mutation := range []string{"refresh", "sign-out"} {
			t.Run(phase+"/"+mutation, func(t *testing.T) {
				c := testClient(t)
				seed(t, c, time.Now().Add(-time.Minute))
				base := mockSignInTransport(t, testutil.NewRSAKey(t))
				exchanging := make(chan struct{})
				release := make(chan struct{})
				var exchanges atomic.Int32
				c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.String() == tokenURL {
						if err := r.ParseForm(); err != nil {
							return nil, err
						}
						if r.Form.Get("grant_type") == "refresh_token" {
							return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", TokenType: "Bearer", ExpiresIn: 3600, Scope: scopeValue(oauth.Scope)})
						}
						exchanges.Add(1)
						close(exchanging)
						select {
						case <-release:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					return base(r)
				})
				auth, done := beginMockSignIn(t, c, c.selected)
				if phase == "exchange" {
					completeMockCallback(t, auth, c.selected)
					select {
					case <-exchanging:
					case <-time.After(3 * time.Second):
						t.Fatal("exchange did not begin")
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if mutation == "refresh" {
					if token, err := c.Token(ctx); err != nil || token != "rotated-access" {
						t.Fatalf("concurrent refresh failed: %v", err)
					}
				} else if err := c.SignOut(ctx, c.selected); err != nil {
					t.Fatalf("concurrent sign-out failed: %v", err)
				}
				close(release)
				if phase == "browser wait" {
					completeMockCallback(t, auth, c.selected)
				}
				if err := awaitMockSignIn(t, done); err == nil || !strings.Contains(err.Error(), "credentials changed") {
					t.Fatalf("stale sign-in was not rejected: %v", err)
				}
				if phase == "browser wait" && exchanges.Load() != 0 {
					t.Fatal("consumed code after credentials had already changed")
				}
				v, err := c.loadCredential(c.selected)
				if err != nil {
					t.Fatal(err)
				}
				if mutation == "refresh" {
					if v.AccessToken != "rotated-access" || v.RefreshToken != "rotated-refresh" {
						t.Fatal("stale OAuth response overwrote rotated credentials")
					}
				} else if v.AccessToken != "" || v.RefreshToken != "" || v.IDToken != "" {
					t.Fatal("stale OAuth response resurrected signed-out credentials")
				}
				// A fresh explicit invocation can still reauthorize the registration.
				c.provider.HTTP.Transport = base
				auth, done = beginMockSignIn(t, c, c.selected)
				completeMockCallback(t, auth, c.selected)
				if err = awaitMockSignIn(t, done); err != nil {
					t.Fatalf("fresh explicit sign-in failed: %v", err)
				}
			})
		}
	}
}

func TestOverlappingAuthorizationUsesIndependentLock(t *testing.T) {
	for _, newRegistration := range []bool{false, true} {
		name := "saved registration"
		if newRegistration {
			name = "new registration after callback"
		}
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			exchanging := make(chan struct{})
			release := make(chan struct{})
			base := mockSignInTransport(t, testutil.NewRSAKey(t))
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == tokenURL {
					close(exchanging)
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				return base(r)
			})
			id := c.selected
			if newRegistration {
				id = ""
			}
			auth, done := beginMockSignIn(t, c, id)
			if newRegistration {
				id = "account-b"
				completeMockCallback(t, auth, id)
				select {
				case <-exchanging:
				case <-time.After(3 * time.Second):
					t.Fatal("exchange did not begin")
				}
			}
			other := *c
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			err := other.SignIn(ctx, id, func(string) error {
				t.Error("overlapping authorization reached browser")
				return errors.New("unexpected overlapping authorization")
			})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("overlapping authorization did not respect its lock: %v", err)
			}
			if _, err := other.Status(context.Background()); err != nil {
				t.Fatalf("authorization lock blocked store access: %v", err)
			}
			close(release)
			if !newRegistration {
				completeMockCallback(t, auth, id)
			}
			if err := awaitMockSignIn(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPendingRegistrationSignOutPreventsActivation(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	base := mockSignInTransport(t, testutil.NewRSAKey(t))
	exchanging := make(chan struct{})
	release := make(chan struct{})
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == tokenURL {
			close(exchanging)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return base(r)
	})
	auth, done := beginMockSignIn(t, c, "")
	completeMockCallback(t, auth, "account-b")
	select {
	case <-exchanging:
	case <-time.After(3 * time.Second):
		t.Fatal("exchange did not begin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.SignOut(ctx, "account-b"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := awaitMockSignIn(t, done); err == nil || !strings.Contains(err.Error(), "credentials changed") {
		t.Fatalf("signed-out pending registration was activated: %v", err)
	}
	v, err := c.loadCredential("account-b")
	if err != nil || !v.PendingRegistration || v.AccessToken != "" || v.IDToken != "" {
		t.Fatal("pending registration was not left signed out")
	}
	i, err := c.loadIndex()
	if err != nil || i.Active != c.selected {
		t.Fatal("pending registration displaced active account")
	}
}

func TestSignInRejectsChangedHost(t *testing.T) {
	for _, phase := range []string{"browser wait", "exchange"} {
		t.Run(phase, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			base := mockSignInTransport(t, testutil.NewRSAKey(t))
			exchanging := make(chan struct{})
			release := make(chan struct{})
			var exchanges atomic.Int32
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == tokenURL {
					exchanges.Add(1)
					close(exchanging)
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				return base(r)
			})
			auth, done := beginMockSignIn(t, c, c.selected)
			if phase == "exchange" {
				completeMockCallback(t, auth, c.selected)
				select {
				case <-exchanging:
				case <-time.After(3 * time.Second):
					t.Fatal("exchange did not begin")
				}
			}
			if err := c.withSessionLock(context.Background(), func() error {
				i, err := c.loadIndex()
				if err != nil {
					return err
				}
				i.HostID = "different-host"
				return c.saveIndex(i)
			}); err != nil {
				t.Fatal(err)
			}
			close(release)
			if phase == "browser wait" {
				completeMockCallback(t, auth, c.selected)
			}
			if err := awaitMockSignIn(t, done); err == nil || !strings.Contains(err.Error(), "host identity changed") {
				t.Fatalf("changed host was not rejected: %v", err)
			}
			if phase == "browser wait" && exchanges.Load() != 0 {
				t.Fatal("consumed code after host had already changed")
			}
			v, err := c.loadCredential(c.selected)
			if err != nil || v.AccessToken != "access" {
				t.Fatal("sign-in for old host changed credentials")
			}
		})
	}
}

func TestSignInRejectsLegacyCredentialWrites(t *testing.T) {
	for _, mutation := range []string{"refresh", "sign-out", "repeated sign-out", "preserved generation"} {
		t.Run(mutation, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			v, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			v.Generation = ""
			if mutation == "repeated sign-out" {
				v.AccessToken, v.RefreshToken, v.Scopes = "", "", ""
			}
			if err = storage.WriteJSON(c.credentialPath(v.ClientID), v); err != nil {
				t.Fatal(err)
			}
			auth, done := beginMockSignIn(t, c, c.selected)
			if err = c.withSessionLock(context.Background(), func() error {
				current, err := c.loadCredential(c.selected)
				if err != nil {
					return err
				}
				if current.Generation == "" {
					t.Error("legacy sign-in snapshot was not upgraded")
				}
				if mutation != "preserved generation" {
					current.Generation = "" // Simulate an older writer dropping the field.
				}
				if mutation == "refresh" || mutation == "preserved generation" {
					current.AccessToken, current.RefreshToken = "legacy-access", "legacy-refresh"
				} else {
					current.AccessToken, current.RefreshToken, current.Scopes = "", "", ""
				}
				return storage.WriteJSON(c.credentialPath(current.ClientID), current)
			}); err != nil {
				t.Fatal(err)
			}
			completeMockCallback(t, auth, c.selected)
			if err = awaitMockSignIn(t, done); err == nil || !strings.Contains(err.Error(), "credentials changed") {
				t.Fatalf("legacy credential change was not detected before exchange: %v", err)
			}
		})
	}
}

func TestSignInTokenExpiryAnchoredToReceipt(t *testing.T) {
	c := testClient(t)
	received := time.Now().UTC().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(received.Unix())
	c.now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	base := mockSignInTransport(t, testutil.NewRSAKey(t))
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			clock.Store(received.Add(2 * time.Minute).Unix())
		}
		return base(r)
	})
	auth, done := beginMockSignIn(t, c, "")
	completeMockCallback(t, auth, "account-b")
	if err := awaitMockSignIn(t, done); err != nil {
		t.Fatal(err)
	}
	v, err := c.loadCredential("account-b")
	if err != nil || !v.ExpiresAt.Equal(received.Add(time.Hour)) {
		t.Fatalf("JWKS delay extended access-token expiry: %v, %v", v.ExpiresAt, err)
	}
}

// writeSelectionVersion simulates an older writer while a browser sign-in is
// pending. It returns the version that the pending sign-in could have observed.
func writeSelectionVersion(t *testing.T, c *Client, version uint64) uint64 {
	t.Helper()
	var previous uint64
	if err := c.withSessionLock(t.Context(), func() error {
		i, err := c.loadIndex()
		if err != nil {
			return err
		}
		previous = i.SelectionVersion
		i.SelectionVersion = version
		return c.saveIndex(i)
	}); err != nil {
		t.Fatal(err)
	}
	return previous
}

func TestPendingSignInPreservesLegacyReselection(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.provider.HTTP.Transport = mockSignInTransport(t, testutil.NewRSAKey(t))
	auth, done := beginMockSignIn(t, c, "")
	// An older binary writes no version when explicitly reselecting A.
	if previous := writeSelectionVersion(t, c, 0); previous == 0 {
		t.Error("legacy selection snapshot was not upgraded")
	}
	completeMockCallback(t, auth, "account-b")
	if err := awaitMockSignIn(t, done); err != nil {
		t.Fatal(err)
	}
	i, err := c.loadIndex()
	if err != nil || i.Active != c.selected {
		t.Fatalf("sign-in displaced a legacy explicit re-selection: %+v, %v", i, err)
	}
}

func TestPendingSignInPreservesLegacyReselectionAcrossAnotherSignIn(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	addMockAccount(t, c, "account-b")
	addMockAccount(t, c, "account-c")
	c.provider.HTTP.Transport = mockSignInTransport(t, testutil.NewRSAKey(t))
	writeSelectionVersion(t, c, 1)
	// B snapshots (A, 1), then an older binary explicitly reselects A and
	// drops the revision. C begins authorizing and initializes that revision.
	first, firstDone := beginMockSignIn(t, c, "account-b")
	writeSelectionVersion(t, c, 0)
	other := *c
	second, secondDone := beginMockSignIn(t, &other, "account-c")
	i, err := c.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if i.SelectionVersion == 0 || i.SelectionVersion == 1 {
		t.Error("normalizing a missing revision reused an earlier snapshot")
	}
	// C is still at the browser prompt when B completes. B must preserve A's
	// explicit selection, rather than mistaking C's initialization for (A, 1).
	completeMockCallback(t, first, "account-b")
	if err = awaitMockSignIn(t, firstDone); err != nil {
		t.Fatal(err)
	}
	i, err = c.loadIndex()
	if err != nil || i.Active != c.selected {
		t.Errorf("sign-in displaced a legacy explicit re-selection after revision normalization: %+v, %v", i, err)
	}
	completeMockCallback(t, second, "account-c")
	if err = awaitMockSignIn(t, secondDone); err != nil {
		t.Fatal(err)
	}
}

func TestPendingSignInPreservesLegacyReselectionAcrossSelect(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	addMockAccount(t, c, "account-b")
	c.provider.HTTP.Transport = mockSignInTransport(t, testutil.NewRSAKey(t))
	// Begin with a legacy index. Select used to create revision 1 here.
	if err := c.Select(context.Background(), c.selected); err != nil {
		t.Fatal(err)
	}
	before, err := c.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	auth, done := beginMockSignIn(t, c, "account-b")
	// An older Select(A) drops the revision. A newer Select(A) must not
	// recreate B's snapshot, even though the selected account stays the same.
	writeSelectionVersion(t, c, 0)
	other := *c
	if err = other.Select(context.Background(), c.selected); err != nil {
		t.Fatal(err)
	}
	after, err := c.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if after.SelectionVersion == 0 || after.SelectionVersion == before.SelectionVersion {
		t.Error("reselecting after a legacy reset recreated the pending snapshot")
	}
	completeMockCallback(t, auth, "account-b")
	if err = awaitMockSignIn(t, done); err != nil {
		t.Fatal(err)
	}
	after, err = c.loadIndex()
	if err != nil || after.Active != c.selected {
		t.Fatalf("pending sign-in overrode the newer Select(A): %+v, %v", after, err)
	}
}

func TestSelectionWritePathsUseFreshRevisions(t *testing.T) {
	for _, operation := range []string{"select", "active sign-out", "sign-in activation", "legacy initialization"} {
		for _, baseline := range []struct {
			name    string
			version uint64
		}{{"legacy", 0}, {"versioned", 37}} {
			if operation == "legacy initialization" && baseline.version != 0 {
				continue
			}
			t.Run(operation+"/"+baseline.name, func(t *testing.T) {
				c := testClient(t)
				seed(t, c, time.Now().Add(time.Hour))
				addMockAccount(t, c, "account-b")
				c.provider.HTTP.Transport = mockSignInTransport(t, testutil.NewRSAKey(t))
				signedIn, err := c.loadCredential(c.selected)
				if err != nil {
					t.Fatal(err)
				}
				seen := make(map[uint64]bool)
				for range 2 {
					// Reset the same index revision, as a legacy writer can do.
					// Restore A's synthetic credentials for the sign-out case.
					if err = c.withSessionLock(context.Background(), func() error {
						i, err := c.loadIndex()
						if err != nil {
							return err
						}
						i.Active = c.selected
						i.SelectionVersion = baseline.version
						if err = c.saveCredential(&signedIn); err != nil {
							return err
						}
						return c.saveIndex(i)
					}); err != nil {
						t.Fatal(err)
					}
					expectedActive := c.selected
					var activationSnapshot uint64
					switch operation {
					case "select":
						err = c.Select(context.Background(), c.selected)
					case "active sign-out":
						err = c.SignOut(context.Background(), c.selected)
						expectedActive = ""
					case "sign-in activation":
						auth, done := beginMockSignIn(t, c, "account-b")
						i, loadErr := c.loadIndex()
						if loadErr != nil {
							t.Fatal(loadErr)
						}
						activationSnapshot = i.SelectionVersion
						completeMockCallback(t, auth, "account-b")
						err = awaitMockSignIn(t, done)
						expectedActive = "account-b"
					case "legacy initialization":
						auth, done := beginMockSignIn(t, c, "account-b")
						cb, parseErr := url.Parse(auth.Get("redirect_uri"))
						if parseErr != nil {
							t.Fatal(parseErr)
						}
						// Decline after initialization to isolate its revision write.
						cb.RawQuery = url.Values{"state": {auth.Get("state")}, "error": {"access_denied"}}.Encode()
						callbackGET(t, cb, http.StatusOK)
						if declined := awaitMockSignIn(t, done); declined == nil || declined.Error() != "ChatGPT authorization failed (access_denied)" {
							t.Fatalf("expected declined sign-in: %v", declined)
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					i, err := c.loadIndex()
					if err != nil {
						t.Fatal(err)
					}
					if i.Active != expectedActive {
						t.Fatalf("wrong active selection: %q", i.Active)
					}
					if i.SelectionVersion == 0 || i.SelectionVersion == baseline.version || seen[i.SelectionVersion] {
						t.Fatal("selection write reused a revision after reset")
					}
					if operation == "sign-in activation" && i.SelectionVersion == activationSnapshot {
						t.Fatal("activation did not replace its authorization snapshot")
					}
					seen[i.SelectionVersion] = true
				}
			})
		}
	}
}
