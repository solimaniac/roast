package chatgpt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

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
