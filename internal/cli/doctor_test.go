package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/config"
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
