package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
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

func TestProviderConversationIdentity(test *testing.T) {
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		test.Run(protocol, func(test *testing.T) {
			var mu sync.Mutex
			attempts := make(map[string]int)
			identities := make(map[string]string)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				defer request.Body.Close()
				body, err := io.ReadAll(request.Body)
				if err != nil {
					test.Error(err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				marker := "conversation-one"
				if strings.Contains(string(body), "conversation-two") {
					marker = "conversation-two"
				} else if !strings.Contains(string(body), marker) {
					test.Error("request lost conversation prompt")
				}
				sessionID := request.Header.Get("X-Agent-Session-Id")
				if !strings.HasPrefix(sessionID, "roast-") {
					test.Errorf("session ID = %q", sessionID)
				}
				if got := request.Header.Values("X-Opencode-Session"); len(got) != 1 || got[0] != sessionID {
					test.Errorf("configured session header = %q, want [%q]", got, sessionID)
				}
				mu.Lock()
				if previous := identities[marker]; previous != "" && previous != sessionID {
					test.Errorf("%s changed identity: %q -> %q", marker, previous, sessionID)
				}
				identities[marker] = sessionID
				attempts[marker]++
				attempt := attempts[marker]
				mu.Unlock()
				writer.Header().Set("Content-Type", "application/json")
				if attempt == 1 {
					writer.Header().Set("Retry-After-Ms", "1")
					writer.WriteHeader(http.StatusInternalServerError)
					_, _ = writer.Write([]byte(`{"error":{"message":"temporary","type":"server_error"}}`))
					return
				}
				tool, arguments := "read", `{"path":"x.go"}`
				if attempt >= 3 {
					tool, arguments = "submit", `{"answer":"done"}`
				}
				var response map[string]any
				switch protocol {
				case "anthropic":
					response = anthropicResponse(tool, arguments)
				case "responses":
					response = map[string]any{
						"id": "resp-test", "object": "response", "status": "completed", "model": "test-model",
						"output": []any{map[string]any{
							"id": "fc-" + tool, "type": "function_call", "call_id": "call-" + tool,
							"name": tool, "arguments": arguments, "status": "completed",
						}},
						"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
					}
				case "chat":
					response = map[string]any{
						"id": "chat-test", "object": "chat.completion", "model": "test-model",
						"choices": []any{map[string]any{
							"index": 0, "finish_reason": "tool_calls",
							"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
								"id": "call-" + tool, "type": "function",
								"function": map[string]any{"name": tool, "arguments": arguments},
							}}},
						}},
						"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
					}
				}
				_ = testutil.WriteInferenceStream(writer, protocol, response)
			}))
			defer server.Close()
			client, err := New(test.Context(), ClientConfig{
				BaseURL: server.URL + "/v1", APIKey: "test", Model: "test-model", Protocol: protocol,
				MaxOutputTokens: 100, MaxAttempts: 2, SessionHeader: "x-opencode-session",
			})
			if err != nil {
				test.Fatal(err)
			}
			var conversations sync.WaitGroup
			for _, marker := range []string{"conversation-one", "conversation-two"} {
				conversations.Go(func() {
					var result struct {
						Answer string `json:"answer"`
					}
					err := client.Converse(test.Context(), "system", marker, ConverseOpts{
						MaxTurns: 2, FinalTool: "submit",
						Tools: basicTools(func(context.Context, map[string]any) (any, error) { return "file contents", nil }),
					}, &result)
					if err != nil || result.Answer != "done" {
						test.Errorf("%s: result = %q, error = %v", marker, result.Answer, err)
					}
				})
			}
			conversations.Wait()
			mu.Lock()
			defer mu.Unlock()
			if len(identities) != 2 || identities["conversation-one"] == identities["conversation-two"] {
				test.Fatalf("independent conversations share identities: %v", identities)
			}
			for marker, count := range attempts {
				if count != 3 {
					test.Errorf("%s: attempts = %d, want retry plus two turns", marker, count)
				}
			}
		})
	}
}

