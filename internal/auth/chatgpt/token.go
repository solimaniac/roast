package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

const planScope = "chatgpt.tokens.use.direct"

func (v credential) planEnabled() bool {
	scopes := strings.Fields(v.Scopes)
	return slices.Contains(scopes, planScope) && slices.Contains(scopes, "resource.invoke")
}

func (v *credential) apply(tokens oauth.TokenResponse, receivedAt time.Time) error {
	scope := ""
	if tokens.Scope != nil {
		scope = *tokens.Scope
	}
	if tokens.AccessToken == "" {
		if tokens.IDToken == "" {
			return errors.New("incomplete ChatGPT token response")
		}
	} else if !strings.EqualFold(tokens.TokenType, "Bearer") || tokens.ExpiresIn <= 0 || tokens.ExpiresIn > 86400 {
		return errors.New("incomplete ChatGPT token response")
	}
	if slices.Contains(strings.Fields(scope), planScope) && (tokens.AccessToken == "" || tokens.RefreshToken == "") {
		return errors.New("incomplete ChatGPT plan token response")
	}

	v.AccessToken = tokens.AccessToken
	v.RefreshToken = tokens.RefreshToken
	v.Scopes = scope
	v.ExpiresAt = receivedAt.Add(time.Duration(tokens.ExpiresIn) * time.Second)
	v.EarliestRefreshAt = int64(tokens.EarliestRefreshAt)
	if tokens.IDToken != "" {
		v.IDToken = tokens.IDToken
	}
	return nil
}

func (c *Client) Token(ctx context.Context) (string, error) {
	if c.selected == "" {
		return "", fmt.Errorf("%w: no account selected; run roast chatgpt signin", ErrAuth)
	}
	var token string
	err := c.withSessionLock(ctx, func() error {
		var err error
		token, err = c.selectedToken(ctx)
		return err
	})
	if err != nil && !errors.Is(err, ErrNetwork) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("%w: %w", ErrAuth, err)
	}
	return token, err
}

// selectedToken runs under the session lock to serialize refresh rotation.
func (c *Client) selectedToken(ctx context.Context) (string, error) {
	v, err := c.loadCredential(c.selected)
	if err != nil {
		return "", err
	}
	if v.Pending != nil {
		if err := c.acceptPending(ctx, &v); err != nil {
			return "", err
		}
	}
	if v.AccessToken == "" {
		return "", errors.New("ChatGPT account is signed out; run roast chatgpt signin --account CLIENT_ID")
	}
	if !v.planEnabled() {
		return "", errors.New("ChatGPT plan usage scope is missing; sign in again")
	}

	now := c.now()
	if now.Before(v.ExpiresAt.Add(-time.Minute)) || now.Unix() < v.EarliestRefreshAt {
		if !now.Before(v.ExpiresAt) {
			return "", errors.New("ChatGPT token expired before refresh is permitted; retry after earliest_refresh_at")
		}
		return v.AccessToken, nil
	}
	return c.refresh(ctx, v)
}

func (c *Client) refresh(ctx context.Context, v credential) (string, error) {
	tokens, err := c.provider.Refresh(ctx, v.ClientID, v.RefreshToken)
	if err != nil {
		if oauth.InvalidRefreshGrant(err) {
			v.clearTokens()
			if err := c.saveCredential(v); err != nil {
				return "", err
			}
		}
		return "", err
	}
	// Rotation has consumed the old refresh token. Persist the new response
	// before fetching JWKS so a later call can retry validation.
	v.Pending = &pendingRefresh{Response: tokens, ReceivedAt: c.now()}
	if err := c.saveCredential(v); err != nil {
		// The provider may have consumed the old refresh token. Best-effort
		// invalidation prevents its reuse if storage becomes writable again now.
		return "", c.rejectPending(&v, fmt.Errorf("ChatGPT refresh completed but its replacement could not be durably saved: %w", err))
	}
	if err := c.acceptPending(ctx, &v); err != nil {
		return "", err
	}

	if !v.planEnabled() {
		return "", errors.New("ChatGPT plan usage permission was removed; enable it explicitly before inference")
	}
	// Keep the rotated credentials if validation outlasted the access token.
	if !c.now().Before(v.ExpiresAt) {
		return "", fmt.Errorf("%w: refreshed token expired during validation; retry", ErrNetwork)
	}
	return v.AccessToken, nil
}

func (c *Client) acceptPending(ctx context.Context, v *credential) error {
	pending := v.Pending
	receivedAt := pending.ReceivedAt
	if receivedAt.IsZero() || receivedAt.Unix() <= 0 || receivedAt.After(c.now().Add(5*time.Second)) {
		return c.rejectPending(v, errors.New("invalid pending ChatGPT token receipt time"))
	}

	response := pending.Response
	// Reject structurally invalid replacements even when identity keys are offline.
	next := *v
	// Only an omitted scope inherits the prior grant; an explicit empty grant revokes it.
	if response.Scope == nil {
		response.Scope = &v.Scopes
	}
	if err := next.apply(response, receivedAt); err != nil {
		return c.rejectPending(v, err)
	}
	if response.IDToken != "" {
		// Validate identity at receipt so a JWKS outage can outlast the ID
		// token. Access-token expiry is still checked against the live clock.
		identity, err := c.provider.ValidateID(ctx, response.IDToken, v.ClientID, "", func() time.Time { return receivedAt })
		if err != nil {
			if errors.Is(err, oauth.ErrInvalidID) {
				return c.rejectPending(v, err)
			}
			return err
		}
		if identity.Subject != v.Subject {
			return c.rejectPending(v, errors.New("refreshed ChatGPT identity changed"))
		}
	}
	next.Pending = nil
	if err := c.saveCredential(next); err != nil {
		return fmt.Errorf("could not persist validated ChatGPT credentials; repair credential storage and retry: %w", err)
	}
	*v = next
	return nil
}

// rejectPending runs under the session lock. Neither the rejected replacement
// nor the old, possibly consumed refresh token may be used after this point.
// Identity and registration remain available for explicit reauthorization.
func (c *Client) rejectPending(v *credential, cause error) error {
	v.clearTokens()
	v.Pending = nil
	v.Scopes = ""
	v.ExpiresAt = time.Time{}
	v.EarliestRefreshAt = 0
	if err := c.saveCredential(*v); err != nil {
		return fmt.Errorf("%w; could not confirm local credential cleanup (%v); repair credential storage, then run roast chatgpt signin --account CLIENT_ID before retrying", cause, err)
	}
	return fmt.Errorf("%w; local credentials cleared; run roast chatgpt signin --account CLIENT_ID", cause)
}
