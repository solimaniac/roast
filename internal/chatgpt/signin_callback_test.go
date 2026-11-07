package chatgpt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func startAsyncSignIn(t *testing.T, c *Client, ctx context.Context) (*url.URL, <-chan error) {
	t.Helper()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- c.SignIn(ctx, "oaiapp_test", func(raw string) error { ready <- raw; return nil }) }()
	select {
	case raw := <-ready:
		auth, _ := url.Parse(raw)
		cb, _ := url.Parse(auth.Query().Get("redirect_uri"))
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
	return string(body)
}

func TestAsyncCallbackCompleteResponseOnFailure(t *testing.T) {
	for _, kind := range []string{"callback", "exchange"} {
		t.Run(kind, func(t *testing.T) {
			for range 20 {
				c := testClient(t)
				seed(t, c, time.Now().Add(time.Hour))
				c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("private-network-marker") })
				var diagnostics bytes.Buffer
				c.SetSignInDiagnostics(&diagnostics)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				cb, done := startAsyncSignIn(t, c, ctx)
				if kind == "callback" {
					q := cb.Query()
					q.Set("error", "private-error-marker")
					cb.RawQuery = q.Encode()
				}
				body := callbackGET(t, cb, 200)
				if body != "Authorization received. Return to Roast to check the result." {
					t.Fatal("unexpected response body")
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
				for _, line := range strings.Split(strings.TrimSpace(diagnostics.String()), "\n") {
					switch line {
					case "chatgpt_signin:listener_ready", "chatgpt_signin:callback_received", "chatgpt_signin:callback_failed", "chatgpt_signin:exchange_started":
					default:
						t.Fatalf("non-enum diagnostic: %q", line)
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
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		close(exchange)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return nil, errors.New("mock failure")
	})
	var diagnostics bytes.Buffer
	c.SetSignInDiagnostics(&diagnostics)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb, done := startAsyncSignIn(t, c, ctx)
	for _, rejection := range []struct{ method, host, event string }{
		{http.MethodPost, cb.Host, "rejected_method"},
		{http.MethodGet, "wrong-host.invalid", "rejected_host"},
	} {
		req, _ := http.NewRequest(rejection.method, cb.String(), nil)
		req.Host = rejection.host
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

func TestCallbackListenerTermination(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{}
	done := serveCallback(server, listener)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || err.Error() != "ChatGPT callback listener stopped before authorization completed" {
			t.Fatal("unexpected listener diagnostic")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listener termination not reported")
	}
	closeCallback(server)
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
	if err := c.locked(ctx, func() error { return nil }); err != nil {
		t.Fatalf("lock not released: %v", err)
	}
}

func TestSignInDiagnosticBoundary(t *testing.T) {
	c := testClient(t)
	var output bytes.Buffer
	c.SetSignInDiagnostics(&output)
	for _, value := range []string{"private-state", "http://127.0.0.1/?code=secret", "Authorization: secret", "listener_ready\nsecret"} {
		c.signInEvent(value)
	}
	if output.Len() != 0 {
		t.Fatal("dynamic diagnostic escaped allowlist")
	}
	c.signInEvent("listener_ready")
	if output.String() != "chatgpt_signin:listener_ready\n" {
		t.Fatal("fixed diagnostic missing")
	}
	c.SetSignInDiagnostics(nil)
	c.signInEvent("listener_failed")
	if output.String() != "chatgpt_signin:listener_ready\n" {
		t.Fatal("disabled diagnostics emitted")
	}
}

func TestCallbackShutdownIsBounded(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(finished)
	})}
	serveCallback(server, listener)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		res, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + listener.Addr().String())
		if err == nil {
			res.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		_ = server.Close()
		t.Fatal("handler did not start")
	}
	begin := time.Now()
	closeCallback(server)
	if time.Since(begin) > 3*time.Second {
		t.Fatal("shutdown exceeded bound")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("fallback close did not cancel active request")
	}
	<-requestDone
}