func TestStructuredCorrectionsRetainConversationIdentity(test *testing.T) {
	var identities []string
	client := testClient(func(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
		identity, _ := ctx.Value(sessionContextKey{}).(string)
		identities = append(identities, identity)
		arguments := `{"answer":123}`
		if len(identities)%2 == 0 {
			arguments = `{"answer":"done"}`
		}
		return toolCallResponse("submit", arguments), nil
	})
	client.cfg.MaxAttempts = 2
	for range 2 {
		var result struct {
			Answer string `json:"answer"`
		}
		if err := client.CompleteStructured(test.Context(), "system", "user", "submit", &result); err != nil {
			test.Fatal(err)
		}
	}
	if len(identities) != 4 || identities[0] == "" || identities[0] != identities[1] || identities[2] != identities[3] || identities[0] == identities[2] {
		test.Fatalf("conversation identities = %v", identities)
	}
}

type sessionRoundTripper func(*http.Request) (*http.Response, error)

func (transport sessionRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestSessionTransportDoesNotMutateRequests(test *testing.T) {
	transport := &captureTransport{cfg: ClientConfig{SessionHeader: "X-Gateway-Session"}, base: sessionRoundTripper(func(request *http.Request) (*http.Response, error) {
		if identity := request.Header.Get("X-Agent-Session-Id"); identity != "roast-test" {
			return nil, fmt.Errorf("session identity = %q", identity)
		}
		if identity := request.Header.Get("X-Gateway-Session"); identity != "roast-test" {
			return nil, fmt.Errorf("configured session identity = %q", identity)
		}
		return &http.Response{StatusCode: http.StatusNoContent}, nil
	})}
	ctx := context.WithValue(test.Context(), sessionContextKey{}, "roast-test")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"X-Agent-Session-Id", "X-Gateway-Session"} {
		if identity := request.Header.Get(name); identity != "" {
			test.Fatalf("mutated caller request header %s: %q", name, identity)
		}
	}
}

// A configured session header that the environment also names goes out once,
// with Roast's value: environment headers are removed before session headers
// are set, so the stripping cannot remove the configured one.
func TestSessionHeaderOverridesEnvironmentHeader(test *testing.T) {
	const ambient = "x-opencode-session: junk-ambient-session\nX-Agent-Session-Id: junk-ambient-session"
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		test.Run(protocol, func(test *testing.T) {
			setAmbientSDKEnvironment(test, map[string]string{"OPENAI_CUSTOM_HEADERS": ambient, "ANTHROPIC_CUSTOM_HEADERS": ambient}, "", "")
			var mu sync.Mutex
			var headers []http.Header
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				mu.Lock()
				headers = append(headers, request.Header.Clone())
				mu.Unlock()
				writer.Header().Set("Content-Type", "application/json")
				_ = testutil.WriteInferenceStream(writer, protocol, structuredToolResponse(protocol, "verdict", `{"status":"ok"}`))
			}))
			defer server.Close()
			client, err := New(test.Context(), ClientConfig{
				BaseURL: server.URL + "/v1", APIKey: "test", Model: "test-model", Protocol: protocol,
				MaxOutputTokens: 100, MaxAttempts: 1, SessionHeader: "X-OpenCode-Session",
			})
			if err != nil {
				test.Fatal(err)
			}
			var result struct {
				Status string `json:"status"`
			}
			if err := client.CompleteStructured(test.Context(), "system", "user", "verdict", &result); err != nil {
				test.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(headers) != 1 {
				test.Fatalf("server received %d requests, want 1", len(headers))
			}
			session, configured := headers[0].Values("X-Agent-Session-Id"), headers[0].Values("X-Opencode-Session")
			if len(session) != 1 || !strings.HasPrefix(session[0], "roast-") || len(configured) != 1 || configured[0] != session[0] {
				test.Fatalf("X-Agent-Session-Id = %q, X-Opencode-Session = %q; want one equal Roast session ID each", session, configured)
			}
		})
	}
}

