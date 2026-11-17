package loopback

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

const callbackPath = "/auth/callback"

type Result struct {
	Code     string
	ClientID string
}

type callback struct {
	result Result
	err    error
}

type Listener struct {
	redirect    string
	server      *http.Server
	stopped     <-chan error
	callbacks   <-chan callback
	diagnostics Diagnostics
}

func Listen(state, clientID string, diagnostics Diagnostics) (*Listener, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("cannot open ChatGPT loopback callback")
	}
	host := listener.Addr().String()
	callbacks := make(chan callback, 1)
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnState:         callbackConnState(diagnostics),
		Handler:           callbackHandler(host, state, clientID, diagnostics, callbacks),
	}
	stopped := serveCallback(server, listener)
	diagnostics.Emit("listener_ready")
	return &Listener{
		redirect:    "http://" + host + callbackPath,
		server:      server,
		stopped:     stopped,
		callbacks:   callbacks,
		diagnostics: diagnostics,
	}, nil
}

func (l *Listener) RedirectURI() string { return l.redirect }

// Wait returns the first callback with matching state. Close the listener when
// sign-in ends so it can reject duplicate callbacks during the exchange.
func (l *Listener) Wait(ctx context.Context) (Result, error) {
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			l.diagnostics.Emit("timed_out")
		} else {
			l.diagnostics.Emit("canceled")
		}
		return Result{}, ctx.Err()
	case err := <-l.stopped:
		l.diagnostics.Emit("listener_failed")
		return Result{}, err
	case cb := <-l.callbacks:
		if cb.err != nil {
			l.diagnostics.Emit("callback_failed")
		}
		return cb.result, cb.err
	}
}

func (l *Listener) Close() { closeCallback(l.server) }

func callbackHandler(host, state, clientID string, diagnostics Diagnostics, callbacks chan<- callback) http.HandlerFunc {
	var received atomic.Bool
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		rejection := ""
		switch {
		case r.Method != http.MethodGet:
			rejection = "rejected_method"
		case r.URL.Path != callbackPath:
			rejection = "rejected_path"
		case r.Host != host:
			rejection = "rejected_host"
		}
		if rejection != "" {
			diagnostics.Emit(rejection)
			http.NotFound(w, r)
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(values["state"]) != 1 || subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(state)) != 1 {
			diagnostics.Emit("rejected_state")
			http.Error(w, "Invalid authorization state", http.StatusBadRequest)
			return
		}
		if !received.CompareAndSwap(false, true) {
			diagnostics.Emit("rejected_duplicate")
			http.Error(w, "Authorization already received", http.StatusBadRequest)
			return
		}
		diagnostics.Emit("callback_received")
		result, err := parseCallback(values, clientID)
		callbacks <- callback{result: result, err: err}
		writeCallbackComplete(w)
	}
}

func parseCallback(values url.Values, clientID string) (Result, error) {
	code, issuedID := values.Get("code"), values.Get("client_id")
	switch {
	case values.Get("error") != "":
		return Result{}, fmt.Errorf("ChatGPT authorization failed (%s)", oauth.ErrorCategory(values.Get("error")))
	case len(values["code"]) != 1 || code == "" || len(values["client_id"]) > 1:
		return Result{}, errors.New("incomplete ChatGPT callback")
	case clientID == "" && !validIssuedClientID(issuedID):
		return Result{}, errors.New("missing issued ChatGPT client ID")
	case clientID != "" && issuedID != "" && issuedID != clientID:
		return Result{}, errors.New("ChatGPT registration changed during sign-in")
	}
	if clientID == "" {
		clientID = issuedID
	}
	return Result{Code: code, ClientID: clientID}, nil
}

func serveCallback(server *http.Server, listener net.Listener) <-chan error {
	done := make(chan error, 1)
	go func() {
		_ = server.Serve(listener)
		// Do not expose the underlying network error, which may contain addresses.
		done <- errors.New("ChatGPT callback listener stopped before authorization completed")
	}()
	return done
}

func closeCallback(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if server.Shutdown(ctx) != nil {
		_ = server.Close()
	}
}

// Issued client IDs are opaque. The documented example prefix is not a format
// contract; reject only the registration placeholder and malformed values.
func validIssuedClientID(id string) bool {
	return id != "" && id != oauth.DynamicClientID && len(id) <= 256 && utf8.ValidString(id) && strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
