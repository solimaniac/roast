package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/bluesky-social/roast/internal/chatgpt"
	"github.com/bluesky-social/roast/internal/testutil"
)

type planRoundTripper func(*http.Request) (*http.Response, error)

func (f planRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func planStream(t *testing.T, tool, arguments string) []byte {
	t.Helper()
	response := structuredToolResponse("responses", tool, arguments)
	for _, raw := range response["output"].([]any) {
		raw.(map[string]any)["namespace"] = chatGPTToolNamespace
	}
	w := httptest.NewRecorder()
	if err := testutil.WriteInferenceStream(w, "responses", response); err != nil {
		t.Fatal(err)
	}
	return w.Body.Bytes()
}

func planResponse(body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(bytes.NewReader(body))}
}

func planTestClient(t *testing.T, cfg ClientConfig, transport http.RoundTripper) Client {
	t.Helper()
	cfg.Protocol, cfg.Model, cfg.MaxOutputTokens = "chatgpt", "gpt-6.1-sol", 1000
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 1
	}
	c, err := chatGPTClient(t.Context(), cfg, func(context.Context) (string, error) { return "oauth-only", nil }, transport)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChatGPTNativeToolLoop(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-key")
	t.Setenv("OPENAI_BASE_URL", "https://wrong.invalid/v1")
	t.Setenv("OPENAI_ORG_ID", "ambient-org")
	t.Setenv("OPENAI_PROJECT_ID", "ambient-project")
	t.Setenv("OPENAI_CUSTOM_HEADERS", "Authorization: wrong\nX-Ambient: private")
	var requests []map[string]any
	var captures []RawResponse
	var expectedCapture [][]byte
	c := planTestClient(t, ClientConfig{CaptureResponse: func(r RawResponse) error {
		captures = append(captures, r)
		return nil
	}}, planRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != chatGPTBaseURL+"/responses" || r.Header.Get("Authorization") != "Bearer oauth-only" {
			t.Fatal("wrong destination or credential")
		}
		for _, h := range []string{"OpenAI-Organization", "OpenAI-Project", "X-Ambient"} {
			if r.Header.Get(h) != "" {
				t.Fatalf("ambient header %s leaked", h)
			}
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
		tool, args := "read", `{}`
		if len(requests) > 1 {
			tool, args = "submit", `{"answer":"done"}`
		}
		body := planStream(t, tool, args)
		expectedCapture = append(expectedCapture, body)
		return planResponse(body), nil
	}))
	var output struct{ Answer string }
	reads := 0
	err := c.Converse(t.Context(), "Exact Roast system prompt", "Review the diff", ConverseOpts{
		MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
			reads++
			if len(captures) != 1 {
				t.Error("tool dispatched before raw capture")
			}
			return "source file content", nil
		}),
	}, &output)
	if err != nil || output.Answer != "done" || reads != 1 || len(requests) != 2 {
		t.Fatalf("output=%+v reads=%d requests=%d err=%v", output, reads, len(requests), err)
	}
	for i, req := range requests {
		if req["store"] != false || req["stream"] != true || req["tool_choice"] != "auto" {
			t.Fatalf("wrong request controls: %#v", req)
		}
		if _, exists := req["max_output_tokens"]; exists {
			t.Fatal("unsupported output limit sent")
		}
		if _, exists := req["previous_response_id"]; exists {
			t.Fatal("server-side history sent")
		}
		input, _ := json.Marshal(req["input"])
		if !bytes.Contains(input, []byte("Exact Roast system prompt")) || !bytes.Contains(input, []byte(`"role":"developer"`)) {
			t.Fatalf("prompt not preserved as developer message: %s", input)
		}
		if i == 1 && (!bytes.Contains(input, []byte("source file content")) || !bytes.Contains(input, []byte(`"namespace":"roast"`))) {
			t.Fatalf("tool history not replayed: %s", input)
		}
		namespaces := req["tools"].([]any)
		if len(namespaces) != 1 || namespaces[0].(map[string]any)["name"] != "roast" || namespaces[0].(map[string]any)["type"] != "namespace" {
			t.Fatalf("tools not namespaced: %#v", namespaces)
		}
		if captures[i].Protocol != "chatgpt" || !bytes.Equal(captures[i].Body, expectedCapture[i]) {
			t.Fatal("raw capture changed the server stream")
		}
	}
}

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
	body, _ := json.Marshal(request)
	got, err := chatGPTRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(got, &decoded)
	for _, field := range unsupported {
		if _, ok := decoded[field]; ok {
			t.Fatalf("unsupported field %s survived", field)
		}
	}
	if !bytes.Contains(got, []byte(`"additionalProperties":false`)) {
		t.Fatal("tool schema lost constraints")
	}
	for _, bad := range []string{`null`, `{"input":"flat"}`, `{"input":null}`, `{"input":[],"tools":[{"type":"tool_search"}]}`} {
		if _, err := chatGPTRequest([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
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
		if _, err := New(t.Context(), cfg); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid config error=%v", err)
		}
	}
	transport := &chatGPTTransport{token: func(context.Context) (string, error) { t.Fatal("retrieved token for wrong endpoint"); return "", nil }}
	for _, endpoint := range []string{"https://wrong.invalid/v1/responses", chatGPTBaseURL + "/models", chatGPTBaseURL + "/responses?query=1"} {
		req, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader("{}"))
		if _, err := transport.RoundTrip(req); err == nil {
			t.Fatal("accepted wrong endpoint")
		}
	}
}

