package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bluesky-social/roast/internal/testutil"
)

func anthropicResponse(tool, arguments string) map[string]any {
	var input map[string]any
	_ = json.Unmarshal([]byte(arguments), &input)
	return map[string]any{
		"id": "msg", "type": "message", "role": "assistant", "model": "claude-test",
		"content":     []any{map[string]any{"type": "tool_use", "id": "toolu-" + tool, "name": tool, "input": input}},
		"stop_reason": "tool_use",
		"usage": map[string]any{
			"input_tokens": 10, "output_tokens": 4,
			"cache_creation_input_tokens": 2, "cache_read_input_tokens": 3,
		},
	}
}

func TestUserAgentIncludesBuildVersion(t *testing.T) {
	if got := userAgent(); got != "roast/dev" {
		t.Fatalf("userAgent() = %q, want roast/dev", got)
	}
}

func TestAnthropicAgentUsesNativeMessages(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	var paths, keys, versions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, request)
		paths = append(paths, r.URL.Path)
		keys = append(keys, r.Header.Get("X-Api-Key"))
		versions = append(versions, r.Header.Get("Anthropic-Version"))
		call := len(requests)
		mu.Unlock()
		tool, args := "read", `{"path":"x.go"}`
		if call == 2 {
			tool, args = "submit", `{"answer":"done"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = testutil.WriteInferenceStream(w, "anthropic", anthropicResponse(tool, args))
	}))
	defer server.Close()

	c, err := New(t.Context(), ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "anthropic-key", Model: "claude-test",
		Protocol: "anthropic", MaxOutputTokens: 1024, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Answer string `json:"answer"`
	}
	err = c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 2, FinalTool: "submit",
		Tools: []Tool{
			{Name: "read", Description: "read", Schema: map[string]any{"type": "object"}, Run: func(context.Context, map[string]any) (any, error) { return map[string]any{"content": "package x"}, nil }},
			{Name: "submit", Description: "submit", Schema: map[string]any{
				"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}},
				"required": []string{"answer"}, "additionalProperties": false,
			}},
		},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Answer != "done" {
		t.Fatalf("answer = %q", out.Answer)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || paths[0] != "/v1/messages" || keys[0] != "anthropic-key" || versions[0] == "" {
		t.Fatalf("requests=%d path=%q key=%q version=%q", len(requests), paths[0], keys[0], versions[0])
	}
	if requests[0]["system"] == nil {
		t.Fatalf("first request lacks system prompt: %#v", requests[0])
	}
	for _, request := range requests {
		if request["stream"] != true {
			t.Fatal("native Anthropic request did not enable streaming")
		}
	}
	// The wrap-up turn keeps the tool set: it is part of the signed-reasoning prefix.
	firstTools, _ := json.Marshal(requests[0]["tools"])
	secondToolsJSON, _ := json.Marshal(requests[1]["tools"])
	secondTools, _ := requests[1]["tools"].([]any)
	if string(firstTools) != string(secondToolsJSON) || len(secondTools) != 2 || secondTools[1].(map[string]any)["name"] != "submit" {
		t.Fatalf("second tools = %s, want the first request's %s", secondToolsJSON, firstTools)
	}
	inputSchema, _ := secondTools[1].(map[string]any)["input_schema"].(map[string]any)
	if inputSchema["type"] != "object" || inputSchema["required"] == nil || inputSchema["properties"] == nil {
		t.Fatalf("Anthropic input schema lost core fields: %#v", inputSchema)
	}
	if _, ok := inputSchema["additionalProperties"]; ok {
		t.Fatalf("Fantasy unexpectedly preserved additionalProperties; revisit the documented adapter limitation: %#v", inputSchema)
	}
	data, _ := json.Marshal(requests[1]["messages"])
	if !strings.Contains(string(data), "package x") || !strings.Contains(string(data), "Exploration budget exhausted") {
		t.Fatalf("second messages = %s", data)
	}
	if usage := c.Usage(); usage.Calls != 2 || usage.UncachedInputTokens != 20 || usage.CachedInputTokens != 6 || usage.CacheWriteTokens != 4 || usage.OutputTokens != 8 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestResponsesAgentReplaysToolOutputAndKeepsToolsOnFinalTurn(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, request)
		call := len(requests)
		mu.Unlock()
		output := map[string]any{
			"id": "fc-read", "type": "function_call", "call_id": "call-read",
			"name": "read", "arguments": `{"path":"x.go"}`, "status": "completed",
		}
		if call == 2 {
			output = map[string]any{
				"id": "fc-submit", "type": "function_call", "call_id": "call-submit",
				"name": "submit", "arguments": `{"answer":"done"}`, "status": "completed",
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = testutil.WriteInferenceStream(w, "responses", map[string]any{
			"id": "resp-" + string(rune('0'+call)), "object": "response", "created_at": 1,
			"status": "completed", "model": "test-model", "output": []any{output},
			"usage": map[string]any{
				"input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
				"input_tokens_details":  map[string]any{"cached_tokens": 0},
				"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			},
		})
	}))
	defer server.Close()
	c, err := New(t.Context(), ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "test", Model: "test-model", Protocol: "responses",
		MaxOutputTokens: 1024, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Answer string `json:"answer"`
	}
	err = c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 2, FinalTool: "submit",
		Tools: []Tool{
			{Name: "read", Schema: map[string]any{"type": "object"}, Run: func(context.Context, map[string]any) (any, error) { return map[string]any{"content": "package x"}, nil }},
			{Name: "submit", Schema: map[string]any{"type": "object"}},
		},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Answer != "done" {
		t.Fatalf("answer = %q", out.Answer)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || requests[1]["previous_response_id"] != nil || requests[1]["store"] != false {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[1]["tool_choice"] != "auto" {
		t.Fatalf("final tool choice = %#v, want auto", requests[1]["tool_choice"])
	}
	firstTools, _ := json.Marshal(requests[0]["tools"])
	secondTools, _ := json.Marshal(requests[1]["tools"])
	if string(firstTools) != string(secondTools) || !strings.Contains(string(secondTools), `"submit"`) {
		t.Fatalf("second tools = %s, want the first request's %s", secondTools, firstTools)
	}
	input, _ := requests[1]["input"].([]any)
	inputJSON, _ := json.Marshal(input)
	if !strings.Contains(string(inputJSON), "function_call_output") || !strings.Contains(string(inputJSON), "package x") {
		t.Fatalf("second input = %s", inputJSON)
	}
}

func TestAnthropicCompleteStructuredUsesAutomaticToolChoice(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&request)
		choice, _ := request["tool_choice"].(map[string]any)
		if choice["type"] != "auto" {
			http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"forced tool choice is not supported"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = testutil.WriteInferenceStream(w, "anthropic", anthropicResponse("verdict", `{"status":"confirmed"}`))
	}))
	defer server.Close()
	c, err := New(t.Context(), ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "key", Model: "claude-test", Protocol: "anthropic",
		MaxOutputTokens: 100, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := c.CompleteStructured(t.Context(), "system", "user", "verdict", &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "confirmed" {
		t.Fatalf("status = %q", out.Status)
	}
	choice, _ := request["tool_choice"].(map[string]any)
	tools, _ := request["tools"].([]any)
	if choice["type"] != "auto" || len(tools) != 1 || tools[0].(map[string]any)["name"] != "verdict" {
		t.Fatalf("request = %#v", request)
	}
}

func TestCaptureFailureDoesNotRetryProviderCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = testutil.WriteInferenceStream(w, "anthropic", anthropicResponse("verdict", `{"status":"confirmed"}`))
	}))
	defer server.Close()
	c, err := New(t.Context(), ClientConfig{
		Role: "verifier", BaseURL: server.URL + "/v1", APIKey: "key", Model: "claude-test",
		Protocol: "anthropic", MaxOutputTokens: 100, MaxAttempts: 3,
		CaptureResponse: func(RawResponse) error { return errors.New("disk full") },
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Status string }
	err = c.CompleteStructured(t.Context(), "system", "user", "verdict", &out)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %v, want capture failure", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestCaptureRejectsOversizedProviderResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "16777217")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c, err := New(t.Context(), ClientConfig{
		Role: "finder", BaseURL: server.URL + "/v1", APIKey: "key", Model: "chat-test", Protocol: "chat",
		MaxOutputTokens: 100, MaxAttempts: 1, CaptureResponse: func(RawResponse) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	err = c.CompleteStructured(t.Context(), "system", "user", "verdict", &out)
	if err == nil || !strings.Contains(err.Error(), "body exceeds 16777216-byte limit") {
		t.Fatalf("error = %v", err)
	}
}

func TestCaptureDoesNotRetryInvalidJSONResponse(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer server.Close()
	c, err := New(t.Context(), ClientConfig{
		Role: "finder", BaseURL: server.URL + "/v1", APIKey: "key", Model: "chat-test", Protocol: "chat",
		MaxOutputTokens: 100, MaxAttempts: 3, CaptureResponse: func(RawResponse) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	err = c.CompleteStructured(t.Context(), "system", "user", "verdict", &out)
	if err == nil || !strings.Contains(err.Error(), "invalid raw JSON") {
		t.Fatalf("error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
}

func TestChatRetriesOnlyThroughFantasy(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"temporary","type":"server_error"}}`))
	}))
	defer server.Close()
	c, err := New(t.Context(), ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "key", Model: "chat-test", Protocol: "chat",
		MaxOutputTokens: 100, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	if err := c.CompleteStructured(t.Context(), "system", "user", "verdict", &out); err == nil {
		t.Fatal("expected endpoint failure")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want one Fantasy-controlled attempt", got)
	}
}

