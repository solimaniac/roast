package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestDoctorEndpointCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	result := doctorEndpointCheck(t.Context(), server.Client(), "finder", config.ResolvedRole{
		BaseURL: server.URL + "/v1", APIKey: "secret", Protocol: "responses",
	})
	if result.Status != doctorPass || strings.Contains(result.Detail, "secret") {
		t.Fatalf("doctorEndpointCheck() = %#v", result)
	}
}

func TestDoctorJSON(t *testing.T) {
	server, _ := newDoctorProbeServer(t, "responses")
	t.Setenv("DOCTOR_KEY", "secret")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	body := "version: 1\ninference_profiles:\n  finder:\n    endpoint: " + server.URL + "/v1\n    api_key_env: DOCTOR_KEY\n    model: finder-model\n    protocol: responses\n  verifier:\n    endpoint: " + server.URL + "/v1\n    api_key_env: DOCTOR_KEY\n    model: verifier-model\n    protocol: responses\nroles:\n  finder:\n    profile: finder\n  verifier:\n    profile: verifier\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"doctor", "--json", "--config", configPath}, &stdout, &stderr)
	if err != nil || code != ExitClean {
		t.Fatalf("doctor code=%d err=%v stdout=%s stderr=%s", code, err, stdout.String(), stderr.String())
	}
	var results []doctorResult
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatalf("decode results: %v: %s", err, stdout.String())
	}
	if len(results) < 8 {
		t.Fatalf("doctor returned only %d results: %#v", len(results), results)
	}
	if strings.Contains(stdout.String(), "secret") {
		t.Fatal("doctor output exposed a credential")
	}
	sources := map[string]doctorResult{}
	for _, result := range results {
		sources[result.Name] = result
	}
	for _, role := range []string{"finder", "verifier"} {
		if got := sources[role+"-credential"]; got.Status != doctorPass || got.Detail != "DOCTOR_KEY (profile api_key_env)" {
			t.Errorf("%s credential = %#v", role, got)
		}
	}
}

func TestDoctorReportsWithheldAmbientCredential(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "anthropic-secret" {
			t.Errorf("ambient pairing did not reach its own base URL: %s", r.URL.Path)
		}
		http.Error(w, "probe only", http.StatusServiceUnavailable)
	}))
	defer gateway.Close()
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
	t.Setenv("ANTHROPIC_BASE_URL", gateway.URL+"/v1")
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	body := "version: 1\ninference_profiles:\n  finder:\n    endpoint: https://template.example/v1\n    model: finder-model\n  verifier:\n    model: verifier-model\n    protocol: anthropic\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	results := doctorChecks(t.Context(), configPath)
	byName := map[string]doctorResult{}
	for _, result := range results {
		byName[result.Name] = result
	}
	finder := byName["finder-credential"]
	if finder.Status != doctorFail || !strings.Contains(finder.Detail, "OPENAI_API_KEY is set but is sent only to its provider's default endpoint or OPENAI_BASE_URL") {
		t.Fatalf("finder credential = %#v", finder)
	}
	if profile := byName["finder-profile"]; profile.Status != doctorFail || !strings.Contains(profile.Detail, "is not sent to https://template.example/v1") {
		t.Fatalf("finder profile = %#v", profile)
	}
	if verifier := byName["verifier-credential"]; verifier.Status != doctorPass || verifier.Detail != "ANTHROPIC_API_KEY (ambient provider key)" {
		t.Fatalf("verifier credential = %#v", verifier)
	}
	if _, probed := byName["finder-endpoint"]; probed {
		t.Fatal("doctor probed an endpoint without a credential")
	}
	for _, result := range results {
		if strings.Contains(result.Detail, "secret") {
			t.Fatalf("doctor exposed a credential: %#v", result)
		}
	}
}

