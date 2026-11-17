package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestChatGPTRequestPreviewContract(t *testing.T) {
	request := map[string]any{
		"input": []any{map[string]any{"role": "system", "content": "preserve"}},
		"tools": []any{map[string]any{"type": "function", "name": "submit", "parameters": map[string]any{"type": "object", "additionalProperties": false}}},
		"store": true, "stream": false,
	}
	unsupported := []string{"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata", "moderation", "multi_agent", "prompt", "prompt_cache_retention", "safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user", "previous_response_id"}
	for _, field := range unsupported {
		request[field] = "unsupported"
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	prompt := fantasy.Prompt{{Role: fantasy.MessageRoleSystem, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "preserve"}}}}
	got, err := encodeChatGPTRequest(body, prompt)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode transformed request: %v", err)
	}
	for _, field := range unsupported {
		if _, ok := decoded[field]; ok {
			t.Fatalf("unsupported field %s survived", field)
		}
	}
	// Check the wire structure, not substrings that could occur in unrelated fields.
	var contract struct {
		Store   bool
		Stream  bool
		Include []string
		Input   []struct{ Role, Content string }
		Tools   []struct {
			Type, Name string
			Tools      []map[string]any
		}
	}
	if err := json.Unmarshal(got, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Store || !contract.Stream {
		t.Errorf("store=%t stream=%t, want false/true", contract.Store, contract.Stream)
	}
	if !reflect.DeepEqual(contract.Include, []string{"reasoning.encrypted_content"}) {
		t.Errorf("include = %v", contract.Include)
	}
	if len(contract.Input) != 1 || contract.Input[0].Role != "developer" || contract.Input[0].Content != "preserve" {
		t.Errorf("input = %+v, want preserved developer message", contract.Input)
	}
	if len(contract.Tools) != 1 || contract.Tools[0].Type != "namespace" || contract.Tools[0].Name != "roast" {
		t.Fatalf("tools = %+v, want Roast namespace", contract.Tools)
	}
	tools := contract.Tools[0].Tools
	if len(tools) != 1 || !reflect.DeepEqual(tools[0], request["tools"].([]any)[0]) {
		t.Errorf("nested tool schema = %+v, want original function schema", tools)
	}
}

func TestChatGPTRequestRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"null request", `null`},
		{"flat input", `{"input":"flat"}`},
		{"null input", `{"input":null}`},
		{"nonlocal tool", `{"input":[],"tools":[{"type":"tool_search"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := encodeChatGPTRequest([]byte(tc.body), nil); err == nil {
				t.Fatalf("accepted invalid request %s", tc.body)
			}
		})
	}
}

func TestChatGPTUnsafeStreamsNeverDispatch(t *testing.T) {
	good := string(planStream(t, "read", `{}`))
	terminal := strings.Index(good, "event: response.completed")
	if terminal < 0 {
		t.Fatal("missing fixture completion")
	}
	cases := map[string]string{
		"truncated":              good[:terminal],
		"incomplete":             good[:terminal] + "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n",
		"failed-limit":           good[:terminal] + "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\",\"message\":\"limited\"}}}\n\n",
		"explicit-error":         good[:terminal] + "data: {\"type\":\"error\",\"code\":\"subscription_sharing_usage_unavailable\"}\n\n",
		"wrong-namespace":        strings.ReplaceAll(good, `"namespace":"roast"`, `"namespace":"shell"`),
		"missing-namespace":      strings.ReplaceAll(good, `"namespace":"roast",`, ""),
		"wrong-completed-status": strings.ReplaceAll(good, `"status":"completed"`, `"status":"failed"`),
		"done-without-complete":  good[:terminal] + "data: [DONE]\n\n",
		"invalid-message-role":   good[:terminal] + "data: " + `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"developer"}]}}` + "\n\n",
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			requests := 0
			c := planTestClient(t, ClientConfig{MaxAttempts: 3}, planRoundTripper(func(*http.Request) (*http.Response, error) {
				requests++
				return planResponse([]byte(stream)), nil
			}))
			var out struct{ Answer string }
			tools := 0
			err := c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
				tools++
				return "wrong", nil
			})}, &out)
			if err == nil || tools != 0 || requests != 1 {
				t.Fatalf("err=%v tools=%d requests=%d", err, tools, requests)
			}
			if name == "failed-limit" && !strings.Contains(err.Error(), "settings/usage") {
				t.Fatalf("no usage recovery: %v", err)
			}
		})
	}
}

func TestChatGPTAdmissionPreservesErrorAndDoesNotRetryLimit(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{429, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","param":"model","message":"limited"}}`},
		{401, `{"detail":"permission missing"}`},
		{403, `{"error":{"code":"subscription_sharing_user_not_eligible"}}`},
		{400, `{"error":{"code":"subscription_sharing_unsupported_capability","param":"tools"}}`},
		{302, `{"detail":"do not redirect"}`},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			requests := 0
			c := planTestClient(t, ClientConfig{MaxAttempts: 3}, planRoundTripper(func(*http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"X-Request-Id": {"req_test"}, "Location": {"https://wrong.invalid"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			var out struct{ Answer string }
			err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			if err == nil || requests != 1 || !strings.Contains(err.Error(), "req_test") || !strings.Contains(err.Error(), tc.body) {
				t.Fatalf("requests=%d error=%v", requests, err)
			}
			if (tc.status == 401 || tc.status == 403) && !errors.Is(err, ErrAuth) {
				t.Fatalf("auth status not classified: %v", err)
			}
		})
	}
}