func TestResponsesRetriesOnlyThroughFantasy(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"temporary","type":"server_error"}}`))
	}))
	defer server.Close()
	c, err := New(t.Context(), ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "key", Model: "responses-test", Protocol: "responses",
		MaxOutputTokens: 100, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	if err := c.CompleteStructured(t.Context(), "system", "user", "verdict", &out); err == nil {
		t.Fatal("expected endpoint failure")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want one Fantasy-controlled attempt", got)
	}
}

func TestAPIKeyProvidersMapAuthenticationFailure(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			t.Run(protocol+"/"+http.StatusText(status), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid credential"}}`))
				}))
				defer server.Close()
				c, err := New(t.Context(), ClientConfig{
					BaseURL: server.URL + "/v1", APIKey: "bad", Model: "test-model", Protocol: protocol,
					MaxOutputTokens: 100, MaxAttempts: 3,
				})
				if err != nil {
					t.Fatal(err)
				}
				var out struct{}
				err = c.CompleteStructured(t.Context(), "system", "user", "verdict", &out)
				if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "check the configured API key") {
					t.Fatalf("error = %v, want ErrAuth with API-key recovery", err)
				}
				if got := calls.Load(); got != 1 {
					t.Fatalf("provider calls = %d, want one authentication failure", got)
				}
			})
		}
	}
}

func TestAnthropicDoesNotForwardCredentialsThroughRedirect(t *testing.T) {
	forwarded := false
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		forwarded = r.Header.Get("X-Api-Key") != ""
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c, err := New(t.Context(), ClientConfig{
		BaseURL: redirect.URL + "/v1", APIKey: "secret", Model: "claude-test", Protocol: "anthropic",
		MaxOutputTokens: 100, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	err = c.CompleteStructured(t.Context(), "system", "user", "verdict", &out)
	if err == nil || forwarded {
		t.Fatalf("error=%v credential_forwarded=%v", err, forwarded)
	}
}