func TestAnthropicInterruptedStreamRetriesWithoutExecutingPartialTools(t *testing.T) {
	var calls, tools atomic.Int32
	var captured []RawResponse
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		tool, arguments := "read", `{"path":"fixture.go"}`
		if call == 3 {
			tool, arguments = "submit", `{"answer":"done"}`
		}
		fixture := httptest.NewRecorder()
		if err := testutil.WriteInferenceStream(fixture, "anthropic", anthropicResponse(tool, arguments)); err != nil {
			t.Error(err)
			return
		}
		body := fixture.Body.String()
		if call == 1 {
			// Include the complete tool input and stop_reason, but drop the
			// terminal message_stop event. This is not a successful turn.
			body = body[:strings.Index(body, "event: message_stop")]
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	ledger := NewLedger(1000000)
	c, err := New(t.Context(), ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "fixture-key", Model: "claude-test", Protocol: "anthropic",
		MaxOutputTokens: 32768, MaxAttempts: 2, Ledger: ledger,
		CaptureResponse: func(response RawResponse) error { captured = append(captured, response); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Answer string }
	err = c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
			tools.Add(1)
			return "contents", nil
		}),
	}, &out)
	if err != nil || calls.Load() != 3 || tools.Load() != 1 || out.Answer != "done" {
		t.Fatalf("error=%v requests=%d tools=%d answer=%q", err, calls.Load(), tools.Load(), out.Answer)
	}
	if len(captured) != 3 || strings.Contains(string(captured[0].Body), "event: message_stop") || !strings.Contains(string(captured[1].Body), "event: message_stop") {
		t.Fatalf("interrupted and complete streams were not preserved: %+v", captured)
	}
	spent, _ := ledger.Committed()
	if c.Usage().Calls != 2 || spent != int(weightedTokens(c.Usage())) || ledger.outstanding != 0 {
		t.Fatalf("usage=%+v spent=%d outstanding=%v", c.Usage(), spent, ledger.outstanding)
	}
}

type trackedResponseBody struct {
	io.Reader
	reads, closes int
}

func (b *trackedResponseBody) Read(p []byte) (int, error) { b.reads++; return b.Reader.Read(p) }
func (b *trackedResponseBody) Close() error               { b.closes++; return nil }

func TestStreamCaptureDoesNotEagerlyReadAndPreservesPartialBytes(t *testing.T) {
	raw := ": keepalive\r\n\r\nevent: message_start\r\ndata: {}\r\n\r\n"
	body := &trackedResponseBody{Reader: strings.NewReader(raw)}
	var captured []RawResponse
	transport := &captureTransport{
		base: sessionRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}, Body: body, ContentLength: -1}, nil
		}),
		cfg: ClientConfig{Protocol: "anthropic", CaptureResponse: func(response RawResponse) error { captured = append(captured, response); return nil }},
	}
	owner := &responseBody{}
	request, _ := http.NewRequestWithContext(context.WithValue(t.Context(), responseBodyKey{}, owner), http.MethodPost, "https://inference.example/v1/messages", nil)
	response, err := transport.RoundTrip(request)
	if err != nil || body.reads != 0 {
		t.Fatalf("RoundTrip error=%v eager reads=%d", err, body.reads)
	}
	buffer := make([]byte, 5)
	if _, err := io.ReadFull(response.Body, buffer); err != nil {
		t.Fatal(err)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 || string(captured[0].Body) != raw[:5] || captured[0].ContentType != "text/event-stream" || body.closes != 1 {
		t.Fatalf("captured=%+v closes=%d", captured, body.closes)
	}
}

func TestStreamCaptureEnforcesLimitWithoutContentLength(t *testing.T) {
	called := false
	transport := &captureTransport{cfg: ClientConfig{CaptureResponse: func(RawResponse) error { called = true; return nil }}}
	body := &trackedResponseBody{Reader: strings.NewReader(strings.Repeat("x", maxCapturedResponseSize+1))}
	capture := &streamCaptureBody{body: body, transport: transport}
	_, err := io.Copy(io.Discard, capture)
	var captureErr *captureError
	if !errors.As(err, &captureErr) || !strings.Contains(err.Error(), "body exceeds") {
		t.Fatalf("error=%v", err)
	}
	if err := capture.Close(); !errors.As(err, &captureErr) || called || body.closes != 1 || capture.data.Len() > maxCapturedResponseSize {
		t.Fatalf("close error=%v callback=%v closes=%d captured bytes=%d", err, called, body.closes, capture.data.Len())
	}
}