func TestChatGPTAuthenticationRecoveryDoesNotSuggestAPIKeys(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		recovery string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"detail":"permission missing"}`, "roast chatgpt signin --account CLIENT_ID"},
		{"forbidden", http.StatusForbidden, `{"detail":"permission missing"}`, "roast chatgpt signin --account CLIENT_ID --enable-plan"},
		{"ineligible-401", http.StatusUnauthorized, `{"error":{"code":"subscription_sharing_user_not_eligible"}}`, "repeating OAuth will not resolve eligibility"},
		{"ineligible-403", http.StatusForbidden, `{"error":{"code":"subscription_sharing_user_not_eligible"}}`, "repeating OAuth will not resolve eligibility"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			c := planTestClient(t, ClientConfig{MaxAttempts: 3}, planRoundTripper(func(*http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"X-Request-Id": {"req_auth"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			var out struct{ Answer string }
			err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			if !errors.Is(err, ErrAuth) || requests != 1 {
				t.Fatalf("requests=%d error=%v, want one attempt and ErrAuth", requests, err)
			}
			var planErr *chatGPTError
			if !errors.As(err, &planErr) || planErr.StatusCode != tc.status || planErr.RequestID != "req_auth" || planErr.Body != tc.body {
				t.Fatalf("lost typed ChatGPT error details: %v", err)
			}
			if !strings.Contains(err.Error(), tc.recovery) || strings.Contains(err.Error(), "API key") {
				t.Fatalf("wrong ChatGPT recovery advice: %v", err)
			}
			if strings.Contains(tc.name, "ineligible") && strings.Contains(err.Error(), "roast chatgpt signin") {
				t.Fatalf("eligibility failure suggests repeating OAuth: %v", err)
			}
		})
	}
}

func TestChatGPTRejectsOverridesAndUnexpectedRoutes(t *testing.T) {
	for _, cfg := range []ClientConfig{{APIKey: "secret"}, {BaseURL: "https://chatgpt.com/backend-api/codex"}, {SessionHeader: "X-Session"}} {
		cfg.Protocol, cfg.Model, cfg.MaxOutputTokens = "chatgpt", "test", 100
		if _, err := New(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "not accepted") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid config error=%v", err)
		}
	}
	transport := &chatGPTTransport{token: func(context.Context) (string, error) { t.Fatal("retrieved token for wrong endpoint"); return "", nil }}
	for _, endpoint := range []string{"https://wrong.invalid/v1/responses", chatGPTBaseURL + "/models", chatGPTBaseURL + "/responses?query=1"} {
		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.RoundTrip(req); err == nil {
			t.Fatal("accepted wrong endpoint")
		}
	}
}

func TestChatGPTRejectsOversizedRestoredHistoryBeforeCredentials(t *testing.T) {
	output := &chatGPTOutput{items: []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"reason-large","summary":[],"encrypted_content":"` + strings.Repeat("x", defaultInputByteCeiling*4) + `","status":"completed"}`),
	}}
	state := &chatGPTCall{prompt: fantasy.Prompt{{
		Role: fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{
			Text:            "small SDK history",
			ProviderOptions: fantasy.ProviderOptions{chatGPTOutputKey: output},
		}},
	}}}
	owner := &responseBody{}
	ctx := context.WithValue(t.Context(), chatGPTCallKey{}, state)
	ctx = context.WithValue(ctx, responseBodyKey{}, owner)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatGPTBaseURL+"/responses", strings.NewReader(`{"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	transport := &chatGPTTransport{
		token: func(context.Context) (string, error) {
			t.Fatal("retrieved credential for oversized restored history")
			return "", nil
		},
		base: planRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Fatal("sent oversized restored history")
			return nil, nil
		}),
	}
	resp, err := transport.RoundTrip(req)
	if resp != nil || err == nil || !strings.Contains(err.Error(), "exceeds the bounded input size") || owner.failure != err {
		t.Fatalf("response=%v error=%v retained failure=%v", resp, err, owner.failure)
	}
}

func TestChatGPTStreamAllowsCommentsCRLFAndMultilineData(t *testing.T) {
	stream := ": keepalive\r\n\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"status\":\"completed\"}}\r\n\r\n"
	b := &chatGPTStreamBody{body: io.NopCloser(strings.NewReader(stream)), reader: bufio.NewReader(strings.NewReader(stream)), fail: func(e error) error { return e }}
	got, err := io.ReadAll(b)
	if err != nil || string(got) != stream || !b.completed {
		t.Fatalf("completed=%v error=%v stream=%q", b.completed, err, got)
	}
}

func TestChatGPTRejectsNonStreamingSuccess(t *testing.T) {
	c := planTestClient(t, ClientConfig{}, planRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"completed"}`))}, nil
	}))
	var out struct{ Answer string }
	if err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out); err == nil || !strings.Contains(err.Error(), "non-streaming") {
		t.Fatalf("error=%v", err)
	}
}
