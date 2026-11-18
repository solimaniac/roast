package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// Exercise the real Fantasy/Anthropic SDK path, not a fake LanguageModel. The
// production-sized ceiling must reach the native endpoint unchanged even though
// the fixture produces only a few tokens. Either streaming or ordinary Messages
// requests are acceptable; compatibility endpoints and smaller budgets are not.
func TestAnthropicNativeMessagesPreservesOutputBudget(t *testing.T) {
	for _, budget := range []int{1024, 32768} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			var calls, toolCalls atomic.Int32
			var captured []RawResponse
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				call := calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "fixture-key" || r.Header.Get("Anthropic-Version") == "" {
					t.Error("request did not use the native authenticated Messages endpoint")
					http.Error(w, "native Messages required", http.StatusBadRequest)
					return
				}
				var request struct {
					Model      string            `json:"model"`
					MaxTokens  int               `json:"max_tokens"`
					Stream     bool              `json:"stream"`
					System     json.RawMessage   `json:"system"`
					Messages   json.RawMessage   `json:"messages"`
					Tools      []json.RawMessage `json:"tools"`
					ToolChoice struct {
						Type string `json:"type"`
					} `json:"tool_choice"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode native request: %v", err)
					http.Error(w, "invalid JSON", http.StatusBadRequest)
					return
				}
				if request.MaxTokens != budget || request.Model != "claude-test" || len(request.System) == 0 || len(request.Tools) == 0 {
					t.Errorf("native request lost its budget/model/system/tools: %+v", request)
					http.Error(w, "request configuration changed", http.StatusBadRequest)
					return
				}
				if request.ToolChoice.Type != "auto" {
					http.Error(w, "forced tool choice is not supported", http.StatusBadRequest)
					return
				}
				tool, arguments := "read", `{"path":"fixture.go"}`
				if call > 1 {
					if !strings.Contains(string(request.Messages), "tool_result") || !strings.Contains(string(request.Messages), "package fixture") {
						t.Error("native follow-up request lost the tool result")
					}
					tool, arguments = "submit", `{"answer":"done"}`
				}
				if err := writeAnthropicBudgetResponse(w, request.Stream, tool, arguments); err != nil {
					t.Errorf("write native fixture response: %v", err)
				}
			}))
			defer server.Close()

			c, err := New(t.Context(), ClientConfig{
				BaseURL: server.URL + "/v1", APIKey: "fixture-key", Model: "claude-test",
				Protocol: "anthropic", MaxOutputTokens: budget, MaxAttempts: 1,
				RequestTimeout: 10 * time.Minute,
				CaptureResponse: func(response RawResponse) error {
					captured = append(captured, response)
					return nil
				},
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
					{Name: "read", Description: "read a file", Schema: map[string]any{"type": "object"}, Run: func(context.Context, map[string]any) (any, error) {
						toolCalls.Add(1)
						return map[string]any{"content": "package fixture"}, nil
					}},
					{Name: "submit", Description: "return the result", Schema: map[string]any{
						"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}},
						"required": []string{"answer"},
					}},
				},
			}, &out)
			if err != nil {
				t.Fatalf("native Anthropic run with max_tokens=%d failed after %d HTTP requests: %v", budget, calls.Load(), err)
			}
			if calls.Load() != 2 || toolCalls.Load() != 1 || out.Answer != "done" {
				t.Fatalf("native run incomplete: HTTP requests=%d tool calls=%d answer=%q", calls.Load(), toolCalls.Load(), out.Answer)
			}
			if len(captured) != 2 || c.Usage().Calls != 2 {
				t.Fatalf("captured=%d usage=%+v", len(captured), c.Usage())
			}
			for _, response := range captured {
				if response.ContentType != "text/event-stream" || !strings.Contains(string(response.Body), "event: message_stop") {
					t.Fatalf("native stream was not captured intact: %+v", response)
				}
			}
		})
	}
}

// Keep the acceptance test independent of the eventual timeout/streaming fix.
func writeAnthropicBudgetResponse(w http.ResponseWriter, stream bool, tool, arguments string) error {
	response := anthropicResponse(tool, arguments)
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(w).Encode(response)
	}
	return testutil.WriteInferenceStream(w, "anthropic", response)
}

// deepSeekThinkingServer emulates DeepSeek's thinking mode on its chat and
// Anthropic-compatible endpoints: any forced tool choice is a 400, and every
// earlier assistant turn must replay its reasoning when tools are present.
type deepSeekThinkingServer struct {
	t        *testing.T
	protocol string
	// respond returns the tool to call and its arguments for the nth request.
	// An empty tool answers in prose; a non-empty finish overrides the
	// natural finish reason.
	respond func(request int) (tool, arguments, finish string)

	mu      sync.Mutex
	choices []string
}

const deepSeekToolChoiceError = `{"error":{"message":"Thinking mode does not support this tool_choice (request_id: fixture)","type":"invalid_request_error"}}`

func newDeepSeekThinkingServer(t *testing.T, protocol string, respond func(int) (string, string, string)) (*httptest.Server, *deepSeekThinkingServer) {
	d := &deepSeekThinkingServer{t: t, protocol: protocol, respond: respond}
	server := httptest.NewServer(d)
	t.Cleanup(server.Close)
	return server, d
}

func (d *deepSeekThinkingServer) reject(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(body))
}

func (d *deepSeekThinkingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ToolChoice json.RawMessage   `json:"tool_choice"`
		Tools      []json.RawMessage `json:"tools"`
		Messages   []map[string]any  `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.choices = append(d.choices, string(request.ToolChoice))
	call := len(d.choices)
	d.mu.Unlock()

	var choice any
	_ = json.Unmarshal(request.ToolChoice, &choice)
	auto := choice == nil || choice == "auto"
	if object, ok := choice.(map[string]any); ok {
		auto = object["type"] == "auto"
	}
	if !auto {
		d.reject(w, deepSeekToolChoiceError)
		return
	}
	for _, message := range request.Messages {
		if message["role"] != "assistant" || len(request.Tools) == 0 {
			continue
		}
		switch d.protocol {
		case "chat":
			if _, ok := message["reasoning_content"]; !ok {
				d.reject(w, `{"error":{"message":"The reasoning_content in the thinking mode must be passed back to the API.","type":"invalid_request_error"}}`)
				return
			}
		case "anthropic":
			content, _ := message["content"].([]any)
			var first map[string]any
			if len(content) > 0 {
				first, _ = content[0].(map[string]any)
			}
			if first["type"] != "thinking" {
				d.reject(w, `{"type":"error","error":{"type":"invalid_request_error","message":"The content[].thinking in the thinking mode must be passed back to the API."}}`)
				return
			}
		}
	}

	tool, arguments, finish := d.respond(call)
	var response map[string]any
	switch d.protocol {
	case "chat":
		message := map[string]any{"role": "assistant", "reasoning_content": "Thinking it through."}
		natural := "stop"
		if tool == "" {
			message["content"] = "Here is my answer in prose."
		} else {
			natural = "tool_calls"
			message["tool_calls"] = []any{map[string]any{
				"id": fmt.Sprintf("call-%d", call), "type": "function",
				"function": map[string]any{"name": tool, "arguments": arguments},
			}}
		}
		if finish == "" {
			finish = natural
		}
		response = map[string]any{
			"id": "chat", "object": "chat.completion", "model": "deepseek-test",
			"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
	case "anthropic":
		content := []any{map[string]any{"type": "thinking", "thinking": "Thinking it through.", "signature": fmt.Sprintf("ds-%d", call)}}
		natural := "end_turn"
		if tool == "" {
			content = append(content, map[string]any{"type": "text", "text": "Here is my answer in prose."})
		} else {
			natural = "tool_use"
			content = append(content, toolUseBlock(fmt.Sprintf("toolu-%d", call), tool, arguments))
		}
		if finish == "" {
			finish = natural
		}
		response = map[string]any{
			"id": "msg", "type": "message", "role": "assistant", "model": "deepseek-test",
			"content": content, "stop_reason": finish,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
		}
	}
	_ = testutil.WriteInferenceStream(w, d.protocol, response)
}

func deepSeekClient(t *testing.T, server *httptest.Server, protocol string) *client {
	t.Helper()
	c, err := New(t.Context(), ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "fixture-key", Model: "deepseek-test",
		Protocol: protocol, MaxOutputTokens: 1024, MaxAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c.(*client)
}

// DeepSeek's thinking mode rejects forced tool choice, so every request a
// review makes must leave tool selection automatic: exploration turns,
// natural submissions, the single-call structured path, and each wrap-up.
func TestDeepSeekThinkingModeUsesAutomaticToolChoice(t *testing.T) {
	type answer struct {
		Answer string `json:"answer"`
	}
	exploreThenSubmit := func(request int) (string, string, string) {
		if request == 1 {
			return "read", `{"path":"a.go"}`, ""
		}
		return "submit", `{"answer":"done"}`, ""
	}
	for _, protocol := range []string{"chat", "anthropic"} {
		for _, mode := range []string{"two-turn", "wrap-up", "structured", "structured-retry", "deadline", "budget"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				respond := exploreThenSubmit
				switch mode {
				case "structured", "deadline", "budget":
					respond = func(int) (string, string, string) { return "submit", `{"answer":"done"}`, "" }
				case "structured-retry":
					respond = func(request int) (string, string, string) {
						if request == 1 {
							return "", "", ""
						}
						return "submit", `{"answer":"done"}`, ""
					}
				}
				server, fixture := newDeepSeekThinkingServer(t, protocol, respond)
				c := deepSeekClient(t, server, protocol)
				opts := ConverseOpts{MaxTurns: 4, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
					return "package a", nil
				})}
				switch mode {
				case "wrap-up":
					opts.MaxTurns = 2
				case "deadline":
					opts.WorkDeadline = time.Now().Add(-time.Second)
				case "budget":
					// Refuses exploration but admits the reserved wrap-up call.
					c.cfg.Ledger = NewLedger(4000)
				}
				var out answer
				var err error
				if strings.HasPrefix(mode, "structured") {
					err = c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
				} else {
					err = c.Converse(t.Context(), "system", "user", opts, &out)
				}
				if err != nil || out.Answer != "done" {
					t.Fatalf("answer=%q error=%v tool choices=%v", out.Answer, err, fixture.choices)
				}
			})
		}
	}
}

