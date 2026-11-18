package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/runbundle"
	"github.com/bluesky-social/roast/internal/runupload"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestConfigInitRejectsBlankInteractiveURL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newConfigInitCommand(&stdout, &stderr)
	cmd.SetIn(strings.NewReader("   \n"))
	err := cmd.Execute()
	if err == nil || err.Error() != "configuration URL is required" {
		t.Fatalf("expected required URL error, got %v", err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Configuration URL: ") {
		t.Fatalf("URL prompt must go to stderr: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestProtocolUpdateThroughConfigurationURLUsesNativeMessages(t *testing.T) {
	for _, entrypoint := range []string{"enrollment", "config init"} {
		t.Run(entrypoint, func(t *testing.T) {
			for _, name := range []string{"ROAST_FINDER_BASE_URL", "ROAST_FINDER_MODEL", "ROAST_FINDER_PROTOCOL", "ROAST_FINDER_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY"} {
				t.Setenv(name, "")
			}
			var calls atomic.Int32
			models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "local-key" {
					t.Error("migrated client did not use native Messages with the preserved credential")
					http.Error(w, "wrong inference request", http.StatusBadRequest)
					return
				}
				var request struct {
					MaxTokens int  `json:"max_tokens"`
					Stream    bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.MaxTokens != 32768 || !request.Stream {
					t.Errorf("migrated native request lost its full budget or streaming: request=%+v error=%v", request, err)
					http.Error(w, "invalid native request", http.StatusBadRequest)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = testutil.WriteJSONStream(w, "anthropic", `{"id":"msg","type":"message","role":"assistant","model":"claude-test","content":[{"type":"tool_use","id":"call","name":"verdict","input":{"ok":true}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer models.Close()
			document := fmt.Sprintf(`version: 1
inference_profiles:
  finder:
    endpoint: %s/v1
    model: claude-test
    protocol: chat
    api_key: local-key
    max_output_tokens: 32768
  verifier:
    model: gpt-test
    protocol: responses
roles:
  finder:
    profile: finder
  verifier:
    profile: verifier
upload:
  enabled: false
`, models.URL)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			template := strings.Replace(document, "    api_key: local-key\n", "", 1)
			template = strings.Replace(template, "protocol: chat", "protocol: anthropic", 1)
			configuration := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
					t.Error("template received local credentials")
				}
				_, _ = fmt.Fprint(w, template)
			}))
			defer configuration.Close()
			var output bytes.Buffer
			cmd := newConfigInitCommand(&output, &output)
			cmd.SetContext(t.Context())
			if entrypoint == "enrollment" {
				if err := offerProvision(cmd, path, path, strings.NewReader(configuration.URL+"\n"), &output); err != nil {
					t.Fatal(err)
				}
			} else {
				cmd.SetArgs([]string{"--from", configuration.URL, "--output", path})
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			role, err := cfg.ResolveRole("finder")
			if err != nil {
				t.Fatal(err)
			}
			client, err := llm.New(t.Context(), llm.ClientConfig{
				BaseURL: role.BaseURL, Model: role.Model, APIKey: role.APIKey,
				Protocol: role.Protocol, MaxOutputTokens: role.MaxOutputTokens, MaxAttempts: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				OK bool `json:"ok"`
			}
			if err := client.CompleteStructured(t.Context(), "system", "user", "verdict", &result); err != nil || !result.OK || calls.Load() != 1 {
				t.Fatalf("native call failed: result=%v calls=%d error=%v", result, calls.Load(), err)
			}
			if needs, err := config.NeedsProvision(path); err != nil || needs {
				t.Fatalf("enrollment would fire again: needs=%v error=%v", needs, err)
			}
		})
	}
}

func TestProtocolUpdateDeclineAndNoninteractiveLeaveConfigAlone(t *testing.T) {
	for _, mode := range []string{"decline", "noninteractive", "explicit config"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"HOME", "APPDATA", "XDG_CONFIG_HOME"} {
				t.Setenv(name, root)
			}
			path, err := config.UserPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			const raw = "version: 1\ninference_profiles:\n  finder:\n    endpoint: https://compat.example/v1\n    model: claude-test\n    protocol: chat\n"
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			cmd := newConfigInitCommand(&output, &output)
			cmd.SetIn(strings.NewReader("\n"))
			if mode == "decline" {
				err = offerProvision(cmd, path, path, cmd.InOrStdin(), &output)
			} else {
				explicit := ""
				if mode == "explicit config" {
					explicit = path
				}
				err = maybeProvision(cmd, explicit, &output, func() bool { return false })
				if output.Len() != 0 {
					t.Fatal("unexpected enrollment prompt")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != raw {
				t.Fatal("config changed without consent")
			}
			needs, err := config.NeedsProvision(path)
			if err != nil || needs != (mode != "decline") {
				t.Fatalf("decline/noninteractive acknowledgement: needs=%v error=%v", needs, err)
			}
		})
	}
}

// isolateUserConfig points the user config and legacy config locations at a
// fresh directory and returns the user config path.
func isolateUserConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"HOME", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(name, root)
	}
	path, err := config.UserPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// forceInteractive makes prompts behave as if stdin and stderr were terminals.
func forceInteractive(t *testing.T) {
	t.Helper()
	previous := interactive
	interactive = func(any, any) bool { return true }
	t.Cleanup(func() { interactive = previous })
}

// pipeInput returns an *os.File stdin that yields input and then EOF.
func pipeInput(t *testing.T, input string) *os.File {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader
}

func serveTemplate(t *testing.T, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
	t.Cleanup(server.Close)
	return server.URL
}

const uploadTemplate = `version: 1
inference_profiles:
  claude:
    endpoint: https://models.example/v1
    model: claude-model
    protocol: anthropic
    api_key_env: TEMPLATE_MODEL_KEY
  verifier:
    model: verifier-model
  independent:
    endpoint: https://independent.example/v1
    model: other-model
    protocol: chat
roles:
  finder:
    profile: claude
    author_routes:
      anthropic: independent
  verifier:
    profile: verifier
upload:
  enabled: true
  backend: presigned
  endpoint: https://uploads.example
  token_env: TEMPLATE_UPLOAD_TOKEN
`

func TestDevNullIsNotInteractive(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	if isTerminal(devNull) {
		t.Fatal("a character device that is not a terminal was treated as interactive")
	}
}

func TestFirstRunPromptSkipsResolvedRolesAndUsesStderr(t *testing.T) {
	forceInteractive(t)
	for _, resolves := range []bool{true, false} {
		t.Run(fmt.Sprintf("roles resolve=%v", resolves), func(t *testing.T) {
			path := isolateUserConfig(t)
			var stdout, stderr bytes.Buffer
			cmd := newConfigInitCommand(&stdout, &stderr)
			cmd.SetIn(pipeInput(t, "\n"))
			if err := maybeProvision(cmd, "", &stderr, func() bool { return resolves }); err != nil {
				t.Fatal(err)
			}
			prompted := strings.Contains(stderr.String(), "Configuration URL")
			if prompted == resolves || stdout.Len() != 0 {
				t.Fatalf("prompted=%v with roles resolving=%v: stdout=%q stderr=%q", prompted, resolves, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("a blank answer wrote a config: %v", err)
			}
		})
	}
}

func TestRolesResolveFromEnvironmentAndFlags(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("OPENAI_API_KEY", "key")
	if rolesResolve(noFlagsChanged, roleFlags{}, roleFlags{}) {
		t.Fatal("roles without models resolved")
	}
	changed := func(name string) bool { return name == "finder-model" || name == "verifier-model" }
	if !rolesResolve(changed, roleFlags{model: "f"}, roleFlags{model: "v"}) {
		t.Fatal("role flags were not considered")
	}
	t.Setenv("ROAST_FINDER_MODEL", "f")
	t.Setenv("ROAST_VERIFIER_MODEL", "v")
	if !rolesResolve(noFlagsChanged, roleFlags{}, roleFlags{}) {
		t.Fatal("environment-only roles did not resolve")
	}
}

func TestReviewWithDevNullStdinKeepsStdoutClean(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	t.Setenv("ROAST_NO_HOOK", "1")
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	repo := t.TempDir()
	runGit(t, repo, "init")
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()

	var stdout, stderr bytes.Buffer
	root := newRootCommand(&stdout, &stderr)
	root.SetIn(devNull)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"review", "--format", "json"})
	err = root.ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "model is not configured") {
		t.Fatalf("unconfigured review: %v", err)
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), "Configuration URL") {
		t.Fatalf("non-interactive review prompted: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestConfigInitPrintsTemplateSummaryToStderr(t *testing.T) {
	isolateUserConfig(t)
	destination := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"config", "init", "--from", serveTemplate(t, uploadTemplate), "--output", destination}, &stdout, &stderr)
	if err != nil || code != ExitClean {
		t.Fatalf("code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "installed configuration at "+destination) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	summary := stderr.String()
	for _, want := range []string{
		"finder:", "profile claude, https://models.example/v1, protocol anthropic, credential env TEMPLATE_MODEL_KEY",
		"author anthropic -> profile independent, https://independent.example/v1, protocol chat, credential none named; set ROAST_FINDER_API_KEY",
		"verifier:", "profile verifier, https://api.openai.com/v1 (default, or OPENAI_BASE_URL), protocol responses, credential none named; OPENAI_API_KEY",
		"upload:", "ON, after every review, to presigned broker https://uploads.example, token env TEMPLATE_UPLOAD_TOKEN",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

// TestProvisionSummaryEscapesConfigurationText covers configurations that
// never pass template validation, such as a local config shown during a
// protocol update: the summary and the upload question must still print no
// control characters.
func TestProvisionSummaryEscapesConfigurationText(t *testing.T) {
	const repaint = "\x1b[2K\rupload: off"
	s3 := config.Upload{
		Enabled: true, Backend: "s3", Endpoint: "https://uploads.example" + repaint,
		Bucket: "reviews\x1b[8m", Prefix: "runs\a", AccessKeyEnv: "ACCESS\x7f", SecretKeyEnv: "SECRET\u009b",
	}
	presigned := config.Upload{Enabled: true, Backend: "presigned", Endpoint: "https://broker.example" + repaint, TokenEnv: "TOKEN\x1b"}
	cfg := config.Config{
		InferenceProfiles: map[string]config.InferenceProfile{
			"finder" + repaint: {BaseURL: "https://models.example/v1\x1b]0;x\a", Model: "m", Protocol: "chat\n", APIKeyEnv: "KEY\x1b"},
			"route\u009b":      {Model: "m", Protocol: "responses"},
		},
		Roles: map[string]config.Role{
			"finder":   {Profile: "finder" + repaint, AuthorRoutes: map[string]string{"openai" + repaint: "route\u009b"}},
			"verifier": {Profile: "route\u009b"},
		},
		Upload: s3,
	}
	var out bytes.Buffer
	writeProvisionSummary(&out, &config.ProvisionPlan{Path: "config.yaml", Config: cfg, ProtocolUpdates: []string{"claude" + repaint}})
	for _, upload := range []config.Upload{s3, presigned} {
		cfg.Upload = upload
		writeProvisionSummary(&out, &config.ProvisionPlan{Path: "config.yaml", Config: cfg})
		out.WriteString(uploadQuestion(upload) + "\n")
	}
	for _, r := range out.String() {
		if (r < 0x20 && r != '\n') || (r >= 0x7f && r <= 0x9f) {
			t.Fatalf("summary contains control character %U:\n%q", r, out.String())
		}
	}
	for _, want := range []string{
		`roast: the template switches claude\x1b[2K\x0dupload: off to`,
		`author openai\x1b[2K\x0dupload: off -> profile route\u009b`,
		`s3 https://uploads.example\x1b[2K\x0dupload: off bucket reviews\x1b[8m prefix runs\x07`,
		`credentials env ACCESS\x7f, SECRET\u009b`,
		`presigned broker https://broker.example\x1b[2K\x0dupload: off, token env TOKEN\x1b`,
		`profile finder\x1b[2K\x0dupload: off, https://models.example/v1\x1b]0;x\x07, protocol chat\n, credential env KEY\x1b`,
		`to s3 https://uploads.example\x1b[2K\x0dupload: off bucket reviews\x1b[8m prefix runs\x07. Install it?`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary missing %s:\n%s", want, out.String())
		}
	}
}

func TestInteractiveInstallRequiresUploadConfirmation(t *testing.T) {
	forceInteractive(t)
	source := serveTemplate(t, uploadTemplate)
	for _, test := range []struct {
		name, answer string
		installed    bool
	}{
		{"default declines", "\n", false},
		{"explicit no", "n\n", false},
		{"end of input declines", "", false},
		{"explicit yes", "y\n", true},
	} {
		for _, entrypoint := range []string{"first run", "config init"} {
			t.Run(entrypoint+"/"+test.name, func(t *testing.T) {
				path := isolateUserConfig(t)
				var stdout, stderr bytes.Buffer
				cmd := newConfigInitCommand(&stdout, &stderr)
				cmd.SetContext(t.Context())
				var err error
				if entrypoint == "first run" {
					cmd.SetIn(pipeInput(t, source+"\n"+test.answer))
					err = maybeProvision(cmd, "", &stderr, func() bool { return false })
				} else {
					cmd.SetIn(pipeInput(t, test.answer))
					cmd.SetArgs([]string{"--from", source})
					err = cmd.Execute()
				}
				_, statErr := os.Stat(path)
				if test.installed != (err == nil) || test.installed != (statErr == nil) {
					t.Fatalf("err=%v installed=%v stderr=%s", err, statErr == nil, stderr.String())
				}
				if !strings.Contains(stderr.String(), "[y/N]") || !strings.Contains(stderr.String(), "presigned broker https://uploads.example") {
					t.Fatalf("missing upload confirmation: %s", stderr.String())
				}
				if entrypoint == "first run" && stdout.Len() != 0 {
					t.Fatalf("first-run install wrote to stdout: %q", stdout.String())
				}
			})
		}
	}
}

func TestNonInteractiveInstallDoesNotAskForUploadConfirmation(t *testing.T) {
	path := isolateUserConfig(t)
	var stdout, stderr bytes.Buffer
	cmd := newConfigInitCommand(&stdout, &stderr)
	cmd.SetContext(t.Context())
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--from", serveTemplate(t, uploadTemplate)})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil || strings.Contains(stderr.String(), "[y/N]") || !strings.Contains(stderr.String(), "upload:") {
		t.Fatalf("err=%v stderr=%s", err, stderr.String())
	}
}

func TestConfigInitExitCodes(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }))
	defer failing.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	for name, test := range map[string]struct {
		url      string
		code     int
		category ErrorCategory
	}{
		"network failure":  {closedURL, ExitPartial, ErrorEndpoint},
		"http failure":     {failing.URL, ExitPartial, ErrorEndpoint},
		"plaintext remote": {"http://config.example/roast", ExitUsage, ErrorUsage},
		"prompt override":  {serveTemplate(t, uploadTemplate+"prompts:\n  verifier: /etc/hosts\n"), ExitUsage, ErrorUsage},
	} {
		t.Run(name, func(t *testing.T) {
			isolateUserConfig(t)
			code, err := ExecuteContext(t.Context(), []string{"config", "init", "--from", test.url, "--output", filepath.Join(t.TempDir(), "config.yaml")}, io.Discard, io.Discard)
			if code != test.code || ErrorCategoryFor(code, err) != test.category {
				t.Fatalf("code=%d category=%s err=%v", code, ErrorCategoryFor(code, err), err)
			}
		})
	}
}

// Exercise the real CLI enrollment, both model roles, durable records, and
// automatic presigned upload without touching a user's installation/services.
func TestLegacyUpgradeThroughConfigurationURLAndPresignedUpload(t *testing.T) {
	for _, inPlace := range []bool{true, false} {
		t.Run(fmt.Sprintf("in-place=%v", inPlace), func(t *testing.T) {
			for _, name := range []string{
				"ROAST_FINDER_BASE_URL", "ROAST_VERIFIER_BASE_URL", "ROAST_FINDER_MODEL", "ROAST_VERIFIER_MODEL",
				"ROAST_FINDER_API_KEY", "ROAST_VERIFIER_API_KEY", "ROAST_FINDER_PROTOCOL", "ROAST_VERIFIER_PROTOCOL",
				"OPENAI_BASE_URL", "OPENAI_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ROAST_AUTHOR_MODEL",
			} {
				t.Setenv(name, "")
			}
			t.Setenv("ROAST_NO_HOOK", "1")
			t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			recordDir := t.TempDir()
			t.Setenv("ROAST_RECORD_DIR", recordDir)
			legacyPath, err := config.LegacyUserPath()
			if err != nil {
				t.Fatal(err)
			}
			legacy := []byte(`gateway:
  api_key: legacy-upgrade-key
roles:
  finder:
    model: old-finder
    api: chat
gate:
  auto_install: false
sourcebot:
  url: https://retired-search.example
  token: discarded-retrieval-key
upload:
  enabled: true
`)
			if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(legacyPath, legacy, 0o600); err != nil {
				t.Fatal(err)
			}
			destination := legacyPath
			if !inPlace {
				destination = filepath.Join(t.TempDir(), "roast", "config.yaml")
			}

			var modelCalls, grants, completions atomic.Int32
			models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer legacy-upgrade-key" || r.URL.Path != "/v1/chat/completions" {
					http.Error(w, "incorrect migrated inference request", http.StatusBadRequest)
					return
				}
				var request struct{ Model string }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				name, arguments := "findings", `{"assessment":"FINDINGS_PRESENT","findings":[{"reasoning":"unconditional panic","severity":"HIGH","confidence":0.95,"description":"The function always panics.","file":"bad.go","lines":[3],"code_quote":"panic(\"oops\")","fixed_code":"return"}]}`
				switch request.Model {
				case "finder-test":
				case "verifier-test":
					name = "submit_verdict"
					arguments = `{"reasoning":"unconditional panic","status":"confirmed","confidence":0.99,"grounding":"bad.go line 3 calls panic unconditionally","recalibrated_severity":""}`
				default:
					http.Error(w, "unexpected model", http.StatusBadRequest)
					return
				}
				modelCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = testutil.WriteJSONStream(w, "chat", fmt.Sprintf(`{"id":"upgrade","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, request.Model, name, arguments))
			}))
			defer models.Close()

			var uploadMu sync.Mutex
			var uploaded []byte
			storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.Header.Get("Authorization") != "" {
					http.Error(w, "broker bearer must not reach object storage", http.StatusBadRequest)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				uploadMu.Lock()
				uploaded = body
				uploadMu.Unlock()
				w.Header().Set("ETag", `"uploaded"`)
			}))
			defer storage.Close()
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer legacy-upgrade-key" || r.Method != http.MethodPost {
					http.Error(w, "incorrect migrated broker credential", http.StatusBadRequest)
					return
				}
				if r.URL.Path == "/api/v1/roast/runs" {
					var request struct {
						RunID  string `json:"run_id"`
						Digest string `json:"bundle_sha256"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						return
					}
					grants.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"run_id": request.RunID, "attempt_id": "attempt", "method": "PUT", "upload_url": storage.URL,
						"object_key": "runs/sha256/" + request.Digest + ".tar.zst",
					})
				} else if strings.HasSuffix(r.URL.Path, "/complete") {
					completions.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "uploaded"})
				} else {
					http.NotFound(w, r)
				}
			}))
			defer control.Close()
			configuration := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("configuration URL received a credential")
				}
				fmt.Fprintf(w, `version: 1
provisioning:
  import_legacy_credential: true
inference_profiles:
  finder:
    endpoint: %s/v1
    model: finder-test
    protocol: chat
  verifier:
    endpoint: %s/v1
    model: verifier-test
    protocol: chat
roles:
  finder:
    profile: finder
  verifier:
    profile: verifier
upload:
  enabled: true
  backend: presigned
  endpoint: %s
`, models.URL, models.URL, control.URL)
			}))
			defer configuration.Close()

			var output bytes.Buffer
			code, err := ExecuteContext(t.Context(), []string{"config", "init", "--from", configuration.URL, "--output", destination}, &output, io.Discard)
			if err != nil || code != ExitClean {
				t.Fatalf("configuration initialization: code=%d err=%v", code, err)
			}
			preserved := legacyPath
			if inPlace {
				preserved += ".pre-v1"
			}
			backup, err := os.ReadFile(preserved)
			if err != nil || !bytes.Equal(backup, legacy) {
				t.Fatalf("legacy config not preserved: %v", err)
			}
			current, err := os.ReadFile(destination)
			if err != nil || bytes.Contains(current, []byte("discarded-retrieval-key")) || bytes.Contains(current, []byte("auto_install")) {
				t.Fatalf("retired configuration survived provisioning: %v", err)
			}

			repo := initChangedRepo(t)
			previous, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(repo); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(previous) })
			output.Reset()
			code, err = ExecuteContext(t.Context(), []string{"review", "--config", destination, "--format", "json", "--max-severity", "HIGH", "--max-attempts", "1"}, &output, io.Discard)
			if err != nil || code != ExitFindings || modelCalls.Load() != 2 || grants.Load() != 1 || completions.Load() != 1 {
				t.Fatalf("upgrade review: code=%d err=%v model calls=%d grants=%d completions=%d output=%s", code, err, modelCalls.Load(), grants.Load(), completions.Load(), output.String())
			}
			var report struct {
				RunID string `json:"run_id"`
			}
			if err := json.Unmarshal(output.Bytes(), &report); err != nil || report.RunID == "" {
				t.Fatalf("review did not produce a durable run: %v", err)
			}
			runDir := filepath.Join(recordDir, "runs", report.RunID)
			if _, err := runbundle.Verify(filepath.Join(runDir, runbundle.BundleFile)); err != nil {
				t.Fatalf("invalid upgraded run bundle: %v", err)
			}
			bundle, err := os.ReadFile(filepath.Join(runDir, runbundle.BundleFile))
			if err != nil {
				t.Fatal(err)
			}
			uploadMu.Lock()
			matches := bytes.Equal(bundle, uploaded)
			uploadMu.Unlock()
			if !matches {
				t.Fatal("automatic upload did not send the durable run bundle")
			}
			statuses, err := runupload.ListStatus(recordDir)
			if err != nil || len(statuses) != 1 || statuses[0].Status != "uploaded" {
				t.Fatalf("upload status: %v err=%v", statuses, err)
			}
			digest := sha256.Sum256(bundle)
			if !strings.Contains(statuses[0].Detail, hex.EncodeToString(digest[:])) {
				t.Fatal("uploaded object is not content addressed")
			}
			meta, err := os.ReadFile(filepath.Join(runDir, record.MetaFile))
			if err != nil || bytes.Contains(meta, []byte("legacy-upgrade-key")) || bytes.Contains(meta, []byte("discarded-retrieval-key")) {
				t.Fatalf("credential leaked into run metadata: %v", err)
			}
		})
	}
}