func TestChatGPTRejectsOversizedRestoredHistoryBeforeCredentials(t *testing.T) {
	output := &chatGPTOutput{Items: []json.RawMessage{
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

func TestChatGPTCredentialFailure(t *testing.T) {
	c, err := chatGPTClient(t.Context(), ClientConfig{Protocol: "chatgpt", Model: "test", MaxOutputTokens: 100}, func(context.Context) (string, error) {
		return "", fmt.Errorf("%w: plan permission missing", chatgpt.ErrAuth)
	}, planRoundTripper(func(*http.Request) (*http.Response, error) { t.Fatal("request sent without consent"); return nil, nil }))
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Answer string }
	if err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong credential error: %v", err)
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

func TestChatGPTUsageCountsCachedInputOnce(t *testing.T) {
	c := planTestClient(t, ClientConfig{}, planRoundTripper(func(*http.Request) (*http.Response, error) {
		response := structuredToolResponse("responses", "submit", `{"answer":"done"}`)
		response["output"].([]any)[0].(map[string]any)["namespace"] = "roast"
		response["usage"] = map[string]any{"input_tokens": 100, "input_tokens_details": map[string]any{"cached_tokens": 30}, "output_tokens": 10}
		w := httptest.NewRecorder()
		_ = testutil.WriteInferenceStream(w, "responses", response)
		return planResponse(w.Body.Bytes()), nil
	}))
	var out struct{ Answer string }
	if err := c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 1, FinalTool: "submit", Tools: basicTools(nil)}, &out); err != nil {
		t.Fatal(err)
	}
	usage := c.Usage()
	if usage.UncachedInputTokens != 70 || usage.CachedInputTokens != 30 || usage.OutputTokens != 10 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestChatGPTReplaysFinalizedReasoningInOriginalOrder(t *testing.T) {
	calls := 0
	var firstOutput []any
	c := planTestClient(t, ClientConfig{}, planRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		var request map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&request)
		if string(request["include"]) != `["reasoning.encrypted_content"]` {
			t.Error("encrypted reasoning not requested")
		}
		tool, arguments := "read", `{}`
		if calls == 2 {
			tool, arguments = "submit", `{"answer":"done"}`
			var input []json.RawMessage
			_ = json.Unmarshal(request["input"], &input)
			var replay []any
			for _, raw := range input {
				var item map[string]any
				_ = json.Unmarshal(raw, &item)
				if item["id"] == "reason-before" || item["id"] == "reason-after" || item["type"] == "function_call" {
					replay = append(replay, item)
				}
			}
			want, _ := json.Marshal(firstOutput)
			got, _ := json.Marshal(replay)
			if !bytes.Equal(got, want) {
				t.Fatalf("finalized raw output lost or reordered\ngot %s\nwant %s", got, want)
			}
		}
		response := structuredToolResponse("responses", tool, arguments)
		function := response["output"].([]any)[0].(map[string]any)
		function["namespace"] = "roast"
		if calls == 1 {
			firstOutput = []any{
				map[string]any{"type": "reasoning", "id": "reason-before", "summary": []any{}, "encrypted_content": "opaque-before", "status": "completed"},
				function,
				map[string]any{"type": "reasoning", "id": "reason-after", "summary": []any{map[string]any{"type": "summary_text", "text": "summary"}}, "encrypted_content": "opaque-finalized", "status": "completed"},
			}
			response["output"] = firstOutput
		}
		w := httptest.NewRecorder()
		_ = testutil.WriteInferenceStream(w, "responses", response)
		// Encrypted content is only final at output_item.done/completed in real
		// streams. The SDK's metadata at item.added must not be the replay source.
		stream := w.Body.String()
		lines := strings.Split(stream, "\n")
		for i, line := range lines {
			if strings.HasPrefix(line, "data:") && strings.Contains(line, `"type":"response.output_item.added"`) {
				lines[i] = strings.ReplaceAll(strings.ReplaceAll(line, `,"encrypted_content":"opaque-before"`, ""), `,"encrypted_content":"opaque-finalized"`, "")
			}
		}
		return planResponse([]byte(strings.Join(lines, "\n"))), nil
	}))
	var out struct{ Answer string }
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) { return "result", nil })}, &out)
	if err != nil || calls != 2 || out.Answer != "done" {
		t.Fatalf("calls=%d output=%+v error=%v", calls, out, err)
	}
}

func TestChatGPTCaptureFailurePreventsToolExecution(t *testing.T) {
	c := planTestClient(t, ClientConfig{CaptureResponse: func(RawResponse) error { return errors.New("disk full") }}, planRoundTripper(func(*http.Request) (*http.Response, error) { return planResponse(planStream(t, "read", `{}`)), nil }))
	var out struct{ Answer string }
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
		t.Error("tool dispatched after capture failure")
		return nil, nil
	})}, &out)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error=%v", err)
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

func TestChatGPTTerminalGuardFailureStopsBeforeDispatch(t *testing.T) {
	c := testClient(nil)
	c.cfg.Protocol = "chatgpt"
	c.model.(*fakeLanguageModel).stream = func(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		return func(yield func(fantasy.StreamPart) bool) {
			// Simulate an SDK which stops reading at a completion event. The
			// transport has flagged it, but no subsequent Read exposes the error.
			owner := ctx.Value(responseBodyKey{}).(*responseBody)
			owner.failure = errors.New("terminal stream guard rejected output")
			if yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "read", ToolCallName: "read", ToolCallInput: `{}`}) {
				if yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "submit", ToolCallName: "submit", ToolCallInput: `{"answer":"must not succeed"}`}) {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				}
			}
		}, nil
	}
	var out struct{ Answer string }
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
		t.Error("tool dispatched after terminal guard failure")
		return nil, nil
	})}, &out)
	if err == nil || out.Answer != "" || !strings.Contains(err.Error(), "terminal stream guard") {
		t.Fatalf("output=%+v error=%v", out, err)
	}
}
