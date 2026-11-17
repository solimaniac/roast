package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
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

// URL omits any retained ID token because the URL may appear in terminal scrollback.
func (a Authorization) URL(redirect string) string {
	challenge := sha256.Sum256([]byte(a.Verifier))
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
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
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
