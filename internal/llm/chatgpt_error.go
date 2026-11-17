package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// chatGPTError preserves admission and streaming error details and implements
// Fantasy's retry policy. Usage exhaustion and unsupported requests must not
// spin or silently switch to API-key billing.
type chatGPTError struct {
	StatusCode int
	RequestID  string
	Body       string
	Code       string
	Param      string
}

func newChatGPTError(status int, requestID string, body []byte) *chatGPTError {
	e := &chatGPTError{StatusCode: status, RequestID: requestID, Body: string(body)}
	var wire struct {
		Code  string `json:"code"`
		Param string `json:"param"`
		Error *struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &wire)
	e.Code, e.Param = wire.Code, wire.Param
	if wire.Error != nil {
		e.Code, e.Param = wire.Error.Code, wire.Error.Param
	}
	return e
}

func (e *chatGPTError) Error() string {
	detail := fmt.Sprintf("ChatGPT plan usage failed (HTTP %d, code %q, param %q, request %q): %s", e.StatusCode, e.Code, e.Param, e.RequestID, e.Body)
	switch e.Code {
	case "subscription_sharing_usage_limit_exceeded":
		detail += "; pause new plan-usage requests and check https://chatgpt.com/settings/usage (the reset time and scope of this limit are not known)"
	case "subscription_sharing_user_not_eligible":
		detail += "; this user, workspace, or policy is not eligible; repeating OAuth will not resolve eligibility"
	case "subscription_sharing_unsupported_capability":
		detail += "; remove the unsupported capability rather than retrying the same request"
	default:
		switch e.StatusCode {
		case http.StatusUnauthorized:
			detail += "; check the selected account with roast chatgpt status and reauthorize it with roast chatgpt signin --account CLIENT_ID"
		case http.StatusForbidden:
			detail += "; check ChatGPT plan permission and account/workspace eligibility; if plan permission is missing, request consent with roast chatgpt signin --account CLIENT_ID --enable-plan"
		}
	}
	return detail
}

func (e *chatGPTError) IsRetryable() bool {
	return e.StatusCode == http.StatusServiceUnavailable && (e.Code == "" || e.Code == "subscription_sharing_usage_unavailable" || e.Code == "subscription_sharing_user_unavailable")
}
