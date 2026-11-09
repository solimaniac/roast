package chatgpt

import (
	"encoding/json"
	"strings"
)

// Only fixed categories cross the user-visible error boundary. Provider error
// descriptions and arbitrary code values can contain authorization data.
func safeOAuthCategory(code string) string {
	switch code {
	case "access_denied", "invalid_request", "invalid_client", "unauthorized_client",
		"invalid_scope", "invalid_resource", "unsupported_grant_type", "unsupported_response_type",
		"invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired",
		"refresh_token_invalidated", "refresh_token_reused", "invalid_token", "invalid_api_key",
		"server_error", "temporarily_unavailable", "rate_limit_exceeded":
		return code
	default:
		return "oauth_error"
	}
}

func oauthResponseCategory(raw json.RawMessage) string {
	var code string
	if json.Unmarshal(raw, &code) == nil {
		return safeOAuthCategory(code)
	}
	var nested struct {
		Code *string `json:"code"`
	}
	if json.Unmarshal(raw, &nested) != nil || nested.Code == nil {
		return "oauth_error"
	}
	return safeOAuthCategory(*nested.Code)
}

// OpenAI's OAuth SDK reads x-request-id. Accept only its recognizable req_
// identifier form, with a bounded ASCII suffix; omit unfamiliar values rather
// than echo arbitrary header text. Never obtain identifiers from callback data.
func safeOAuthRequestID(id string) string {
	if !strings.HasPrefix(id, "req_") || len(id) < 12 || len(id) > 128 {
		return ""
	}
	for _, ch := range id[4:] {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' {
			continue
		}
		return ""
	}
	return id
}
