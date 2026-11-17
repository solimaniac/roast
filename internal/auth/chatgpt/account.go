package chatgpt

import (
	"context"
	"errors"
	"os"
	"time"
)

// Account is the registration information that can be shown by the CLI.
type Account struct {
	PendingRegistration bool      `json:"pending_registration,omitempty"`
	ClientID            string    `json:"client_id"`
	Subject             string    `json:"subject"`
	Email               string    `json:"email,omitempty"`
	Active              bool      `json:"active"`
	SignedIn            bool      `json:"signed_in"`
	PlanEnabled         bool      `json:"plan_enabled"`
	ExpiresAt           time.Time `json:"expires_at"`
}

// Status reads the saved registrations and their current local grant state.
func (c *Client) Status(ctx context.Context) ([]Account, error) {
	if _, err := os.Lstat(c.dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var out []Account
	err := c.withSessionLock(ctx, func() error {
		i, err := c.loadIndex()
		if err != nil {
			return err
		}
		for _, id := range i.Clients {
			v, err := c.loadCredential(id)
			if err != nil {
				return err
			}
			a := v.Account
			a.Active = id == i.Active
			a.SignedIn = v.signedIn()
			a.PlanEnabled = v.planEnabled()
			out = append(out, a)
		}
		return nil
	})
	return out, err
}

// Select chooses the registration that future clients will use.
func (c *Client) Select(ctx context.Context, id string) error {
	return c.withSessionLock(ctx, func() error {
		i, err := c.loadIndex()
		if err != nil {
			return err
		}
		v, err := c.loadCredential(id)
		if err != nil {
			return err
		}
		if !v.signedIn() {
			return errors.New("account is signed out; run roast chatgpt signin --account CLIENT_ID")
		}
		i.setActive(id)
		return c.saveIndex(i)
	})
}

// SignOut attempts revocation and clears the local grant. An empty ID selects
// the active registration.
func (c *Client) SignOut(ctx context.Context, id string) error {
	return c.withSessionLock(ctx, func() error {
		i, err := c.loadIndex()
		if err != nil {
			return err
		}
		if id == "" {
			id = i.Active
		}
		if id == "" {
			return errors.New("no active ChatGPT account")
		}
		v, err := c.loadCredential(id)
		if err != nil {
			return err
		}

		refreshToken := v.RefreshToken
		if v.Pending != nil && v.Pending.Response.RefreshToken != "" {
			refreshToken = v.Pending.Response.RefreshToken
		}
		var remoteErr error
		if refreshToken != "" {
			remoteErr = c.provider.Revoke(ctx, id, refreshToken)
		}

		v.clearGrant()
		if err := c.saveCredential(&v); err != nil {
			return err
		}
		if i.Active == id {
			i.setActive("")
			if err := c.saveIndex(i); err != nil {
				return err
			}
		}
		if remoteErr != nil {
			return errors.New("signed out locally; remote revocation was not confirmed. Disconnect Roast in ChatGPT Settings")
		}
		return nil
	})
}