func TestDeepSeekFixtureRejectsForcedToolChoice(t *testing.T) {
	for protocol, choices := range map[string][]string{
		"chat":      {`"required"`, `{"type":"function","function":{"name":"submit"}}`},
		"anthropic": {`{"type":"any"}`, `{"type":"tool","name":"submit"}`},
	} {
		server, _ := newDeepSeekThinkingServer(t, protocol, func(int) (string, string, string) { return "submit", `{}`, "" })
		for _, choice := range choices {
			resp, err := http.Post(server.URL+"/v1/messages", "application/json", strings.NewReader(`{"tool_choice":`+choice+`,"tools":[{}],"messages":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			var body strings.Builder
			_, _ = io.Copy(&body, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body.String(), "Thinking mode does not support this tool_choice") {
				t.Fatalf("%s %s: status=%d body=%s", protocol, choice, resp.StatusCode, body.String())
			}
		}
	}
}

// When a thinking model's turn ends without a usable submission, the error
// must say why rather than only that the loop ended.
func TestNoSubmissionNamesTheCause(t *testing.T) {
	for _, protocol := range []string{"chat", "anthropic"} {
		t.Run(protocol+"/truncated", func(t *testing.T) {
			finish := "length"
			if protocol == "anthropic" {
				finish = "max_tokens"
			}
			server, _ := newDeepSeekThinkingServer(t, protocol, func(int) (string, string, string) { return "", "", finish })
			var out struct{ Answer string }
			err := deepSeekClient(t, server, protocol).CompleteStructured(t.Context(), "system", "user", "submit", &out)
			if !errors.Is(err, ErrNoFinalCall) || !strings.Contains(err.Error(), `finish reason "length"`) || !strings.Contains(err.Error(), "max_output_tokens") {
				t.Fatalf("error = %v, want the truncation named", err)
			}
		})
		t.Run(protocol+"/prose", func(t *testing.T) {
			server, _ := newDeepSeekThinkingServer(t, protocol, func(int) (string, string, string) { return "", "", "" })
			var out struct{ Answer string }
			err := deepSeekClient(t, server, protocol).CompleteStructured(t.Context(), "system", "user", "submit", &out)
			if !errors.Is(err, ErrNoFinalCall) || !strings.Contains(err.Error(), "deepseek-test") || !strings.Contains(err.Error(), "2 submission attempts") || !strings.Contains(err.Error(), "without calling `submit`") {
				t.Fatalf("error = %v, want the missing tool call named", err)
			}
		})
	}
	t.Run("provider-rejection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(deepSeekToolChoiceError))
		}))
		defer server.Close()
		c := deepSeekClient(t, server, "chat")
		var out struct{ Answer string }
		err := c.Converse(t.Context(), "system", "user", ConverseOpts{
			MaxTurns: 4, FinalTool: "submit", WorkDeadline: time.Now().Add(-time.Second), Tools: basicTools(nil),
		}, &out)
		if !errors.Is(err, ErrWorkDeadline) || !strings.Contains(err.Error(), "Thinking mode does not support this tool_choice") {
			t.Fatalf("error = %v, want the provider's error body", err)
		}
		if strings.HasPrefix(err.Error(), ErrNoFinalCall.Error()) || strings.Count(err.Error(), ErrNoFinalCall.Error()) > 1 {
			t.Fatalf("error = %v, want the provider error ahead of a single generic sentinel", err)
		}
	})
}

// newReasoningOnlyServer answers every request the way a thinking model does
// when its reasoning spends the whole output ceiling: reasoning content only,
// no text and no tool call, then the given finish reason.
func newReasoningOnlyServer(t *testing.T, protocol, finish, signature string) (*httptest.Server, *atomic.Int32) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
		var response map[string]any
		switch protocol {
		case "chat":
			response = map[string]any{
				"id": "chat", "object": "chat.completion", "model": "deepseek-test",
				"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": map[string]any{
					"role": "assistant", "reasoning_content": "Checking each call site before deciding",
				}}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 256, "total_tokens": 266},
			}
		case "anthropic":
			response = map[string]any{
				"id": "msg", "type": "message", "role": "assistant", "model": "deepseek-test",
				"content": []any{map[string]any{
					"type": "thinking", "thinking": "Checking each call site before deciding", "signature": signature,
				}},
				"stop_reason": finish,
				"usage":       map[string]any{"input_tokens": 10, "output_tokens": 256},
			}
		}
		_ = testutil.WriteInferenceStream(w, protocol, response)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// A thinking model can spend its whole output ceiling on reasoning and stop
// with a length finish before calling any tool. The error must name that
// finish once and point at max_output_tokens, on the exploration turn and on
// the single-call structured path, without spending retries on it.
func TestReasoningOnlyLengthFinishNamesTheCeiling(t *testing.T) {
	type variant struct{ protocol, finish, signature string }
	for name, v := range map[string]variant{
		"chat":               {"chat", "length", ""},
		"anthropic/signed":   {"anthropic", "max_tokens", "ds-1"},
		"anthropic/unsigned": {"anthropic", "max_tokens", ""},
	} {
		for _, mode := range []string{"converse", "structured"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				server, requests := newReasoningOnlyServer(t, v.protocol, v.finish, v.signature)
				c := deepSeekClient(t, server, v.protocol)
				var out struct {
					Answer string `json:"answer"`
				}
				var err error
				if mode == "structured" {
					err = c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
				} else {
					err = c.Converse(t.Context(), "system", "user", ConverseOpts{
						MaxTurns: 4, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
							t.Error("exploration tool ran after a reasoning-only response")
							return nil, nil
						}),
					}, &out)
				}
				if err == nil {
					t.Fatal("reasoning-only response produced no error")
				}
				message := err.Error()
				t.Logf("error: %s", message)
				if !errors.Is(err, ErrNoFinalCall) || errors.Is(err, ErrEndpoint) {
					t.Fatalf("error = %v, want a no-submission error, not an endpoint failure", err)
				}
				if !strings.Contains(message, "deepseek-test") || !strings.Contains(message, `finish reason "length"`) || !strings.Contains(message, "raise max_output_tokens") {
					t.Fatalf("error = %v, want the length finish and the max_output_tokens hint", err)
				}
				if strings.Count(message, "finish reason") != 1 || strings.Count(message, "max_output_tokens") != 1 {
					t.Fatalf("error = %v, want the cause stated once", err)
				}
				if got := requests.Load(); got != 1 {
					t.Fatalf("requests = %d, want 1: a length finish is not retried", got)
				}
			})
		}
	}
}

// Fantasy maps finish reasons it does not model specifically, such as
// DeepSeek's insufficient_system_resource, to a generic category. The error
// must also name the provider's own reason, and only a length finish suggests
// raising max_output_tokens.
func TestAbnormalFinishNamesTheProviderReason(t *testing.T) {
	for finish, category := range map[string]string{
		"insufficient_system_resource": "error",
		"content_filter":               "content-filter",
		"unrecognized_reason":          "unknown",
	} {
		t.Run(finish, func(t *testing.T) {
			server, _ := newReasoningOnlyServer(t, "chat", finish, "")
			var out struct{ Answer string }
			err := deepSeekClient(t, server, "chat").CompleteStructured(t.Context(), "system", "user", "submit", &out)
			if err == nil {
				t.Fatal("abnormal finish produced no error")
			}
			message := err.Error()
			t.Logf("error: %s", message)
			want := fmt.Sprintf("finish reason %q (the provider sent %q)", category, finish)
			if !errors.Is(err, ErrNoFinalCall) || !strings.Contains(message, want) || strings.Count(message, finish) != 1 {
				t.Fatalf("error = %v, want %s stated once", err, want)
			}
			if strings.Contains(message, "max_output_tokens") {
				t.Fatalf("error = %v, want no output-ceiling hint for a non-length finish", err)
			}
		})
	}
}

// bindingServer is a native Anthropic Messages fixture that enforces signed
// thinking-block binding the way Claude 5.1+ models do: a block stays valid
// only while the system prompt, the tool set, and every content block before
// it are unchanged. Content is compared after a JSON round trip, so key order
// and whitespace are ignored but any added, removed, edited, or reordered
// block is a mismatch.
type bindingServer struct {
	t       *testing.T
	respond func(request int) []any

	mu       sync.Mutex
	requests []bindingRequest
	issued   map[string]string
}

type bindingRequest struct {
	System     json.RawMessage `json:"system"`
	Tools      json.RawMessage `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Messages   []struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	} `json:"messages"`
}

func newBindingServer(t *testing.T, respond func(request int) []any) *httptest.Server {
	b := &bindingServer{t: t, respond: respond, issued: map[string]string{}}
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	return server
}

func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Errorf("decode request JSON: %v", err)
		return string(raw)
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func (b *bindingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var request bindingRequest
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, request)

	var prefix bytes.Buffer
	prefix.WriteString(canonicalJSON(b.t, request.System) + "\n" + canonicalJSON(b.t, request.Tools) + "\n")
	for i, message := range request.Messages {
		prefix.WriteString(message.Role + ":")
		for j, block := range message.Content {
			var head struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			}
			_ = json.Unmarshal(block, &head)
			if head.Type == "thinking" {
				if want, ok := b.issued[head.Signature]; !ok || want != prefix.String() {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					message := fmt.Sprintf("messages.%d.content.%d: Invalid `signature` in `thinking` block. The block is bound to a different conversation. Remove the block, or set `thinking.block_binding.prefix_mismatch_behavior` to \"drop_block\". That setting requires the `thinking-binding-controls-2026-08-01` value in the `anthropic-beta` header.", i, j)
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": message}})
					return
				}
			}
			prefix.WriteString(canonicalJSON(b.t, block) + "\n")
		}
	}

	content := b.respond(len(b.requests))
	prefix.WriteString("assistant:")
	stop := "end_turn"
	for _, raw := range content {
		block := raw.(map[string]any)
		switch block["type"] {
		case "thinking":
			signature := fmt.Sprintf("sig-%d", len(b.issued)+1)
			block["signature"] = signature
			b.issued[signature] = prefix.String()
		case "tool_use":
			stop = "tool_use"
		}
		data, _ := json.Marshal(block)
		prefix.WriteString(canonicalJSON(b.t, data) + "\n")
	}
	_ = testutil.WriteInferenceStream(w, "anthropic", map[string]any{
		"id": fmt.Sprintf("msg-%d", len(b.requests)), "type": "message", "role": "assistant", "model": "claude-test",
		"content": content, "stop_reason": stop,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
}

func thinkingBlock(text string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": text}
}

func toolUseBlock(id, name, arguments string) map[string]any {
	var input map[string]any
	_ = json.Unmarshal([]byte(arguments), &input)
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

func bindingClient(t *testing.T, server *httptest.Server) Client {
	t.Helper()
	c, err := New(t.Context(), ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "fixture-key", Model: "claude-test",
		Protocol: "anthropic", MaxOutputTokens: 1024, MaxAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Every follow-up request must replay what each signed thinking block was
// produced under: the same system prompt, the same tools, and the same
// earlier content, including any reminder Roast added to the conversation.
func TestAnthropicReplayKeepsSignedThinkingPrefix(t *testing.T) {
	readTool := func(reads *int) Tool {
		return Tool{Name: "read", Description: "read a file", Schema: map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
		}, Run: func(_ context.Context, args map[string]any) (any, error) {
			*reads++
			return map[string]any{"content": "package " + fmt.Sprint(args["path"])}, nil
		}}
	}
	submitTool := Tool{Name: "submit", Description: "return the result", Schema: map[string]any{
		"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}},
		"required": []string{"answer"},
	}}
	type answer struct {
		Answer string `json:"answer"`
	}

	t.Run("structured-retry", func(t *testing.T) {
		server := newBindingServer(t, func(request int) []any {
			if request == 1 {
				return []any{thinkingBlock(""), toolUseBlock("toolu-1", "submit", `{"answer":42}`)}
			}
			return []any{thinkingBlock(""), toolUseBlock("toolu-2", "submit", `{"answer":"done"}`)}
		})
		var out answer
		if err := bindingClient(t, server).CompleteStructured(t.Context(), "system", "user", "submit", &out); err != nil || out.Answer != "done" {
			t.Fatalf("answer=%q error=%v", out.Answer, err)
		}
	})

	t.Run("wrap-up-turn", func(t *testing.T) {
		server := newBindingServer(t, func(request int) []any {
			if request == 1 {
				return []any{thinkingBlock("look first"), toolUseBlock("toolu-1", "read", `{"path":"a"}`)}
			}
			return []any{thinkingBlock(""), toolUseBlock("toolu-2", "submit", `{"answer":"done"}`)}
		})
		var reads int
		var out answer
		err := bindingClient(t, server).Converse(t.Context(), "system", "user", ConverseOpts{
			MaxTurns: 2, FinalTool: "submit", Tools: []Tool{readTool(&reads), submitTool},
		}, &out)
		if err != nil || out.Answer != "done" || reads != 1 {
			t.Fatalf("answer=%q reads=%d error=%v", out.Answer, reads, err)
		}
	})

	t.Run("interleaved-thinking", func(t *testing.T) {
		server := newBindingServer(t, func(request int) []any {
			if request == 1 {
				return []any{
					thinkingBlock("first"), toolUseBlock("toolu-a", "read", `{"path":"a"}`),
					thinkingBlock("between tools"), toolUseBlock("toolu-b", "read", `{"path":"b"}`),
				}
			}
			return []any{toolUseBlock("toolu-c", "submit", `{"answer":"done"}`)}
		})
		var reads int
		var out answer
		err := bindingClient(t, server).Converse(t.Context(), "system", "user", ConverseOpts{
			MaxTurns: 4, FinalTool: "submit", Tools: []Tool{readTool(&reads), submitTool},
		}, &out)
		if err != nil || out.Answer != "done" || reads != 2 {
			t.Fatalf("answer=%q reads=%d error=%v", out.Answer, reads, err)
		}
	})

	t.Run("input-budget-warning", func(t *testing.T) {
		server := newBindingServer(t, func(request int) []any {
			if request < 3 {
				return []any{thinkingBlock(""), toolUseBlock(fmt.Sprintf("toolu-%d", request), "read", `{"path":"a"}`)}
			}
			return []any{toolUseBlock("toolu-3", "submit", `{"answer":"done"}`)}
		})
		var reads int
		var out answer
		err := bindingClient(t, server).Converse(t.Context(), "system", "user", ConverseOpts{
			MaxTurns: 6, FinalTool: "submit", InputByteCeiling: 1, Tools: []Tool{readTool(&reads), submitTool},
		}, &out)
		if err != nil || out.Answer != "done" {
			t.Fatalf("answer=%q reads=%d error=%v", out.Answer, reads, err)
		}
	})

	t.Run("wrap-up-records-result", func(t *testing.T) {
		// Recording on the wrap-up turn runs and is replayed unchanged.
		server := newBindingServer(t, func(request int) []any {
			if request == 1 {
				return []any{thinkingBlock("save it first"), toolUseBlock("toolu-1", "record", `{"note":"a"}`)}
			}
			return []any{thinkingBlock(""), toolUseBlock("toolu-2", "submit", `{"answer":"done"}`)}
		})
		var reads, records int
		var out answer
		err := bindingClient(t, server).Converse(t.Context(), "system", "user", ConverseOpts{
			MaxTurns: 1, FinalTool: "submit", Tools: []Tool{readTool(&reads), {
				Name: "record", Description: "record a note", RecordsResult: true,
				Schema: map[string]any{"type": "object", "properties": map[string]any{"note": map[string]any{"type": "string"}}},
				Run: func(context.Context, map[string]any) (any, error) {
					records++
					return map[string]any{"recorded": true}, nil
				},
			}, submitTool},
		}, &out)
		if err != nil || out.Answer != "done" || records != 1 || reads != 0 {
			t.Fatalf("answer=%q records=%d reads=%d error=%v", out.Answer, records, reads, err)
		}
	})

	t.Run("final-turn-exploration", func(t *testing.T) {
		// The model ignores the wrap-up notice once. Its exploration call is
		// answered, not executed, and the replay keeps the call it made.
		server := newBindingServer(t, func(request int) []any {
			if request == 1 {
				return []any{thinkingBlock(""), toolUseBlock("toolu-1", "read", `{"path":"a"}`), thinkingBlock("after the call")}
			}
			return []any{toolUseBlock("toolu-2", "submit", `{"answer":"done"}`)}
		})
		var reads int
		var out answer
		err := bindingClient(t, server).Converse(t.Context(), "system", "user", ConverseOpts{
			MaxTurns: 1, FinalTool: "submit", Tools: []Tool{readTool(&reads), submitTool},
		}, &out)
		if err != nil || out.Answer != "done" || reads != 0 {
			t.Fatalf("answer=%q reads=%d error=%v", out.Answer, reads, err)
		}
	})
}

func TestBindingServerRejectsChangedPrefix(t *testing.T) {
	server := newBindingServer(t, func(int) []any {
		return []any{thinkingBlock(""), toolUseBlock("toolu-1", "read", `{}`)}
	})
	post := func(body string) (int, string) {
		resp, err := http.Post(server.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	user := `{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`
	if code, _ := post(`{"system":[],"tools":[],"messages":[` + user + `]}`); code != http.StatusOK {
		t.Fatalf("first request status = %d", code)
	}
	assistant := `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig-1"},{"type":"tool_use","id":"toolu-1","name":"read","input":{}}]}`
	result := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu-1","content":"x"}]}`
	if code, body := post(`{"system":[],"tools":[],"messages":[` + user + `,` + assistant + `,` + result + `]}`); code != http.StatusOK {
		t.Fatalf("unchanged replay status = %d: %s", code, body)
	}
	dropped := `{"role":"user","content":[{"type":"text","text":"a"}]}`
	code, body := post(`{"system":[],"tools":[],"messages":[` + dropped + `,` + assistant + `,` + result + `]}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "messages.1.content.0: Invalid `signature` in `thinking` block") {
		t.Fatalf("changed prefix status = %d: %s", code, body)
	}
}

// Live tests run a minimal two-turn review loop against a real endpoint. They
// are opt-in: each skips unless its ROAST_LIVE_*_API_KEY is set. Keys are
// never logged; failures report only tool choices and provider findings.

func liveSetting(name, fallback string) string {
	if value := strings.TrimSpace(liveEnv[name]); value != "" {
		return value
	}
	return fallback
}

// liveRecorder fronts the real endpoint. It records each request's tool
// choice and, for native Anthropic, asks the API to report thinking blocks
// whose bound prefix changed. With prefix_mismatch_behavior unset the request
// is otherwise unchanged: accounts that enforce binding still reject it, and
// accounts that do not list the block in input_transformations.
type liveRecorder struct {
	target      string
	bindingBeta bool

	mu          sync.Mutex
	choices     []string
	thinking    int
	transformed []string
}

func (l *liveRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var request struct {
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	_ = json.Unmarshal(body, &request)
	l.mu.Lock()
	l.choices = append(l.choices, string(request.ToolChoice))
	l.mu.Unlock()

	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, l.target+strings.TrimPrefix(r.URL.Path, "/v1"), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	upstream.Header = r.Header.Clone()
	upstream.Header.Del("Accept-Encoding")
	if l.bindingBeta {
		upstream.Header.Add("anthropic-beta", "thinking-binding-controls-2026-08-01")
	}
	// Never follow a redirect: it would carry the live credential to another
	// origin, which Roast's own client refuses to do.
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(upstream)
	if err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "upstream read failed", http.StatusBadGateway)
		return
	}
	if l.bindingBeta && resp.StatusCode == http.StatusOK {
		l.inspectAnthropicStream(data)
	}
	for name, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

func (l *liveRecorder) inspectAnthropicStream(data []byte) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		payload, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Message struct {
				InputTransformations []json.RawMessage `json:"input_transformations"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		l.mu.Lock()
		if event.Type == "content_block_start" && event.ContentBlock.Type == "thinking" {
			l.thinking++
		}
		for _, entry := range event.Message.InputTransformations {
			l.transformed = append(l.transformed, string(entry))
		}
		l.mu.Unlock()
	}
}

// runLiveTwoTurn runs one exploration call, its replayed result, and the
// submission on the reserved wrap-up turn: the path every review relies on.
func runLiveTwoTurn(t *testing.T, cfg ClientConfig, recorder *liveRecorder) {
	t.Helper()
	server := httptest.NewServer(recorder)
	defer server.Close()
	cfg.BaseURL = server.URL + "/v1"
	cfg.MaxOutputTokens = 1024
	cfg.MaxAttempts = 2
	cfg.RequestTimeout = 2 * time.Minute
	c, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	token := rand.Text()
	read := false
	var out struct {
		Primes int    `json:"primes"`
		Token  string `json:"token"`
	}
	err = c.Converse(t.Context(),
		// The arithmetic invites a thinking block on the first turn, which is
		// what the replay check needs; adaptive models may still skip it.
		"You are checking a tool loop. Before calling any tool, think carefully: how many of 2^31-1, 2^61-1, and 2^67-1 are prime? Then call read_note exactly once. Then call submit with primes set to that count and token set to the token read_note returned.",
		"Begin.",
		ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: []Tool{
			{Name: "read_note", Description: "Read the note that holds the token.", Schema: map[string]any{
				"type": "object", "properties": map[string]any{}, "additionalProperties": false,
			}, Run: func(context.Context, map[string]any) (any, error) {
				read = true
				return map[string]any{"token": token}, nil
			}},
			{Name: "submit", Description: "Submit the result.", Schema: reflectSchema(&out)},
		}}, &out)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	t.Logf("requests=%d thinking blocks=%d tool choices=%v", len(recorder.choices), recorder.thinking, recorder.choices)
	if err != nil {
		t.Fatalf("two-turn loop failed: %v", err)
	}
	if !read || out.Token != token || len(recorder.choices) < 2 {
		t.Fatalf("tool result did not round-trip: read=%v token match=%v requests=%d", read, out.Token == token, len(recorder.choices))
	}
	for _, choice := range recorder.choices {
		if choice != "" && choice != `"auto"` && !strings.Contains(choice, `"type":"auto"`) {
			t.Fatalf("request forced tool choice %s", choice)
		}
	}
	if len(recorder.transformed) > 0 {
		t.Fatalf("API reported replayed thinking blocks bound to a different prefix: %v", recorder.transformed)
	}
	if recorder.bindingBeta && recorder.thinking == 0 {
		t.Log("no thinking block this run, so the replay-binding check was vacuous")
	}
}

func TestLiveAnthropicTwoTurnReview(t *testing.T) {
	key := liveSetting("ROAST_LIVE_ANTHROPIC_API_KEY", "")
	if key == "" {
		t.Skip("set ROAST_LIVE_ANTHROPIC_API_KEY to run against a native Anthropic endpoint")
	}
	base := strings.TrimRight(liveSetting("ROAST_LIVE_ANTHROPIC_BASE_URL", "https://api.anthropic.com/v1"), "/")
	for _, model := range strings.Split(liveSetting("ROAST_LIVE_ANTHROPIC_MODELS", "claude-sonnet-5-5"), ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		t.Run(model, func(t *testing.T) {
			runLiveTwoTurn(t, ClientConfig{Role: "finder", APIKey: key, Model: model, Protocol: "anthropic"},
				&liveRecorder{target: base, bindingBeta: true})
		})
	}
}

func TestLiveDeepSeekTwoTurnReview(t *testing.T) {
	key := liveSetting("ROAST_LIVE_DEEPSEEK_API_KEY", "")
	if key == "" {
		t.Skip("set ROAST_LIVE_DEEPSEEK_API_KEY to run against a DeepSeek endpoint")
	}
	base := strings.TrimRight(liveSetting("ROAST_LIVE_DEEPSEEK_BASE_URL", "https://api.deepseek.com/v1"), "/")
	runLiveTwoTurn(t, ClientConfig{
		Role: "finder", APIKey: key,
		Model:    liveSetting("ROAST_LIVE_DEEPSEEK_MODEL", "deepseek-v4-pro"),
		Protocol: liveSetting("ROAST_LIVE_DEEPSEEK_PROTOCOL", "chat"),
	}, &liveRecorder{target: base})
}

// A thinking model given too small an output ceiling spends it on reasoning
// and stops before calling a tool. The error must name the length finish and
// the setting to raise, not report a generic failure.
func TestLiveDeepSeekReasoningCeiling(t *testing.T) {
	key := liveSetting("ROAST_LIVE_DEEPSEEK_API_KEY", "")
	if key == "" {
		t.Skip("set ROAST_LIVE_DEEPSEEK_API_KEY to run against a DeepSeek endpoint")
	}
	recorder := &liveRecorder{target: strings.TrimRight(liveSetting("ROAST_LIVE_DEEPSEEK_BASE_URL", "https://api.deepseek.com/v1"), "/")}
	server := httptest.NewServer(recorder)
	defer server.Close()
	c, err := New(t.Context(), ClientConfig{
		Role: "finder", APIKey: key, BaseURL: server.URL + "/v1",
		Model:           liveSetting("ROAST_LIVE_DEEPSEEK_MODEL", "deepseek-v4-pro"),
		Protocol:        liveSetting("ROAST_LIVE_DEEPSEEK_PROTOCOL", "chat"),
		MaxOutputTokens: 256, MaxAttempts: 1, RequestTimeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Primes int `json:"primes"`
	}
	err = c.CompleteStructured(t.Context(),
		"Before calling any tool, think carefully and at length: for each of 2^31-1, 2^61-1, and 2^67-1, decide whether it is prime and check your reasoning. Then call submit with primes set to that count.",
		"Begin.", "submit", &out)
	if err == nil {
		t.Skipf("the model submitted within a 256-token ceiling (primes=%d), so the length path was not exercised", out.Primes)
	}
	t.Logf("error: %v", err)
	if !errors.Is(err, ErrNoFinalCall) || !strings.Contains(err.Error(), `finish reason "length"`) || !strings.Contains(err.Error(), "raise max_output_tokens") {
		t.Fatalf("error = %v, want the length finish named with the max_output_tokens hint", err)
	}
}

// This is a deliberate opt-in to use Roast's separately authorized account.
// It never accepts token environment variables or reads Codex credentials.
func TestLiveChatGPTTwoTurnReview(t *testing.T) {
	if liveEnv["ROAST_LIVE_CHATGPT"] != "1" {
		t.Skip("set ROAST_LIVE_CHATGPT=1 after roast chatgpt signin to test the direct plan route")
	}
	ran := false
	for _, model := range strings.Split(liveSetting("ROAST_LIVE_CHATGPT_MODELS", "gpt-6.1-sol"), ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		ran = true
		t.Run(model, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
			defer cancel()
			c, err := New(ctx, ClientConfig{Protocol: "chatgpt", Model: model, MaxOutputTokens: 2048, MaxAttempts: 1, RequestTimeout: 2 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			token := rand.Text()
			read := false
			var out struct {
				Token string `json:"token"`
			}
			err = c.Converse(ctx, "Test a native function-tool loop. Call read_note once, then submit the token it returned.", "Begin the check.", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: []Tool{
				{Name: "read_note", Description: "Read the diagnostic token.", Schema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}, Run: func(context.Context, map[string]any) (any, error) {
					read = true
					return map[string]string{"token": token}, nil
				}},
				{Name: "submit", Description: "Submit the diagnostic token.", Schema: reflectSchema(&out)},
			}}, &out)
			if err != nil {
				t.Fatal(err)
			}
			if !read || out.Token != token || c.Usage().Calls < 2 {
				t.Fatal("live native tool result did not round-trip")
			}
			t.Logf("direct plan route passed: %d model calls", c.Usage().Calls)
		})
	}
	if !ran {
		t.Fatal("ROAST_LIVE_CHATGPT_MODELS contains no model slugs")
	}
}

// liveEnv holds the opt-in ROAST_LIVE_* settings. They are read before the
// ambient environment is cleared, so no other test can see them.
var liveEnv = map[string]string{}

func TestMain(m *testing.M) {
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "ROAST_LIVE_") {
			liveEnv[name] = value
		}
	}
	testutil.ClearAmbientEnvironment()
	os.Exit(m.Run())
}
