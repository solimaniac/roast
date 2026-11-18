package llm

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/roast/internal/testutil"
)

// ambientCustomHeaders collides with every header Roast owns as well as
// carrying one that Roast never sets.
const ambientCustomHeaders = "X-Ambient-Header: junk-custom\n" +
	"Authorization: Bearer junk-custom-authorization\n" +
	"X-Api-Key: junk-custom-api-key\n" +
	"User-Agent: junk-custom-user-agent\n" +
	"X-Agent-Session-Id: junk-custom-session"

// Each scenario selects one branch of the SDK credential chains. The
// Anthropic SDK stops at the first source it finds, so no single environment
// reaches every variable. $DECOY expands to a server that must never be called.
var ambientSDKScenarios = []struct {
	name    string
	env     map[string]string
	profile string
}{
	{name: "provider keys", env: map[string]string{
		"ANTHROPIC_API_KEY":     "junk-anthropic-api-key",
		"ANTHROPIC_BASE_URL":    "$DECOY",
		"OPENAI_API_KEY":        "junk-openai-api-key",
		"OPENAI_ADMIN_KEY":      "junk-openai-admin-key",
		"OPENAI_ORG_ID":         "junk-openai-org",
		"OPENAI_PROJECT_ID":     "junk-openai-project",
		"OPENAI_WEBHOOK_SECRET": "junk-openai-webhook",
		"OPENAI_BASE_URL":       "$DECOY/v1",
		"OPENAI_CUSTOM_HEADERS": ambientCustomHeaders,
	}},
	{name: "auth token", env: map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "junk-anthropic-auth-token",
		"ANTHROPIC_BASE_URL":   "$DECOY",
		"OPENAI_ADMIN_KEY":     "junk-openai-admin-key",
		"OPENAI_ORG_ID":        "junk-openai-org",
		"OPENAI_BASE_URL":      "$DECOY/v1",
	}},
	{name: "explicit profile", profile: "ambient", env: map[string]string{
		"ANTHROPIC_PROFILE":         "ambient",
		"ANTHROPIC_ORGANIZATION_ID": "junk-anthropic-org",
	}},
	{name: "fallback profile", profile: "default", env: map[string]string{
		"ANTHROPIC_WORKSPACE_ID": "junk-anthropic-workspace-env",
	}},
	{name: "federation", env: map[string]string{
		"ANTHROPIC_FEDERATION_RULE_ID": "junk-federation-rule",
		"ANTHROPIC_ORGANIZATION_ID":    "junk-anthropic-org",
		"ANTHROPIC_IDENTITY_TOKEN":     "junk-identity-token",
		"ANTHROPIC_SERVICE_ACCOUNT_ID": "junk-service-account",
		"ANTHROPIC_WORKSPACE_ID":       "junk-anthropic-workspace-env",
		"ANTHROPIC_BASE_URL":           "$DECOY",
	}},
	{name: "custom headers", env: map[string]string{
		"ANTHROPIC_CUSTOM_HEADERS":      ambientCustomHeaders,
		"ANTHROPIC_WEBHOOK_SIGNING_KEY": "junk-anthropic-webhook",
		"ANTHROPIC_BASE_URL":            "$DECOY",
		"OPENAI_CUSTOM_HEADERS":         ambientCustomHeaders,
		"OPENAI_BASE_URL":               "$DECOY/v1",
	}},
}

