package loopback

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestReauthorizationCallbackParameters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query url.Values
		valid bool
	}{
		{"client ID omitted", url.Values{"code": {"one-use-code"}}, true},
		{"same client ID", url.Values{"code": {"one-use-code"}, "client_id": {"saved"}}, true},
		{"explicit empty client ID", url.Values{"code": {"one-use-code"}, "client_id": {""}}, false},
		{"empty error with code", url.Values{"code": {"one-use-code"}, "error": {""}}, false},
		{"repeated error with code", url.Values{"code": {"one-use-code"}, "error": {"", "access_denied"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseCallback(tc.query, "saved")
			if tc.valid {
				if err != nil || result != (Result{Code: "one-use-code", ClientID: "saved"}) {
					t.Fatalf("callback lost the saved registration: %v", err)
				}
			} else if err == nil || result != (Result{}) {
				t.Fatal("malformed callback supplied a code for exchange")
			}
		})
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

func TestSignInDiagnosticBoundary(t *testing.T) {
	var output bytes.Buffer
	diagnostic := NewDiagnostics(&output)
	for _, value := range []string{"private-state", "http://127.0.0.1/?code=secret", "Authorization: secret", "listener_ready\nsecret", "connection_opened\nsecret", "connection_active:secret", "connection_closed http://private-host/"} {
		diagnostic.Emit(value)
	}
	if output.Len() != 0 {
		t.Fatal("dynamic diagnostic escaped allowlist")
	}
	diagnostic.Emit("listener_ready")
	if output.String() != "chatgpt_signin:listener_ready\n" {
		t.Fatal("fixed diagnostic missing")
	}
	diagnostic = NewDiagnostics(nil)
	diagnostic.Emit("listener_failed")
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

func TestCallbackConnectionDiagnosticBoundary(t *testing.T) {
	var output bytes.Buffer
	diagnostic := NewDiagnostics(&output)
	hook := callbackConnState(diagnostic)
	// The callback must neither inspect the connection (nil is deliberate) nor
	// read a replaced setting during a late close notification.
	diagnostic = NewDiagnostics(nil)
	for _, state := range []http.ConnState{http.StateNew, http.StateActive, http.StateIdle, http.StateHijacked, http.StateClosed, http.ConnState(-1), http.ConnState(99)} {
		hook(nil, state)
	}
	if output.String() != "chatgpt_signin:connection_opened\nchatgpt_signin:connection_active\nchatgpt_signin:connection_closed\n" {
		t.Fatal("connection state escaped fixed event mapping")
	}
	if callbackConnState(diagnostic) != nil {
		t.Fatal("disabled diagnostics installed a connection hook")
	}
}

func TestConcurrentCallbacksAcceptOneResult(t *testing.T) {
	const attempts = 16
	callbacks := make(chan callback, attempts)
	handler := callbackHandler("127.0.0.1:1234", "expected", "account", Diagnostics{}, callbacks)
	statuses := make(chan int, attempts)
	start := make(chan struct{})
	var requests sync.WaitGroup
	for range attempts {
		requests.Go(func() {
			<-start
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/auth/callback?state=expected&code=code", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			statuses <- response.Code
		})
	}
	close(start)
	requests.Wait()
	close(statuses)

	accepted := 0
	for status := range statuses {
		switch status {
		case http.StatusOK:
			accepted++
		case http.StatusBadRequest:
		default:
			t.Fatalf("unexpected callback status %d", status)
		}
	}
	if accepted != 1 || len(callbacks) != 1 {
		t.Fatalf("accepted %d requests and published %d results, want one each", accepted, len(callbacks))
	}
	got := <-callbacks
	if got.err != nil || got.result != (Result{Code: "code", ClientID: "account"}) {
		t.Fatalf("unexpected callback result: %+v", got)
	}
}