func TestDoctorRoundtripUsesConfiguredResponsesRoute(t *testing.T) {
	_, fixture := newDoctorProbeServer(t, "responses")
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		fixture.ServeHTTP(w, r)
	}))
	defer server.Close()
	result := doctorRoundtripCheck(t.Context(), "finder", config.ResolvedRole{
		BaseURL: server.URL + "/v1", APIKey: "secret", Model: "finder-model",
		Protocol: "responses", MaxOutputTokens: 1024,
	})
	if result.Status != doctorPass || len(paths) != 2 {
		t.Fatalf("doctorRoundtripCheck() = %#v, paths=%v", result, paths)
	}
}

func TestDoctorReportsAndSendsSessionHeader(t *testing.T) {
	probes := map[string]*doctorProbeServer{
		"finder-model":   {t: t, protocol: "responses"},
		"verifier-model": {t: t, protocol: "responses"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &request); err != nil || probes[request.Model] == nil {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		configured := r.Header.Get("X-Opencode-Session")
		if want := request.Model == "finder-model"; want != (configured != "") || configured != "" && configured != r.Header.Get("X-Agent-Session-Id") {
			http.Error(w, "session_header was not applied to exactly the finder profile", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		probes[request.Model].ServeHTTP(w, r)
	}))
	defer server.Close()
	t.Setenv("DOCTOR_KEY", "secret")
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	body := "version: 1\ninference_profiles:\n  finder:\n    endpoint: " + server.URL + "/v1\n    api_key_env: DOCTOR_KEY\n    model: finder-model\n    session_header: x-opencode-session\n  verifier:\n    endpoint: " + server.URL + "/v1\n    api_key_env: DOCTOR_KEY\n    model: verifier-model\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	byName := map[string]doctorResult{}
	for _, result := range doctorChecks(t.Context(), configPath) {
		byName[result.Name] = result
	}
	if finder := byName["finder-profile"]; finder.Status != doctorPass || !strings.HasSuffix(finder.Detail, ", session header x-opencode-session") {
		t.Fatalf("finder profile = %#v", finder)
	}
	if verifier := byName["verifier-profile"]; verifier.Status != doctorPass || strings.Contains(verifier.Detail, "session header") {
		t.Fatalf("verifier profile = %#v", verifier)
	}
	for _, role := range []string{"finder", "verifier"} {
		if roundtrip := byName[role+"-roundtrip"]; roundtrip.Status != doctorPass {
			t.Errorf("%s roundtrip = %#v", role, roundtrip)
		}
	}
}

var doctorTokenPattern = regexp.MustCompile(`roast-doctor-[A-Z2-7]+`)

// doctorProbeServer answers the roundtrip probe the way a thinking model
// would: the first turn calls the lookup tool (with a signed thinking block on
// the Anthropic protocol), the second submits the token it got back. It
// rejects forced tool choice, as DeepSeek's thinking mode does, and rejects a
// follow-up whose tools or opening message differ from the first request, as
// Claude 5 does for signed thinking.
type doctorProbeServer struct {
	t        *testing.T
	protocol string

	mu         sync.Mutex
	requests   int
	firstTools string
	firstInput string
	token      string
}

func newDoctorProbeServer(t *testing.T, protocol string) (*httptest.Server, *doctorProbeServer) {
	d := &doctorProbeServer{t: t, protocol: protocol}
	server := httptest.NewServer(d)
	t.Cleanup(server.Close)
	return server, d
}

func (d *doctorProbeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(`{"data":[]}`))
		return
	}
	body, _ := io.ReadAll(r.Body)
	var request struct {
		ToolChoice json.RawMessage   `json:"tool_choice"`
		Tools      json.RawMessage   `json:"tools"`
		Messages   []json.RawMessage `json:"messages"`
		Input      []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reject := func(message string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error"}})
	}
	choice := strings.TrimSpace(string(request.ToolChoice))
	if choice != `"auto"` && !strings.Contains(choice, `"type":"auto"`) {
		reject("Thinking mode does not support this tool_choice")
		return
	}
	opening := append(request.Messages, request.Input...)
	if len(opening) == 0 {
		reject("no messages")
		return
	}

	d.mu.Lock()
	d.requests++
	call := d.requests
	if call == 1 {
		d.firstTools, d.firstInput = string(request.Tools), string(opening[0])
	} else if string(request.Tools) != d.firstTools || string(opening[0]) != d.firstInput {
		d.mu.Unlock()
		reject("messages.1.content.0: Invalid `signature` in `thinking` block. The block is bound to a different conversation.")
		return
	}
	token := doctorTokenPattern.FindString(string(body))
	d.token = token
	d.mu.Unlock()

	tool, arguments := "doctor_lookup", `{}`
	if token != "" {
		tool, arguments = "doctor_probe", fmt.Sprintf(`{"ok":true,"token":%q}`, token)
	}
	w.Header().Set("Content-Type", "application/json")
	var response map[string]any
	switch d.protocol {
	case "anthropic":
		var input map[string]any
		_ = json.Unmarshal([]byte(arguments), &input)
		stop := "tool_use"
		response = map[string]any{
			"id": "msg", "type": "message", "role": "assistant", "model": "probe-model",
			"content": []any{
				map[string]any{"type": "thinking", "thinking": "", "signature": fmt.Sprintf("sig-%d", call)},
				map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu-%d", call), "name": tool, "input": input},
			},
			"stop_reason": stop, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
		}
	case "chat":
		response = map[string]any{
			"id": "chat", "object": "chat.completion", "model": "probe-model",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant", "reasoning_content": "Following the instructions.",
				"tool_calls": []any{map[string]any{
					"id": fmt.Sprintf("call-%d", call), "type": "function",
					"function": map[string]any{"name": tool, "arguments": arguments},
				}},
			}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
	case "responses":
		response = map[string]any{
			"id": fmt.Sprintf("resp-%d", call), "object": "response", "created_at": 1,
			"status": "completed", "model": "probe-model",
			"output": []any{map[string]any{
				"id": fmt.Sprintf("fc-%d", call), "type": "function_call", "call_id": fmt.Sprintf("call-%d", call),
				"name": tool, "arguments": arguments, "status": "completed",
			}},
			"usage": map[string]any{
				"input_tokens": 10, "output_tokens": 3, "total_tokens": 13,
				"input_tokens_details":  map[string]any{"cached_tokens": 0},
				"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			},
		}
	}
	_ = testutil.WriteInferenceStream(w, d.protocol, response)
}

// The probe must exercise what a review relies on: a tool call, its result
// replayed on the next request, and a final submission, all under automatic
// tool choice and an unchanged conversation prefix.
func TestDoctorRoundtripRunsTwoTurnToolLoop(t *testing.T) {
	for _, protocol := range []string{"anthropic", "chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			server, fixture := newDoctorProbeServer(t, protocol)
			result := doctorRoundtripCheck(t.Context(), "finder", config.ResolvedRole{
				BaseURL: server.URL + "/v1", APIKey: "secret", Model: "probe-model",
				Protocol: protocol, MaxOutputTokens: 32768,
			})
			if result.Status != doctorPass || fixture.requests != 2 || fixture.token == "" {
				t.Fatalf("result=%#v requests=%d token=%q", result, fixture.requests, fixture.token)
			}
		})
	}
}

func TestDoctorRoundtripRequiresTheToolResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = testutil.WriteJSONStream(w, "chat", `{"id":"chat","object":"chat.completion","model":"probe-model","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"doctor_probe","arguments":"{\"ok\":true,\"token\":\"guessed\"}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()
	result := doctorRoundtripCheck(t.Context(), "verifier", config.ResolvedRole{
		BaseURL: server.URL + "/v1", APIKey: "secret", Model: "probe-model", Protocol: "chat", MaxOutputTokens: 1024,
	})
	if result.Status != doctorFail || !strings.Contains(result.Detail, "doctor_lookup") {
		t.Fatalf("result = %#v, want a failure naming the skipped tool call", result)
	}
}
