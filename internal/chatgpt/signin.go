package chatgpt

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// signInTimeout bounds browser interaction while allowing time for a user to
// complete a secure login handoff. Earlier caller deadlines and cancellation win.
const signInTimeout = 30 * time.Minute

func signInContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, signInTimeout)
}

// SetSignInDiagnostics enables fixed, value-free sign-in events. Configure it
// before starting sign-in; nil disables diagnostics. It never logs request data.
func (c *Client) SetSignInDiagnostics(w io.Writer) {
	if w == nil {
		c.signInDiagnostic = nil
		return
	}
	var mu sync.Mutex
	c.signInDiagnostic = func(event string) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintln(w, "chatgpt_signin:"+event)
	}
}

func (c *Client) signInEvent(event string) {
	// Keep this allowlist at the output boundary: even an accidental dynamic
	// argument must never expose authorization data.
	switch event {
	case "listener_ready", "listener_failed", "rejected_method", "rejected_path", "rejected_host", "rejected_state", "rejected_duplicate", "callback_received", "callback_failed", "exchange_started", "canceled", "timed_out":
		if c.signInDiagnostic != nil {
			c.signInDiagnostic(event)
		}
	}
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

// SignIn performs an explicit user-initiated authorization. An empty clientID
// registers a new account; a saved ID reauthorizes that registration. showURL
// receives a browser URL containing no saved ID token or other credential.
func (c *Client) SignIn(ctx context.Context, clientID string, showURL func(string) error) error {
	return c.signIn(ctx, clientID, false, showURL)
}

// EnablePlan explicitly requests renewed consent for an existing registration.
func (c *Client) EnablePlan(ctx context.Context, clientID string, showURL func(string) error) error {
	if clientID == "" {
		return errors.New("enabling plan usage requires an existing --account CLIENT_ID")
	}
	return c.signIn(ctx, clientID, true, showURL)
}

func (c *Client) signIn(ctx context.Context, clientID string, consent bool, showURL func(string) error) error {
	ctx, cancel := signInContext(ctx)
	defer cancel()
	return c.authorizationLocked(ctx, clientID, func() error {
		var i index
		var old credential
		if err := c.locked(ctx, func() error {
			var e error
			i, e = c.loadIndex()
			if e != nil {
				return e
			}
			if clientID != "" {
				old, e = c.load(clientID)
				if e != nil {
					return e
				}
				if old.Subject == "" && !old.PendingRegistration {
					return errors.New("ChatGPT registration has no validated identity")
				}
			}
			if e = c.ensureHost(&i); e != nil {
				return e
			}
			// An older writer drops this field, so even reselecting the same active
			// account remains detectable while this authorization is pending.
			if i.SelectionVersion == 0 {
				if e = i.setActive(i.Active); e != nil {
					return e
				}
				if e = c.saveIndex(i); e != nil {
					return e
				}
			}
			// Upgrade legacy snapshots before releasing the lock. An older binary
			// drops this field when writing, even on an otherwise idempotent sign-out.
			if clientID != "" && old.Generation == "" {
				if e = c.save(old); e != nil {
					return e
				}
				old, e = c.load(clientID)
			}
			return e
		}); err != nil {
			return err
		}
		listener, e := net.Listen("tcp4", "127.0.0.1:0")
		if e != nil {
			return errors.New("cannot open ChatGPT loopback callback")
		}
		defer listener.Close()
		redirect := "http://" + listener.Addr().String() + "/auth/callback"
		state, e := randomValue()
		if e != nil {
			return e
		}
		nonce, e := randomValue()
		if e != nil {
			return e
		}
		verifier, e := randomValue()
		if e != nil {
			return e
		}
		challenge := sha256.Sum256([]byte(verifier))
		pendingID := clientID
		if pendingID == "" {
			pendingID = "dynamic_agent_client"
		}
		q := url.Values{"client_id": {pendingID}, "ext_agent_host_id": {i.HostID}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {Scope}, "resource": {APIBaseURL}, "state": {state}, "nonce": {nonce}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}}
		if clientID == "" {
			q.Set("agent_name_hint", "Roast")
		}
		if consent {
			q.Set("prompt", "consent")
		}
		// Deliberately omit retained id_token_hint from a CLI URL that can appear in
		// terminal scrollback. OpenAI's account selector handles returning sign-ins.
		type callback struct {
			code, id string
			err      error
		}
		result := make(chan callback, 1)
		var once sync.Once
		server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Security-Policy", "default-src 'none'")
			w.Header().Set("Referrer-Policy", "no-referrer")
			rejection := ""
			switch {
			case r.Method != http.MethodGet:
				rejection = "rejected_method"
			case r.URL.Path != "/auth/callback":
				rejection = "rejected_path"
			case r.Host != listener.Addr().String():
				rejection = "rejected_host"
			}
			if rejection != "" {
				c.signInEvent(rejection)
				http.NotFound(w, r)
				return
			}
			values, err := url.ParseQuery(r.URL.RawQuery)
			if err != nil || len(values["state"]) != 1 || subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(state)) != 1 {
				c.signInEvent("rejected_state")
				http.Error(w, "Invalid authorization state", 400)
				return
			}
			accepted := false
			once.Do(func() {
				accepted = true
				c.signInEvent("callback_received")
				v := callback{code: values.Get("code"), id: values.Get("client_id")}
				switch {
				case values.Get("error") != "":
					v.err = errors.New("ChatGPT authorization was declined or failed")
				case len(values["code"]) != 1 || v.code == "" || len(values["client_id"]) > 1:
					v.err = errors.New("incomplete ChatGPT callback")
				case clientID == "" && !validIssuedClientID(v.id):
					v.err = errors.New("missing issued ChatGPT client ID")
				case clientID != "" && v.id != "" && v.id != clientID:
					v.err = errors.New("ChatGPT registration changed during sign-in")
				}
				if clientID != "" {
					v.id = clientID
				}
				result <- v
			})
			if !accepted {
				c.signInEvent("rejected_duplicate")
				http.Error(w, "Authorization already received", 400)
				return
			}
			_, _ = w.Write([]byte("Authorization received. Return to Roast to check the result."))
		})}
		serveDone := serveCallback(server, listener)
		defer closeCallback(server)
		c.signInEvent("listener_ready")
		if e = showURL(authorizeURL + "?" + q.Encode()); e != nil {
			return e
		}
		var cb callback
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				c.signInEvent("timed_out")
			} else {
				c.signInEvent("canceled")
			}
			return ctx.Err()
		case err := <-serveDone:
			c.signInEvent("listener_failed")
			return err
		case cb = <-result:
		}
		if cb.err != nil {
			c.signInEvent("callback_failed")
			return cb.err
		}
		finish := func() error {
			// Recheck under the store lock before consuming a one-use code. A browser
			// wait must not undo a sign-out or overwrite a token refreshed meanwhile.
			if e = c.locked(ctx, func() error {
				current, err := c.loadIndex()
				if err != nil {
					return err
				}
				if current.HostID != i.HostID {
					return errors.New("ChatGPT host identity changed during sign-in")
				}
				if clientID != "" {
					return c.checkSignInSnapshot(old)
				}
				// The callback identifies a newly issued client, not an authenticated
				// user. Retain a pending mapping so failed exchanges can be retried.
				if slices.Contains(current.Clients, cb.id) {
					return errors.New("ChatGPT registration already exists; reauthorize using --account")
				}
				if _, loadErr := c.load(cb.id); !errors.Is(loadErr, os.ErrNotExist) {
					return errors.New("ChatGPT registration record already exists or is unreadable; reauthorize using --account")
				}
				pending := credential{Account: Account{ClientID: cb.id, PendingRegistration: true}}
				if err = c.save(pending); err != nil {
					return err
				}
				old, err = c.load(cb.id)
				if err != nil {
					return err
				}
				current.Clients = append(current.Clients, cb.id)
				return c.saveIndex(current)
			}); e != nil {
				return e
			}
			var t tokenResponse
			c.signInEvent("exchange_started")
			if e = c.request(ctx, http.MethodPost, tokenURL, url.Values{"grant_type": {"authorization_code"}, "client_id": {cb.id}, "code": {cb.code}, "code_verifier": {verifier}, "redirect_uri": {redirect}, "resource": {APIBaseURL}}, "", &t); e != nil {
				return fmt.Errorf("%w; retry sign-in with roast chatgpt signin --account %s", e, cb.id)
			}
			receivedAt := c.now()
			identity, e := c.validateID(ctx, t.IDToken, cb.id, nonce)
			if e != nil {
				return fmt.Errorf("%w; retry sign-in with roast chatgpt signin --account %s", e, cb.id)
			}
			if clientID != "" && old.Subject != "" && identity.Subject != old.Subject {
				return errors.New("ChatGPT account identity changed during reauthorization")
			}

			v := credential{Account: Account{ClientID: cb.id, Subject: identity.Subject, Email: identity.Email}}
			if e = c.apply(&v, t); e != nil {
				return e
			}
			// JWKS validation must not extend the token's actual usable lifetime.
			v.ExpiresAt = receivedAt.Add(time.Duration(t.ExpiresIn) * time.Second)
			if e = c.locked(ctx, func() error {
				current, err := c.loadIndex()
				if err != nil {
					return err
				}
				if current.HostID != i.HostID {
					return errors.New("ChatGPT host identity changed during sign-in")
				}
				if err = c.checkSignInSnapshot(old); err != nil {
					return err
				}
				if err = c.save(v); err != nil {
					return err
				}
				if !slices.Contains(current.Clients, cb.id) {
					current.Clients = append(current.Clients, cb.id)
				}
				// A newer explicit selection (even reselecting the same account),
				// sign-out, or completed sign-in wins over this browser flow.
				if current.SelectionVersion == i.SelectionVersion && current.Active == i.Active {
					if err = current.setActive(cb.id); err != nil {
						return err
					}
				}
				return c.saveIndex(current)
			}); e != nil {
				return e
			}
			if !hasScope(v.Scopes, "chatgpt.tokens.use.direct") || !hasScope(v.Scopes, "resource.invoke") {
				return fmt.Errorf("signed in, but ChatGPT plan usage is disabled; enable with roast chatgpt signin --account %s --enable-plan, or configure another billing path", cb.id)
			}
			return nil
		}
		if clientID == "" {
			// A new registration has no ID until the callback. Lock that ID too
			// before publishing its pending record for an explicit retry.
			return c.authorizationLocked(ctx, cb.id, finish)
		}
		return finish()
	})
}

// checkSignInSnapshot runs under the session lock at both exchange and commit.
func (c *Client) checkSignInSnapshot(old credential) error {
	current, err := c.load(old.ClientID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, old) {
		return errors.New("ChatGPT credentials changed during sign-in; retry sign-in explicitly")
	}
	return nil
}

// Issued client IDs are opaque. The documented example prefix is not a format
// contract; reject only the registration placeholder and malformed values.
func validIssuedClientID(id string) bool {
	return id != "" && id != "dynamic_agent_client" && len(id) <= 256 && utf8.ValidString(id) && strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
