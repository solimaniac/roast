package oauth

import (
	"crypto/rand"
	"net/url"

	"golang.org/x/oauth2"
)

const DynamicClientID = "dynamic_agent_client"

type Authorization struct {
	ClientID string
	HostID   string
	State    string
	Nonce    string
	Verifier string
	Consent  bool
}

// NewAuthorization binds a fresh state, OIDC nonce, and PKCE verifier to one sign-in.
func NewAuthorization(clientID, hostID string, consent bool) Authorization {
	return Authorization{
		ClientID: clientID,
		HostID:   hostID,
		Consent:  consent,
		State:    rand.Text(),
		Nonce:    rand.Text(),
		Verifier: oauth2.GenerateVerifier(),
	}
}

// URL omits any retained ID token because the URL may appear in terminal scrollback.
func (a Authorization) URL(redirect string) string {
	query := url.Values{
		"client_id":             {a.ClientID},
		"ext_agent_host_id":     {a.HostID},
		"response_type":         {"code"},
		"redirect_uri":          {redirect},
		"scope":                 {Scope},
		"resource":              {APIBaseURL},
		"state":                 {a.State},
		"nonce":                 {a.Nonce},
		"code_challenge_method": {"S256"},
		"code_challenge":        {oauth2.S256ChallengeFromVerifier(a.Verifier)},
	}
	if a.ClientID == "" {
		query.Set("client_id", DynamicClientID)
		query.Set("agent_name_hint", "Roast")
	}
	if a.Consent {
		query.Set("prompt", "consent")
	}
	return authorizeURL + "?" + query.Encode()
}
