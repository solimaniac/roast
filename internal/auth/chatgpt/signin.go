package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/loopback"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

const signInTimeout = 30 * time.Minute

func signInContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, signInTimeout)
}

// SetSignInDiagnostics enables fixed, value-free sign-in events. Configure it
// before starting sign-in; nil disables diagnostics. A final connection-close
// event may reach w after SignIn returns. Synchronize concurrent reads of w.
func (c *Client) SetSignInDiagnostics(w io.Writer) {
	c.signInDiagnostic = loopback.NewDiagnostics(w)
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

	return c.withAuthorizationLock(ctx, clientID, func() error {
		snapshot, err := c.beginSignIn(ctx, clientID)
		if err != nil {
			return err
		}
		auth := oauth.NewAuthorization(clientID, snapshot.hostID, consent)
		listener, err := loopback.Listen(auth.State, clientID, c.signInDiagnostic)
		if err != nil {
			return err
		}
		defer listener.Close()

		redirect := listener.RedirectURI()
		if err := showURL(auth.URL(redirect)); err != nil {
			return err
		}
		callback, err := listener.Wait(ctx)
		if err != nil {
			return err
		}

		if clientID == "" {
			// Serialize the new ID with reauthorizations before publishing it.
			return c.withAuthorizationLock(ctx, callback.ClientID, func() error {
				return c.finishSignIn(ctx, snapshot, auth, redirect, callback)
			})
		}
		return c.finishSignIn(ctx, snapshot, auth, redirect, callback)
	})
}

type signInSnapshot struct {
	hostID           string
	active           string
	selectionVersion uint64
	credential       credential
}

func (c *Client) beginSignIn(ctx context.Context, clientID string) (signInSnapshot, error) {
	var snapshot signInSnapshot
	err := c.withSessionLock(ctx, func() error {
		i, err := c.loadIndex()
		if err != nil {
			return err
		}
		var previous credential
		if clientID != "" {
			previous, err = c.loadCredential(clientID)
			if err != nil {
				return err
			}
			if previous.Subject == "" && !previous.PendingRegistration {
				return errors.New("ChatGPT registration has no validated identity")
			}
		}
		if err := c.ensureHost(&i); err != nil {
			return err
		}

		// An older writer drops the revision. Write one before waiting so
		// reselecting even the same active account remains detectable.
		if i.SelectionVersion == 0 {
			i.setActive(i.Active)
			if err := c.saveIndex(i); err != nil {
				return err
			}
		}
		// Older writers drop the generation even on an idempotent sign-out.
		if clientID != "" && previous.Generation == "" {
			if err := c.saveCredential(&previous); err != nil {
				return err
			}
		}
		snapshot = signInSnapshot{
			hostID:           i.HostID,
			active:           i.Active,
			selectionVersion: i.SelectionVersion,
			credential:       previous,
		}
		return nil
	})
	return snapshot, err
}

func (c *Client) finishSignIn(ctx context.Context, snapshot signInSnapshot, auth oauth.Authorization, redirect string, callback loopback.Result) error {
	previous, err := c.prepareExchange(ctx, snapshot, callback.ClientID)
	if err != nil {
		return err
	}
	c.signInDiagnostic.Emit("exchange_started")
	tokens, err := c.provider.Exchange(ctx, callback.ClientID, callback.Code, auth.Verifier, redirect)
	if err != nil {
		return fmt.Errorf("%w; retry sign-in with roast chatgpt signin --account %s", err, callback.ClientID)
	}
	receivedAt := c.now()
	identity, err := c.provider.ValidateID(ctx, tokens.IDToken, callback.ClientID, auth.Nonce, c.now)
	if err != nil {
		return fmt.Errorf("%w; retry sign-in with roast chatgpt signin --account %s", err, callback.ClientID)
	}
	if previous.Subject != "" && identity.Subject != previous.Subject {
		return errors.New("ChatGPT account identity changed during reauthorization")
	}

	next := credential{Account: Account{ClientID: callback.ClientID, Subject: identity.Subject, Email: identity.Email}}
	// Anchor the access token's lifetime to receipt of the token response.
	if err := next.applyTokenResponse(tokens, receivedAt); err != nil {
		return err
	}
	if err := c.commitSignIn(ctx, snapshot, previous, next); err != nil {
		return err
	}
	if !next.planEnabled() {
		return fmt.Errorf("signed in, but ChatGPT plan usage is disabled; enable with roast chatgpt signin --account %s --enable-plan, or configure another billing path", callback.ClientID)
	}
	return nil
}

// Recheck the snapshot before consuming a one-use code. For a new client,
// persist its pending registration so the user can retry a failed exchange.
func (c *Client) prepareExchange(ctx context.Context, snapshot signInSnapshot, clientID string) (credential, error) {
	previous := snapshot.credential
	err := c.withSessionLock(ctx, func() error {
		current, err := c.loadSignInIndex(snapshot.hostID)
		if err != nil {
			return err
		}
		if previous.ClientID != "" {
			return c.checkSignInSnapshot(previous)
		}
		if slices.Contains(current.Clients, clientID) {
			return errors.New("ChatGPT registration already exists; reauthorize using --account")
		}
		if _, err := c.loadCredential(clientID); !errors.Is(err, os.ErrNotExist) {
			return errors.New("ChatGPT registration record already exists or is unreadable; reauthorize using --account")
		}
		previous = credential{Account: Account{ClientID: clientID, PendingRegistration: true}}
		if err := c.saveCredential(&previous); err != nil {
			return err
		}
		current.Clients = append(current.Clients, clientID)
		return c.saveIndex(current)
	})
	return previous, err
}

func (c *Client) commitSignIn(ctx context.Context, snapshot signInSnapshot, previous, next credential) error {
	return c.withSessionLock(ctx, func() error {
		current, err := c.loadSignInIndex(snapshot.hostID)
		if err != nil {
			return err
		}
		if err := c.checkSignInSnapshot(previous); err != nil {
			return err
		}
		if err := c.saveCredential(&next); err != nil {
			return err
		}
		if !slices.Contains(current.Clients, next.ClientID) {
			current.Clients = append(current.Clients, next.ClientID)
		}
		// A newer selection, sign-out, or completed sign-in wins.
		if current.SelectionVersion == snapshot.selectionVersion && current.Active == snapshot.active {
			current.setActive(next.ClientID)
		}
		return c.saveIndex(current)
	})
}

func (c *Client) loadSignInIndex(hostID string) (index, error) {
	current, err := c.loadIndex()
	if err != nil {
		return index{}, err
	}
	if current.HostID != hostID {
		return index{}, errors.New("ChatGPT host identity changed during sign-in")
	}
	return current, nil
}

// checkSignInSnapshot runs under the session lock at both exchange and commit.
func (c *Client) checkSignInSnapshot(old credential) error {
	current, err := c.loadCredential(old.ClientID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, old) {
		return errors.New("ChatGPT credentials changed during sign-in; retry sign-in explicitly")
	}
	return nil
}
