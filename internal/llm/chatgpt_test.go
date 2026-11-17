package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/bluesky-social/roast/internal/auth/chatgpt"
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
	c, err := newChatGPTResponsesClient(t.Context(), cfg, func(context.Context) (string, error) { return "oauth-only", nil }, transport)
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
	if len(captures) != len(requests) {
		t.Fatalf("captured responses = %d, want %d", len(captures), len(requests))
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
		input, err := json.Marshal(req["input"])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(input, []byte("Exact Roast system prompt")) || !bytes.Contains(input, []byte(`"role":"developer"`)) {
			t.Fatalf("prompt not preserved as developer message: %s", input)
		}
		if i == 1 && (!bytes.Contains(input, []byte("source file content")) || !bytes.Contains(input, []byte(`"namespace":"roast"`))) {
			t.Fatalf("tool history not replayed: %s", input)
		}
		namespaces, ok := req["tools"].([]any)
		if !ok || len(namespaces) != 1 {
			t.Fatalf("tools = %#v, want one namespace", req["tools"])
		}
		namespace, ok := namespaces[0].(map[string]any)
		if !ok || namespace["name"] != "roast" || namespace["type"] != "namespace" {
			t.Fatalf("tools not namespaced: %#v", namespaces)
		}
		if captures[i].Protocol != "chatgpt" || !bytes.Equal(captures[i].Body, expectedCapture[i]) {
			t.Fatal("raw capture changed the server stream")
		}
	}
}

func TestChatGPTCredentialFailure(t *testing.T) {
	c, err := newChatGPTResponsesClient(t.Context(), ClientConfig{Protocol: "chatgpt", Model: "test", MaxOutputTokens: 100}, func(context.Context) (string, error) {
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

func TestChatGPTUsageCountsCachedInputOnce(t *testing.T) {
	c := planTestClient(t, ClientConfig{}, planRoundTripper(func(*http.Request) (*http.Response, error) {
		response := structuredToolResponse("responses", "submit", `{"answer":"done"}`)
		response["output"].([]any)[0].(map[string]any)["namespace"] = "roast"
		response["usage"] = map[string]any{"input_tokens": 100, "input_tokens_details": map[string]any{"cached_tokens": 30}, "output_tokens": 10}
		w := httptest.NewRecorder()
		if err := testutil.WriteInferenceStream(w, "responses", response); err != nil {
			return nil, err
		}
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
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		if string(request["include"]) != `["reasoning.encrypted_content"]` {
			t.Error("encrypted reasoning not requested")
		}
		tool, arguments := "read", `{}`
		if calls == 2 {
			tool, arguments = "submit", `{"answer":"done"}`
			var input []json.RawMessage
			if err := json.Unmarshal(request["input"], &input); err != nil {
				return nil, err
			}
			var replay []any
			for _, raw := range input {
				var item map[string]any
				if err := json.Unmarshal(raw, &item); err != nil {
					return nil, err
				}
				if item["id"] == "reason-before" || item["id"] == "reason-after" || item["type"] == "function_call" {
					replay = append(replay, item)
				}
			}
			want, err := json.Marshal(firstOutput)
			if err != nil {
				return nil, err
			}
			got, err := json.Marshal(replay)
			if err != nil {
				return nil, err
			}
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
		if err := testutil.WriteInferenceStream(w, "responses", response); err != nil {
			return nil, err
		}
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

func TestChatGPTAccountSnapshotKeepsInitializationFailure(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(name, root)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(configDir, "roast", "chatgpt")
	initial, err := chatgpt.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := initial.PrepareHost(t.Context()); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(index, []byte("invalid index"), 0600); err != nil {
		t.Fatal(err)
	}
	operation := WithChatGPTAccount(t.Context())
	initialErr := CheckChatGPTLogin(operation)
	if initialErr == nil {
		t.Fatal("invalid index did not fail initialization")
	}
	if err := os.WriteFile(index, []byte(`{"active":"account-b","clients":["account-b"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Derived request contexts retain the original failed selection instead of
	// silently adopting an account that appeared later in the operation.
	request, cancel := context.WithCancel(operation)
	defer cancel()
	cfg := ClientConfig{Protocol: "chatgpt", Model: "test-model", MaxOutputTokens: 100}
	if _, err := New(request, cfg); !errors.Is(err, ErrAuth) || !errors.Is(err, initialErr) {
		t.Fatalf("original operation error = %v, want cached authentication failure", err)
	}
	if err := CheckChatGPTLogin(request); !errors.Is(err, initialErr) {
		t.Fatal("credential check forgot the original initialization failure")
	}
	if _, err := ChatGPTModels(request); !errors.Is(err, initialErr) {
		t.Fatal("catalog check forgot the original initialization failure")
	}
	// Starting another operation captures the new selection even if its parent
	// context carries an old snapshot. Construction does not retrieve a token.
	if _, err := New(WithChatGPTAccount(operation), cfg); err != nil {
		t.Fatalf("next operation failed to capture the repaired index: %v", err)
	}
}
