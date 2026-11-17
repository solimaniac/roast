package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// ErrorCategory returns a fixed category safe to show to the user. Provider
// descriptions and arbitrary codes can contain authorization data.
func ErrorCategory(code string) string {
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
		return ErrorCategory(code)
	}
	var nested struct {
		Code *string `json:"code"`
	}
	if json.Unmarshal(raw, &nested) != nil || nested.Code == nil {
		return "oauth_error"
	}
	return ErrorCategory(*nested.Code)
}

var requestIDPattern = regexp.MustCompile(`^req_[A-Za-z0-9_-]{8,124}$`)

// requestIDForError omits unfamiliar header values instead of echoing provider data.
func requestIDForError(id string) string {
	if !requestIDPattern.MatchString(id) {
		return ""
	}
	return id
}

var ErrAuth = errors.New("ChatGPT authentication required")
var ErrNetwork = errors.New("ChatGPT service unavailable")

type oauthError struct {
	code      string
	status    int
	requestID string
}

func (e *oauthError) Is(target error) bool {
	if e.status >= 500 || e.status == 429 {
		return target == ErrNetwork
	}
	return target == ErrAuth
}
func (e *oauthError) Error() string {
	message := fmt.Sprintf("ChatGPT OAuth request failed (HTTP %d, %s)", e.status, ErrorCategory(e.code))
	if requestID := requestIDForError(e.requestID); requestID != "" {
		message += "; request ID " + requestID
	}
	if errors.Is(e, ErrNetwork) {
		return message + "; retry later"
	}
	return message
}

func InvalidRefreshGrant(err error) bool {
	var failure *oauthError
	if !errors.As(err, &failure) || errors.Is(err, ErrNetwork) {
		return false
	}
	switch failure.code {
	case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
		return true
	default:
		return false
	}
}
