package chatgpt

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

// A credential belongs to one registration. Its identity survives sign-out.
type credential struct {
	Account
	Generation        string          `json:"generation,omitempty"`
	IDToken           string          `json:"id_token,omitempty"`
	AccessToken       string          `json:"access_token,omitempty"`
	RefreshToken      string          `json:"refresh_token,omitempty"`
	Scopes            string          `json:"scope,omitempty"`
	EarliestRefreshAt int64           `json:"earliest_refresh_at,omitempty"`
	Pending           *pendingRefresh `json:"pending_refresh,omitempty"`
}

// A refresh can rotate the token before the identity keys are available.
// Keep the response and its receipt time together until validation succeeds.
type pendingRefresh struct {
	Response   oauth.TokenResponse `json:"response"`
	ReceivedAt time.Time           `json:"received_at"`
}

const planScope = "chatgpt.tokens.use.direct"

func (v credential) signedIn() bool {
	return v.AccessToken != "" || v.IDToken != ""
}

func (v credential) planEnabled() bool {
	scopes := strings.Fields(v.Scopes)
	return v.AccessToken != "" && slices.Contains(scopes, planScope) && slices.Contains(scopes, "resource.invoke")
}

func (v *credential) clearGrant() {
	v.IDToken = ""
	v.AccessToken = ""
	v.RefreshToken = ""
	v.Scopes = ""
	v.ExpiresAt = time.Time{}
	v.EarliestRefreshAt = 0
	v.Pending = nil
}

func (v *credential) applyTokenResponse(tokens oauth.TokenResponse, receivedAt time.Time) error {
	scope := ""
	if tokens.Scope != nil {
		scope = *tokens.Scope
	}
	switch {
	case tokens.AccessToken == "" && tokens.IDToken == "":
		return errors.New("incomplete ChatGPT token response")
	case tokens.AccessToken != "" && (!strings.EqualFold(tokens.TokenType, "Bearer") || tokens.ExpiresIn <= 0 || tokens.ExpiresIn > 86400):
		return errors.New("incomplete ChatGPT token response")
	case slices.Contains(strings.Fields(scope), planScope) && (tokens.AccessToken == "" || tokens.RefreshToken == ""):
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