func TestProvidersIgnoreAmbientSDKEnvironment(t *testing.T) {
	const configuredKey = "configured-key"
	paths := map[string]string{"chat": "/v1/chat/completions", "responses": "/v1/responses", "anthropic": "/v1/messages"}
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		for _, scenario := range ambientSDKScenarios {
			t.Run(protocol+"/"+scenario.name, func(t *testing.T) {
				decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					t.Errorf("request reached the ambient base URL: %s %s", r.Method, r.URL.Path)
					http.Error(w, "decoy", http.StatusTeapot)
				}))
				defer decoy.Close()
				setAmbientSDKEnvironment(t, scenario.env, scenario.profile, decoy.URL)

				var mu sync.Mutex
				var requests []*http.Request
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					requests = append(requests, r.Clone(r.Context()))
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					_ = testutil.WriteInferenceStream(w, protocol, structuredToolResponse(protocol, "verdict", `{"status":"ok"}`))
				}))
				defer server.Close()

				c, err := New(t.Context(), ClientConfig{
					BaseURL: server.URL + "/v1", APIKey: configuredKey, Model: "test-model",
					Protocol: protocol, MaxOutputTokens: 100, MaxAttempts: 1,
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
				if out.Status != "ok" {
					t.Fatalf("status = %q", out.Status)
				}

				mu.Lock()
				defer mu.Unlock()
				if len(requests) != 1 {
					t.Fatalf("configured endpoint received %d requests, want 1", len(requests))
				}
				r := requests[0]
				if r.URL.Path != paths[protocol] {
					t.Errorf("path = %q, want %q", r.URL.Path, paths[protocol])
				}
				wantAuthorization, wantAPIKey := "Bearer "+configuredKey, ""
				if protocol == "anthropic" {
					wantAuthorization, wantAPIKey = "", configuredKey
				}
				if got := r.Header.Values("Authorization"); strings.Join(got, ",") != wantAuthorization {
					t.Errorf("Authorization = %q, want %q", got, wantAuthorization)
				}
				if got := r.Header.Values("X-Api-Key"); strings.Join(got, ",") != wantAPIKey {
					t.Errorf("X-Api-Key = %q, want %q", got, wantAPIKey)
				}
				if got := r.Header.Get("User-Agent"); got != userAgent() {
					t.Errorf("User-Agent = %q, want %q", got, userAgent())
				}
				if got := r.Header.Values("X-Agent-Session-Id"); len(got) != 1 || !strings.HasPrefix(got[0], "roast-") {
					t.Errorf("X-Agent-Session-Id = %q, want one Roast session", got)
				}
				for _, name := range []string{"X-Ambient-Header", "OpenAI-Organization", "OpenAI-Project", "Anthropic-Workspace-Id"} {
					if got := r.Header.Values(name); len(got) != 0 {
						t.Errorf("%s = %q, want absent", name, got)
					}
				}
				for name, values := range r.Header {
					for _, value := range values {
						if strings.Contains(value, "junk") {
							t.Errorf("ambient value reached the configured endpoint: %s: %s", name, value)
						}
					}
				}
			})
		}
	}
}

func TestNewRejectsMissingAPIKey(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		for _, key := range []string{"", " "} {
			_, err := New(t.Context(), ClientConfig{
				BaseURL: "http://127.0.0.1:1/v1", APIKey: key, Model: "test-model",
				Protocol: protocol, MaxOutputTokens: 100,
			})
			if err == nil || !strings.Contains(err.Error(), "API key is not configured") {
				t.Errorf("%s with key %q: error = %v", protocol, key, err)
			}
		}
	}
}

// setAmbientSDKEnvironment clears every provider SDK variable, isolates the
// Anthropic profile directory, and applies env with $DECOY expanded.
func setAmbientSDKEnvironment(t *testing.T, env map[string]string, profile, decoyURL string) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "OPENAI_") {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
	configDir := t.TempDir()
	t.Setenv("ANTHROPIC_CONFIG_DIR", configDir)
	for name, value := range env {
		t.Setenv(name, strings.ReplaceAll(value, "$DECOY", decoyURL))
	}
	if profile == "" {
		return
	}
	if err := os.MkdirAll(filepath.Join(configDir, "configs"), 0o700); err != nil {
		t.Fatal(err)
	}
	document := `{"authentication":{"type":"user_oauth"},"base_url":"` + decoyURL + `","workspace_id":"junk-anthropic-workspace"}`
	if err := os.WriteFile(filepath.Join(configDir, "configs", profile+".json"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}

func structuredToolResponse(protocol, tool, arguments string) map[string]any {
	switch protocol {
	case "anthropic":
		return anthropicResponse(tool, arguments)
	case "responses":
		return map[string]any{
			"id": "resp-test", "object": "response", "status": "completed", "model": "test-model",
			"output": []any{map[string]any{
				"id": "fc-" + tool, "type": "function_call", "call_id": "call-" + tool,
				"name": tool, "arguments": arguments, "status": "completed",
			}},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
		}
	default:
		return map[string]any{
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
}
