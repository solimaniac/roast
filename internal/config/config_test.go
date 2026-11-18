package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/testutil"
	"go.yaml.in/yaml/v3"
)

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUserPath(t *testing.T) {
	home := t.TempDir()
	appData := t.TempDir()
	xdg := filepath.Join(t.TempDir(), "config with spaces")
	native := filepath.Join(home, ".config")
	switch runtime.GOOS {
	case "darwin":
		native = filepath.Join(home, "Library", "Application Support")
	case "windows":
		native = appData
	}
	for _, tc := range []struct {
		name  string
		xdg   string
		unset bool
	}{
		{name: "unset", unset: true},
		{name: "empty"},
		{name: "absolute", xdg: xdg},
		{name: "relative", xdg: "relative-config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("APPDATA", appData)
			t.Setenv("XDG_CONFIG_HOME", tc.xdg)
			if tc.unset {
				if err := os.Unsetenv("XDG_CONFIG_HOME"); err != nil {
					t.Fatal(err)
				}
			}
			path, err := UserPath()
			if tc.name == "relative" && runtime.GOOS != "windows" {
				if err == nil || !strings.Contains(err.Error(), "XDG_CONFIG_HOME") {
					t.Fatalf("UserPath() = %q, %v; want an XDG_CONFIG_HOME error", path, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			root := native
			if tc.name == "absolute" && runtime.GOOS != "windows" {
				root = xdg
			}
			if want := filepath.Join(root, "roast", "config.yaml"); path != want {
				t.Fatalf("UserPath() = %q, want %q", path, want)
			}
		})
	}
}

func TestLoadExplicitPathOverridesConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "relative-config")
	path := writeConfig(t, `version: 1
inference_profiles:
  finder:
    model: explicit-finder
roles:
  finder:
    profile: finder
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.InferenceProfiles["finder"].Model; got != "explicit-finder" {
		t.Fatalf("finder model = %q, want explicit-finder", got)
	}
}

func TestLoadResolvesNamedProfilesAndRoleEnvironment(t *testing.T) {
	path := writeConfig(t, `version: 1
inference_profiles:
  primary:
    endpoint: https://finder.example/v1
    api_key_env: FINDER_PROFILE_KEY
    model: finder-file
    protocol: chat
  verify:
    model: verifier-file
roles:
  finder:
    profile: primary
  verifier:
    profile: verify
review:
  confidence_floor: 0.7
`)
	t.Setenv("FINDER_PROFILE_KEY", "profile-key")
	t.Setenv("ROAST_FINDER_MODEL", "finder-env")
	t.Setenv("OPENAI_API_KEY", "openai-key")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	finder, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if finder.Profile != "primary" || finder.Model != "finder-env" || finder.APIKey != "profile-key" || finder.BaseURL != "https://finder.example/v1" || finder.Protocol != "chat" {
		t.Fatalf("finder role = %#v", finder)
	}
	verifier, err := cfg.ResolveRole("verifier")
	if err != nil {
		t.Fatal(err)
	}
	if verifier.Model != "verifier-file" || verifier.APIKey != "openai-key" || verifier.Protocol != "responses" {
		t.Fatalf("verifier role = %#v", verifier)
	}
	if cfg.Review.ConfidenceFloor != 0.7 {
		t.Fatalf("confidence floor = %v", cfg.Review.ConfidenceFloor)
	}
	if len(cfg.InferenceProfiles) != 2 {
		t.Fatalf("profiles = %#v, want exactly the two configured profiles", cfg.InferenceProfiles)
	}
}

func TestResolveRoleUsesNativeAnthropicDefaults(t *testing.T) {
	path := writeConfig(t, `version: 1
inference_profiles:
  claude:
    model: claude-test
    protocol: anthropic
roles:
  finder:
    profile: claude
`)
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("OPENAI_API_KEY", "openai-key")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if role.BaseURL != "https://api.anthropic.com/v1" || role.APIKey != "anthropic-key" || role.Protocol != "anthropic" {
		t.Fatalf("finder role = %#v", role)
	}
	if err := ValidateRole("finder", role); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRoleIgnoresLegacyGlobalOverrides(t *testing.T) {
	path := writeConfig(t, `version: 1
inference_profiles:
  finder:
    model: finder-model
roles:
  finder:
    profile: finder
`)
	t.Setenv("ROAST_API_KEY", "wrong-key")
	t.Setenv("ROAST_BASE_URL", "https://wrong.example/v1")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("OPENAI_BASE_URL", "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if role.APIKey != "openai-key" || role.BaseURL != DefaultBaseURL("responses") {
		t.Fatalf("legacy globals changed role: %#v", role)
	}
}

func TestLoadRejectsProfileCredentialAmbiguity(t *testing.T) {
	path := writeConfig(t, `version: 1
inference_profiles:
  bad:
    model: model
    api_key: literal
    api_key_env: MODEL_KEY
roles:
  finder:
    profile: bad
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected ambiguous credential configuration to be rejected")
	}
}

func TestValidateRoleRejectsEmbeddedCredentials(t *testing.T) {
	err := ValidateRole("finder", ResolvedRole{
		Profile: "finder", BaseURL: "https://user:password@example.com/v1", APIKey: "key",
		Model: "model", Protocol: "chat", MaxOutputTokens: 100,
	})
	if err == nil {
		t.Fatal("expected embedded credentials to be rejected")
	}
}

func TestResolveFinderFlipSelectsNamedProfile(t *testing.T) {
	path := writeConfig(t, `version: 1
inference_profiles:
  primary:
    endpoint: https://openai.example/v1
    api_key_env: OPENAI_TEST_KEY
    model: gpt-author
    protocol: responses
    max_output_tokens: 1000
  independent:
    endpoint: https://anthropic.example/v1
    api_key_env: ANTHROPIC_TEST_KEY
    model: claude-reviewer
    protocol: anthropic
    max_output_tokens: 2000
roles:
  finder:
    profile: primary
    author_routes:
      openai: independent
`)
	t.Setenv("OPENAI_TEST_KEY", "openai-key")
	t.Setenv("ANTHROPIC_TEST_KEY", "anthropic-key")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	finder, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.ResolveFinderFlip(finder, "openai/gpt-author")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role.Profile != "independent" || got.Role.Model != "claude-reviewer" || got.Role.Protocol != "anthropic" || got.Role.APIKey != "anthropic-key" || got.Role.MaxOutputTokens != 2000 {
		t.Fatalf("resolved role = %#v", got.Role)
	}
	if got.Note == "" || got.Warn != "" {
		t.Fatalf("note=%q warn=%q", got.Note, got.Warn)
	}
}

func TestResolveFinderFlipRejectsSameFamilyTarget(t *testing.T) {
	cfg := Config{InferenceProfiles: map[string]InferenceProfile{
		"same": {Model: "o3-reviewer", Protocol: "responses", BaseURL: defaultOpenAIBaseURL, MaxOutputTokens: 100},
	}}
	finder := ResolvedRole{Model: "gpt-finder", AuthorRoutes: map[string]string{"openai": "same"}}
	got, err := cfg.ResolveFinderFlip(finder, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role.Model != finder.Model || got.Warn == "" {
		t.Fatalf("result = %#v", got)
	}
}

func TestResolveFinderFlipSelectsExactAuthorRoute(t *testing.T) {
	cfg := Config{InferenceProfiles: map[string]InferenceProfile{
		"bar": {Model: "claude-reviewer", Protocol: "anthropic", BaseURL: defaultAnthropicBaseURL, MaxOutputTokens: 100},
	}}
	finder := ResolvedRole{
		Model: "gpt-default", AuthorRoutes: map[string]string{"foo-model": "bar"},
	}
	got, err := cfg.ResolveFinderFlip(finder, "FOO-MODEL")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role.Profile != "bar" || got.Role.Model != "claude-reviewer" || got.Role.Protocol != "anthropic" {
		t.Fatalf("result = %#v", got)
	}
}

func TestInferFamily(t *testing.T) {
	for input, want := range map[string]string{
		"anthropic/claude-sonnet": "anthropic",
		"gpt-5":                   "openai",
		"gemini-2.5-pro":          "google",
		"mistral-large":           "mistral",
		"grok-4":                  "xai",
	} {
		if got := InferFamily(input); got != want {
			t.Errorf("InferFamily(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLoadAndResolveS3CompatibleUpload(t *testing.T) {
	path := writeConfig(t, `version: 1
upload:
  enabled: true
  backend: s3
  endpoint: https://objects.example.test/
  bucket: roast-data
  prefix: team/runs/
  region: auto
  access_key_env: TEST_UPLOAD_ACCESS
  secret_key_env: TEST_UPLOAD_SECRET
  session_token_env: TEST_UPLOAD_SESSION
  path_style: true
`)
	t.Setenv("TEST_UPLOAD_ACCESS", "access")
	t.Setenv("TEST_UPLOAD_SECRET", "secret")
	t.Setenv("TEST_UPLOAD_SESSION", "session")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := cfg.ResolveUpload()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Endpoint != "https://objects.example.test" || resolved.Bucket != "roast-data" || resolved.Prefix != "team/runs" || resolved.Region != "auto" || !resolved.PathStyle {
		t.Fatalf("resolved upload = %#v", resolved)
	}
	if resolved.AccessKey != "access" || resolved.SecretKey != "secret" || resolved.SessionToken != "session" {
		t.Fatal("resolved upload credentials do not match their explicit environment sources")
	}
}

func TestLoadAndResolvePresignedUpload(t *testing.T) {
	path := writeConfig(t, `version: 1
upload:
  enabled: true
  backend: presigned
  endpoint: https://uploads.example.test/
  token_env: TEST_BROKER_TOKEN
`)
	t.Setenv("TEST_BROKER_TOKEN", "broker-token")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := cfg.ResolveUpload()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Backend != "presigned" || resolved.Endpoint != "https://uploads.example.test" || resolved.Token != "broker-token" {
		t.Fatalf("resolved upload = %#v", resolved)
	}
}

func TestUploadNeverUsesProviderCredentialChain(t *testing.T) {
	path := writeConfig(t, `version: 1
upload:
  endpoint: https://objects.example.test
  bucket: roast-data
`)
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("ROAST_UPLOAD_ACCESS_KEY", "")
	t.Setenv("ROAST_UPLOAD_SECRET_KEY", "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ResolveUpload(); err == nil {
		t.Fatal("expected explicit Roast upload credentials; AWS environment credentials must not be consulted")
	}
}

func TestLoadMigratesUnversionedNamedProfiles(t *testing.T) {
	path := writeConfig(t, `inference_profiles:
  old:
    base_url: https://draft.example/v1
    model: draft-model
    protocol: chat
roles:
  finder:
    profile: old
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != CurrentVersion || cfg.InferenceProfiles["old"].BaseURL != "https://draft.example/v1" {
		t.Fatalf("migrated config = %#v", cfg)
	}
}

func TestLoadMigratesLegacyInlineRoles(t *testing.T) {
	path := writeConfig(t, `gateway:
  base_url: https://legacy-gateway.example/v1
  api_key: shared-legacy-key
upload:
  enabled: true
gate:
  auto_install: false
sourcebot:
  url: https://retired-search.example
  token: discarded-retrieval-key
roles:
  finder:
    model: gpt-finder
    api: chat
    max_output_tokens: 1234
    author_flip:
      openai:
        model: claude-reviewer
        api: anthropic
  verifier:
    model: verifier-model
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	finder, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if finder.Profile != "finder" || finder.BaseURL != "https://legacy-gateway.example/v1" || finder.APIKey != "shared-legacy-key" || finder.Protocol != "chat" || finder.MaxOutputTokens != 1234 {
		t.Fatalf("migrated finder = %#v", finder)
	}
	routeName := finder.AuthorRoutes["openai"]
	route := cfg.InferenceProfiles[routeName]
	if route.Model != "claude-reviewer" || route.Protocol != "anthropic" || route.BaseURL != "https://legacy-gateway.example/v1" || route.APIKey != "shared-legacy-key" || route.MaxOutputTokens != 1234 {
		t.Fatalf("migrated author route %q = %#v", routeName, route)
	}
	if cfg.Upload.Enabled {
		t.Fatal("legacy upload enrollment must not enable a new upload destination")
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"auto_install", "sourcebot", "retired-search.example", "discarded-retrieval-key"} {
		if strings.Contains(string(raw), retired) {
			t.Errorf("retired setting %q survived migration", retired)
		}
	}
}

func TestLoadRecognizesLegacyConfigWithoutExplicitRoles(t *testing.T) {
	path := writeConfig(t, `gateway:
  api_key: unused-legacy-key
gate:
  auto_install: false
sourcebot:
  url: https://retired-search.example
  token: discarded-retrieval-key
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != CurrentVersion || cfg.Upload.Enabled {
		t.Fatalf("unexpected migrated config: %#v", cfg)
	}
}

func TestLoadLegacyConfigStillRejectsUnknownFields(t *testing.T) {
	for _, suffix := range []string{"unknown: true\n", "gate:\n  auto_instal: false\n", "sourcebot:\n  tokne: invalid\n"} {
		if _, err := Load(writeConfig(t, "gateway:\n  api_key: legacy-key\n"+suffix)); err == nil {
			t.Fatalf("unknown legacy setting was accepted: %q", suffix)
		}
	}
}

func TestLoadEnforcesVersionedSchema(t *testing.T) {
	for name, body := range map[string]string{
		"future version":       "version: 2\n",
		"zero version":         "version: 0\n",
		"old field in v1":      "version: 1\ninference_profiles:\n  old:\n    base_url: https://example.test/v1\n",
		"retired gate in v1":   "version: 1\ngate:\n  auto_install: false\n",
		"retired search in v1": "version: 1\nsourcebot:\n  url: https://retired-search.example\n",
		"multiple documents":   "version: 1\n---\nversion: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("expected config to be rejected")
			}
		})
	}
}

func TestLoadRejectsInvalidUploadConfiguration(t *testing.T) {
	for name, body := range map[string]string{
		"enabled without destination": "upload:\n  enabled: true\n",
		"plaintext remote endpoint":   "upload:\n  endpoint: http://objects.example.test\n  bucket: b\n",
		"endpoint path":               "upload:\n  endpoint: https://objects.example.test/base\n  bucket: b\n",
		"credential in endpoint":      "upload:\n  endpoint: https://user:secret@objects.example.test\n  bucket: b\n",
		"escaping prefix":             "upload:\n  endpoint: https://objects.example.test\n  bucket: b\n  prefix: ../runs\n",
		"bad env name":                "upload:\n  endpoint: https://objects.example.test\n  bucket: b\n  access_key_env: not-valid!\n",
		"presigned with bucket":       "upload:\n  backend: presigned\n  endpoint: https://uploads.example.test\n  bucket: b\n",
		"s3 with token":               "upload:\n  backend: s3\n  endpoint: https://objects.example.test\n  bucket: b\n  token_env: TOKEN\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("expected invalid upload configuration to be rejected")
			}
		})
	}
}

func TestLoadAllowsExplicitLocalPlaintextUpload(t *testing.T) {
	for name, body := range map[string]string{
		"loopback":               "upload:\n  endpoint: http://127.0.0.1:9000\n  bucket: b\n",
		"trusted network opt in": "upload:\n  endpoint: http://minio:9000\n  bucket: b\n  allow_insecure_http: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err != nil {
				t.Fatalf("expected local plaintext endpoint to be accepted: %v", err)
			}
		})
	}
}

func TestLoadSessionHeader(t *testing.T) {
	for _, header := range []string{"x-opencode-session", "X-Gateway-Session", "!#$%&'*+-.^_`|~09azAZ", "X-" + strings.Repeat("s", 62)} {
		t.Run(header, func(t *testing.T) {
			path := writeConfig(t, "version: 1\ninference_profiles:\n  gateway:\n    endpoint: https://gateway.example/v1\n    model: gateway-model\n    protocol: chat\n    session_header: "+strconv.Quote(header)+
				"\n  plain:\n    model: plain-model\nroles:\n  finder:\n    profile: gateway\n  verifier:\n    profile: plain\n")
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			finder, err := cfg.ResolveRole("finder")
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := cfg.ResolveRole("verifier")
			if err != nil {
				t.Fatal(err)
			}
			if finder.SessionHeader != header || verifier.SessionHeader != "" {
				t.Fatalf("session headers = %q, %q; want %q, none", finder.SessionHeader, verifier.SessionHeader, header)
			}
		})
	}
}

func TestLoadRejectsInvalidSessionHeader(t *testing.T) {
	cases := map[string]string{}
	for _, name := range []string{
		"Authorization", "X-Api-Key", "Proxy-Authorization", "Cookie", "Host",
		"Content-Type", "Content-Length", "Transfer-Encoding", "Connection", "User-Agent",
		"Anthropic-Version", "Anthropic-Beta", "OpenAI-Organization", "OpenAI-Project",
		"X-Agent-Session-Id",
		// Message framing, connection control, and other SDK or transport headers.
		"Content-Encoding", "Keep-Alive", "Proxy-Connection", "TE", "Trailer", "Upgrade", "Expect",
		"Accept", "Accept-Encoding", "Anthropic-Workspace-Id", "X-Stainless-Retry-Count", "X-Stainless-Future-Header",
	} {
		for _, spelling := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
			cases[spelling] = "is reserved"
		}
	}
	for _, name := range []string{
		"x opencode session", "x-opencode-session:", " x-opencode-session", "x-session\t", "x-sessión",
		"x/session", "(session)", `x"session`, "x-session\r\nX-Injected: 1", "X-" + strings.Repeat("s", 63),
	} {
		cases[name] = "must be an HTTP header name"
	}
	for header, want := range cases {
		t.Run(header, func(t *testing.T) {
			_, err := Load(writeConfig(t, "version: 1\ninference_profiles:\n  gateway:\n    model: m\n    session_header: "+strconv.Quote(header)+"\n"))
			if err == nil || !strings.Contains(err.Error(), "inference_profiles.gateway.session_header") || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want a session_header error containing %q", err, want)
			}
		})
	}
}

func TestChatGPTProfile(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	t.Setenv("OPENAI_BASE_URL", "https://ambient.invalid/v1")
	cfg := Defaults()
	cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "chatgpt", Model: "gpt-test", MaxOutputTokens: 1024}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRole("finder", role); err != nil {
		t.Fatal(err)
	}
	if role.APIKey != "" || role.BaseURL != "https://api.openai.com/v1" || role.Credential.Kind != CredentialChatGPTLogin {
		t.Fatalf("unexpected role: %#v", role)
	}
	if !strings.Contains(role.Credential.String(), "not verified") {
		t.Fatal(role.Credential.String())
	}
	profile := cfg.InferenceProfiles["finder"]
	if strings.Contains(profile.DescribeEndpoint(), "OPENAI") || strings.Contains(profile.DescribeCredential("finder"), "API_KEY") {
		t.Fatal("ChatGPT display offers API credentials")
	}
	role.Model = ""
	if ValidateRole("finder", role) == nil {
		t.Fatal("missing model accepted")
	}
}

func TestChatGPTRejectsProfileHTTPFields(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile InferenceProfile
	}{
		{"custom endpoint", InferenceProfile{BaseURL: "https://example.com"}},
		{"explicit default endpoint", InferenceProfile{BaseURL: "https://api.openai.com/v1"}},
		{"literal key", InferenceProfile{APIKey: "secret"}},
		{"key environment variable", InferenceProfile{APIKeyEnv: "MISSING_KEY"}},
		{"session header", InferenceProfile{SessionHeader: "X-Session"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := tc.profile
			profile.Protocol, profile.Model = "chatgpt", "gpt-test"
			_, err := normalizeProfile("test", profile)
			if err == nil || !strings.Contains(err.Error(), "requires no endpoint") {
				t.Fatalf("normalizeProfile error = %v, want managed-login field rejection", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("validation disclosed API key")
			}
		})
	}
}

func TestChatGPTRejectsRoleOverrides(t *testing.T) {
	for _, variable := range []string{"ROAST_FINDER_API_KEY", "ROAST_FINDER_BASE_URL"} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv(variable, "private-value")
			cfg := Defaults()
			cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "chatgpt", Model: "gpt-test", MaxOutputTokens: 1024}
			if _, err := cfg.ResolveRole("finder"); err == nil || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("unsafe validation: %v", err)
			}
		})
	}
}

func TestChatGPTCLIOverrides(t *testing.T) {
	chatgpt := "chatgpt"
	for _, kind := range []string{CredentialProviderEnv, CredentialProfileEnv, CredentialProfileKey, CredentialRoleEnv} {
		t.Run(kind, func(t *testing.T) {
			role := ResolvedRole{Protocol: "responses", BaseURL: DefaultBaseURL("responses"), Model: "gpt-test", APIKey: "secret", MaxOutputTokens: 1024, Credential: CredentialSource{Kind: kind}}
			role = OverrideEndpoint("finder", role, nil, &chatgpt)
			err := ValidateRole("finder", role)
			if kind == CredentialProviderEnv {
				if err != nil || role.APIKey != "" || role.Credential.Kind != CredentialChatGPTLogin {
					t.Fatalf("ambient override: %#v %v", role, err)
				}
			} else if err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
	role := ResolvedRole{Protocol: "chatgpt", BaseURL: "https://api.openai.com/v1", Model: "gpt-test", MaxOutputTokens: 1024, Credential: CredentialSource{Kind: CredentialChatGPTLogin}}
	for _, endpoint := range []string{"https://example.com", "https://api.openai.com/v1"} {
		got := OverrideEndpoint("finder", role, &endpoint, nil)
		if ValidateRole("finder", got) == nil {
			t.Fatal("explicit endpoint accepted")
		}
	}
	t.Setenv("ROAST_FINDER_API_KEY", "secret")
	role = OverrideEndpoint("finder", role, nil, nil)
	if ValidateRole("finder", role) == nil {
		t.Fatal("role API key accepted")
	}
}

func TestChatGPTProtocolEnvironmentOverride(t *testing.T) {
	t.Setenv("ROAST_FINDER_PROTOCOL", "chatgpt")
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	t.Setenv("OPENAI_BASE_URL", "https://ambient.invalid/v1")
	cfg := Defaults()
	cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "responses", Model: "gpt-test", MaxOutputTokens: 1024}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRole("finder", role); err != nil {
		t.Fatal(err)
	}
	if role.APIKey != "" || role.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("ambient settings followed protocol override: %#v", role)
	}
	for _, profile := range []InferenceProfile{
		{Protocol: "responses", APIKey: "secret"},
		{Protocol: "responses", APIKeyEnv: "MISSING_KEY"},
		{Protocol: "responses", BaseURL: DefaultBaseURL("responses")},
		{Protocol: "responses", SessionHeader: "X-Session"},
	} {
		cfg.InferenceProfiles["finder"] = profile
		if _, err := cfg.ResolveRole("finder"); err == nil {
			t.Fatalf("profile HTTP settings followed chatgpt override: %#v", profile)
		}
	}
}

func TestCodexCLIProtocolRejected(t *testing.T) {
	_, err := normalizeProfile("finder", InferenceProfile{Protocol: "codex", Model: "gpt-test"})
	if err == nil {
		t.Fatal("Codex CLI protocol accepted; no implicit inference fallback is allowed")
	}
}

func TestSwitchFromChatGPTUsesTheAPIKeyProvidersEndpoint(t *testing.T) {
	const gateway = "https://gateway.example/v1"
	t.Setenv("OPENAI_BASE_URL", gateway)
	t.Setenv("OPENAI_API_KEY", "gateway-key")
	cfg := Defaults()
	cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "chatgpt", Model: "test", MaxOutputTokens: 128}
	for _, source := range []string{"flag", "environment"} {
		t.Run(source, func(t *testing.T) {
			protocol := "responses"
			if source == "environment" {
				t.Setenv("ROAST_FINDER_PROTOCOL", protocol)
			}
			role, err := cfg.ResolveRole("finder")
			if err != nil {
				t.Fatal(err)
			}
			if source == "flag" {
				role = OverrideEndpoint("finder", role, nil, &protocol)
			}
			if err := ValidateRole("finder", role); err != nil {
				t.Fatal(err)
			}
			if role.BaseURL != gateway || role.APIKey != "gateway-key" || role.Credential.Kind != CredentialProviderEnv {
				t.Fatalf("switching from ChatGPT should use the endpoint paired with OPENAI_API_KEY, got %s", role.BaseURL)
			}
		})
	}
}

const (
	ambientGateway = "https://ambient-gateway.example/v1"
	customEndpoint = "https://template-endpoint.example/v1"
)

// TestCredentialAndEndpointPrecedence enumerates every combination of
// profile endpoint, profile credential, ambient provider base URL, role-scoped
// key, and protocol, and checks each against the documented rule.
func TestCredentialAndEndpointPrecedence(t *testing.T) {
	type expectation struct {
		endpoint string
		key      string
		kind     string
	}
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		keyVar, baseVar := providerAPIKeyEnv(protocol), providerBaseURLEnv(protocol)
		for _, endpoint := range []string{"unset", "default", "custom", "ambient"} {
			for _, credential := range []string{"none", "api_key_env", "api_key"} {
				for _, ambientBase := range []bool{false, true} {
					for _, roleKey := range []bool{false, true} {
						if endpoint == "ambient" && !ambientBase {
							continue
						}
						name := fmt.Sprintf("%s/endpoint=%s/credential=%s/%s=%v/role_key=%v", protocol, endpoint, credential, baseVar, ambientBase, roleKey)
						t.Run(name, func(t *testing.T) {
							clearProviderEnv(t)
							t.Setenv(keyVar, "ambient-key")
							t.Setenv("PROFILE_KEY", "profile-key")
							if ambientBase {
								t.Setenv(baseVar, ambientGateway)
							}
							if roleKey {
								t.Setenv("ROAST_FINDER_API_KEY", "role-key")
							}
							profile := fmt.Sprintf("    model: m\n    protocol: %s\n", protocol)
							switch endpoint {
							case "default":
								profile += "    endpoint: " + DefaultBaseURL(protocol) + "\n"
							case "custom":
								profile += "    endpoint: " + customEndpoint + "\n"
							case "ambient":
								profile += "    endpoint: " + ambientGateway + "\n"
							}
							switch credential {
							case "api_key_env":
								profile += "    api_key_env: PROFILE_KEY\n"
							case "api_key":
								profile += "    api_key: literal-key\n"
							}
							role := resolveFinderProfile(t, profile)

							var want expectation
							switch endpoint {
							case "unset":
								want.endpoint = DefaultBaseURL(protocol)
								if ambientBase {
									want.endpoint = ambientGateway
								}
							case "default":
								want.endpoint = DefaultBaseURL(protocol)
							case "custom":
								want.endpoint = customEndpoint
							case "ambient":
								want.endpoint = ambientGateway
							}
							switch {
							case roleKey:
								want.key, want.kind = "role-key", CredentialRoleEnv
							case credential == "api_key_env":
								want.key, want.kind = "profile-key", CredentialProfileEnv
							case credential == "api_key":
								want.key, want.kind = "literal-key", CredentialProfileKey
							case endpoint != "custom":
								// Default endpoint, or the base URL the same
								// environment pairs with the key.
								want.key, want.kind = "ambient-key", CredentialProviderEnv
							default:
								want.kind = CredentialNone
							}
							if role.BaseURL != want.endpoint || role.APIKey != want.key || role.Credential.Kind != want.kind {
								t.Fatalf("got endpoint=%q key=%q kind=%q, want endpoint=%q key=%q kind=%q",
									role.BaseURL, role.APIKey, role.Credential.Kind, want.endpoint, want.key, want.kind)
							}
							if want.kind == CredentialNone && role.Credential.Withheld != keyVar {
								t.Fatalf("withheld = %q, want %q", role.Credential.Withheld, keyVar)
							}
						})
					}
				}
			}
		}
	}
}

func TestTemplateProfileWithoutCredentialNeverReceivesAmbientKey(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "personal-anthropic-key")
	t.Setenv("OPENAI_API_KEY", "personal-openai-key")
	t.Setenv("ANTHROPIC_BASE_URL", ambientGateway)
	for _, protocol := range []string{"anthropic", "responses"} {
		role := resolveFinderProfile(t, fmt.Sprintf("    endpoint: %s\n    model: m\n    protocol: %s\n", customEndpoint, protocol))
		if role.BaseURL != customEndpoint || role.APIKey != "" {
			t.Fatalf("%s: endpoint=%q key=%q", protocol, role.BaseURL, role.APIKey)
		}
		err := ValidateRole("finder", role)
		if err == nil || !strings.Contains(err.Error(), "is not sent to "+customEndpoint) || !strings.Contains(err.Error(), "ROAST_FINDER_API_KEY") {
			t.Fatalf("%s: unexpected validation error %v", protocol, err)
		}
	}
}

func TestRoleEnvironmentOverrides(t *testing.T) {
	for name, test := range map[string]struct {
		env          map[string]string
		profile      string
		wantEndpoint string
		wantProtocol string
		wantKey      string
	}{
		"role base URL keeps profile credential": {
			env:          map[string]string{"ROAST_FINDER_BASE_URL": "https://role.example/v1", "PROFILE_KEY": "profile-key", "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n    endpoint: " + customEndpoint + "\n    api_key_env: PROFILE_KEY\n",
			wantEndpoint: "https://role.example/v1", wantProtocol: "responses", wantKey: "profile-key",
		},
		"role base URL never receives ambient key": {
			env:          map[string]string{"ROAST_FINDER_BASE_URL": "https://role.example/v1", "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n",
			wantEndpoint: "https://role.example/v1", wantProtocol: "responses", wantKey: "",
		},
		"role base URL with role key": {
			env:          map[string]string{"ROAST_FINDER_BASE_URL": "https://role.example/v1", "ROAST_FINDER_API_KEY": "role-key", "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n",
			wantEndpoint: "https://role.example/v1", wantProtocol: "responses", wantKey: "role-key",
		},
		"role base URL wins over ambient base URL": {
			env:          map[string]string{"ROAST_FINDER_BASE_URL": "https://role.example/v1", "OPENAI_BASE_URL": ambientGateway, "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n",
			wantEndpoint: "https://role.example/v1", wantProtocol: "responses", wantKey: "",
		},
		"protocol override crosses providers from the default endpoint": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "anthropic", "ANTHROPIC_API_KEY": "anthropic", "OPENAI_API_KEY": "openai"},
			profile:      "    model: m\n",
			wantEndpoint: DefaultBaseURL("anthropic"), wantProtocol: "anthropic", wantKey: "anthropic",
		},
		"profile credential does not follow a provider crossing": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "anthropic", "PROFILE_KEY": "openai-profile", "ANTHROPIC_API_KEY": "anthropic"},
			profile:      "    model: m\n    api_key_env: PROFILE_KEY\n",
			wantEndpoint: DefaultBaseURL("anthropic"), wantProtocol: "anthropic", wantKey: "anthropic",
		},
		"provider crossing then ambient anthropic base URL": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "anthropic", "ANTHROPIC_BASE_URL": ambientGateway, "ANTHROPIC_API_KEY": "anthropic"},
			profile:      "    model: m\n",
			wantEndpoint: ambientGateway, wantProtocol: "anthropic", wantKey: "anthropic",
		},
		"protocol override on a custom endpoint keeps the profile credential": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "anthropic", "PROFILE_KEY": "gateway", "ANTHROPIC_API_KEY": "anthropic"},
			profile:      "    model: m\n    endpoint: " + customEndpoint + "\n    api_key_env: PROFILE_KEY\n",
			wantEndpoint: customEndpoint, wantProtocol: "anthropic", wantKey: "gateway",
		},
		"protocol override on a custom endpoint never sends an ambient key": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "anthropic", "ANTHROPIC_API_KEY": "anthropic"},
			profile:      "    model: m\n    endpoint: " + customEndpoint + "\n",
			wantEndpoint: customEndpoint, wantProtocol: "anthropic", wantKey: "",
		},
		"protocol override within one provider keeps the profile credential": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "chat", "PROFILE_KEY": "profile", "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n    api_key_env: PROFILE_KEY\n",
			wantEndpoint: DefaultBaseURL("chat"), wantProtocol: "chat", wantKey: "profile",
		},
		"configured endpoint ignores ambient base URL": {
			env:          map[string]string{"ANTHROPIC_BASE_URL": ambientGateway, "PROFILE_KEY": "profile"},
			profile:      "    model: m\n    protocol: anthropic\n    endpoint: " + customEndpoint + "\n    api_key_env: PROFILE_KEY\n",
			wantEndpoint: customEndpoint, wantProtocol: "anthropic", wantKey: "profile",
		},
		"ambient base URL is protocol scoped": {
			env:          map[string]string{"OPENAI_BASE_URL": ambientGateway, "ANTHROPIC_API_KEY": "anthropic"},
			profile:      "    model: m\n    protocol: anthropic\n",
			wantEndpoint: DefaultBaseURL("anthropic"), wantProtocol: "anthropic", wantKey: "anthropic",
		},
		"endpoint comparison ignores a trailing slash": {
			env:          map[string]string{"OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n    endpoint: " + DefaultBaseURL("responses") + "/\n",
			wantEndpoint: DefaultBaseURL("responses") + "/", wantProtocol: "responses", wantKey: "ambient",
		},
		"role model and output limit": {
			env:          map[string]string{"ROAST_FINDER_MODEL": "env-model", "ROAST_FINDER_MAX_OUTPUT_TOKENS": "77", "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n",
			wantEndpoint: DefaultBaseURL("responses"), wantProtocol: "responses", wantKey: "ambient",
		},
	} {
		t.Run(name, func(t *testing.T) {
			clearProviderEnv(t)
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			role := resolveFinderProfile(t, test.profile)
			if role.BaseURL != test.wantEndpoint || role.Protocol != test.wantProtocol || role.APIKey != test.wantKey {
				t.Fatalf("got endpoint=%q protocol=%q key=%q source=%v", role.BaseURL, role.Protocol, role.APIKey, role.Credential)
			}
			if name == "role model and output limit" && (role.Model != "env-model" || role.MaxOutputTokens != 77) {
				t.Fatalf("role overrides not applied: %#v", role)
			}
		})
	}
}

func TestInvalidRoleMaxOutputTokensIsRejected(t *testing.T) {
	cfg, err := Load(writeConfig(t, `version: 1
inference_profiles:
  primary:
    model: gpt-primary
  independent:
    model: claude-reviewer
    protocol: anthropic
  verifier:
    model: v
roles:
  finder:
    profile: primary
    author_routes:
      openai: independent
  verifier:
    profile: verifier
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"lots", "0", "-5", "1.5", "1e3", " 7"} {
		t.Run(value, func(t *testing.T) {
			clearProviderEnv(t)
			t.Setenv("ROAST_FINDER_MAX_OUTPUT_TOKENS", value)
			_, err := cfg.ResolveRole("finder")
			if err == nil || !strings.Contains(err.Error(), "ROAST_FINDER_MAX_OUTPUT_TOKENS must be an integer >= 1") || !strings.Contains(err.Error(), fmt.Sprintf("%q", value)) {
				t.Fatalf("ResolveRole error = %v", err)
			}
			if _, err := cfg.ResolveFinderFlip(ResolvedRole{Model: "gpt-finder", AuthorRoutes: map[string]string{"openai": "independent"}}, "gpt-author"); err == nil {
				t.Fatal("author route resolution ignored the invalid override")
			}
			if _, err := cfg.ResolveRole("verifier"); err != nil {
				t.Fatalf("finder override affected the verifier: %v", err)
			}
		})
	}
	clearProviderEnv(t)
	t.Setenv("ROAST_FINDER_MAX_OUTPUT_TOKENS", "4096")
	role, err := cfg.ResolveRole("finder")
	if err != nil || role.MaxOutputTokens != 4096 {
		t.Fatalf("valid override: tokens=%d err=%v", role.MaxOutputTokens, err)
	}
}

func TestEnvironmentOnlyDefaultsHonorProviderBaseURL(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPENAI_BASE_URL", ambientGateway)
	t.Setenv("OPENAI_API_KEY", "ambient")
	t.Setenv("ROAST_FINDER_MODEL", "m")
	cfg, err := Load(writeConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if role.BaseURL != ambientGateway || role.APIKey != "ambient" || role.Credential.Kind != CredentialProviderEnv {
		t.Fatalf("environment-only role = %#v", role)
	}
	if cfg.InferenceProfiles["finder"].BaseURL != "" {
		t.Fatal("normalization must not turn an unset endpoint into a configured one")
	}
}

func TestAuthorRouteProfileUsesTheSameCredentialRule(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", ambientGateway)
	t.Setenv("ANTHROPIC_API_KEY", "ambient")
	cfg, err := Load(writeConfig(t, `version: 1
inference_profiles:
  primary:
    model: gpt-primary
  independent:
    endpoint: `+customEndpoint+`
    model: claude-reviewer
    protocol: anthropic
  verifier:
    model: v
roles:
  finder:
    profile: primary
    author_routes:
      openai: independent
  verifier:
    profile: verifier
`))
	if err != nil {
		t.Fatal(err)
	}
	finder, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	flip, err := cfg.ResolveFinderFlip(finder, "gpt-author")
	if err != nil {
		t.Fatal(err)
	}
	if flip.Role.Profile != "independent" || flip.Role.BaseURL != customEndpoint || flip.Role.APIKey != "" {
		t.Fatalf("author route = %#v", flip.Role)
	}
}

// TestEndpointOverridesMatchAcrossEnvironmentAndFlags checks that a command-line
// protocol or endpoint override lands on the same endpoint with the same
// credential as the equivalent ROAST_<ROLE>_* variable.
func TestEndpointOverridesMatchAcrossEnvironmentAndFlags(t *testing.T) {
	const gateway = "https://gateway.example/v1"
	for name, test := range map[string]struct {
		profile      string
		env          map[string]string
		protocol     string
		baseURL      string
		wantEndpoint string
		wantKey      string
	}{
		"unset endpoint follows the new provider's base URL": {
			profile:      "    model: m\n    api_key_env: MY_OPENAI\n",
			env:          map[string]string{"MY_OPENAI": "openai-profile", "ANTHROPIC_BASE_URL": gateway, "ANTHROPIC_API_KEY": "gateway-key"},
			protocol:     "anthropic",
			wantEndpoint: gateway, wantKey: "gateway-key",
		},
		"unset endpoint without a base URL goes to the provider default": {
			profile:      "    model: m\n    api_key_env: MY_OPENAI\n",
			env:          map[string]string{"MY_OPENAI": "openai-profile", "ANTHROPIC_API_KEY": "anthropic-key"},
			protocol:     "anthropic",
			wantEndpoint: DefaultBaseURL("anthropic"), wantKey: "anthropic-key",
		},
		"unset endpoint leaves the previous provider's base URL": {
			profile:      "    model: m\n",
			env:          map[string]string{"OPENAI_BASE_URL": gateway, "OPENAI_API_KEY": "openai-key", "ANTHROPIC_API_KEY": "anthropic-key"},
			protocol:     "anthropic",
			wantEndpoint: DefaultBaseURL("anthropic"), wantKey: "anthropic-key",
		},
		"explicit default endpoint with a trailing slash moves": {
			profile:      "    model: m\n    endpoint: " + DefaultBaseURL("responses") + "/\n    api_key_env: MY_OPENAI\n",
			env:          map[string]string{"MY_OPENAI": "openai-profile", "ANTHROPIC_BASE_URL": gateway, "ANTHROPIC_API_KEY": "anthropic-key"},
			protocol:     "anthropic",
			wantEndpoint: DefaultBaseURL("anthropic"), wantKey: "anthropic-key",
		},
		"explicit custom endpoint stays with its credential": {
			profile:      "    model: m\n    endpoint: " + customEndpoint + "\n    api_key_env: MY_GATEWAY\n",
			env:          map[string]string{"MY_GATEWAY": "gateway-profile", "ANTHROPIC_BASE_URL": gateway, "ANTHROPIC_API_KEY": "anthropic-key"},
			protocol:     "anthropic",
			wantEndpoint: customEndpoint, wantKey: "gateway-profile",
		},
		"same provider protocol keeps the profile credential": {
			profile:      "    model: m\n    api_key_env: MY_OPENAI\n",
			env:          map[string]string{"MY_OPENAI": "openai-profile", "OPENAI_BASE_URL": gateway, "OPENAI_API_KEY": "openai-key"},
			protocol:     "chat",
			wantEndpoint: gateway, wantKey: "openai-profile",
		},
		"endpoint override with a protocol override keeps the profile credential": {
			profile:  "    model: m\n    api_key_env: MY_OPENAI\n",
			env:      map[string]string{"MY_OPENAI": "openai-profile", "ANTHROPIC_API_KEY": "anthropic-key"},
			protocol: "anthropic", baseURL: "https://chosen.example/v1",
			wantEndpoint: "https://chosen.example/v1", wantKey: "openai-profile",
		},
		"endpoint override never carries an ambient key": {
			profile:      "    model: m\n",
			env:          map[string]string{"OPENAI_API_KEY": "openai-key"},
			baseURL:      "https://chosen.example/v1",
			wantEndpoint: "https://chosen.example/v1", wantKey: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			resolve := func(path string) ResolvedRole {
				clearProviderEnv(t)
				for key, value := range test.env {
					t.Setenv(key, value)
				}
				var baseURL, protocol *string
				if test.protocol != "" {
					protocol = &test.protocol
				}
				if test.baseURL != "" {
					baseURL = &test.baseURL
				}
				if path == "environment" {
					if protocol != nil {
						t.Setenv("ROAST_FINDER_PROTOCOL", *protocol)
					}
					if baseURL != nil {
						t.Setenv("ROAST_FINDER_BASE_URL", *baseURL)
					}
					return resolveFinderProfile(t, test.profile)
				}
				return OverrideEndpoint("finder", resolveFinderProfile(t, test.profile), baseURL, protocol)
			}
			fromEnv, fromFlags := resolve("environment"), resolve("flags")
			for path, role := range map[string]ResolvedRole{"environment": fromEnv, "flags": fromFlags} {
				if role.BaseURL != test.wantEndpoint || role.APIKey != test.wantKey {
					t.Errorf("%s: endpoint=%q key=%q source=%v, want endpoint=%q key=%q", path, role.BaseURL, role.APIKey, role.Credential, test.wantEndpoint, test.wantKey)
				}
			}
			if fromEnv.BaseURL != fromFlags.BaseURL || fromEnv.Protocol != fromFlags.Protocol || fromEnv.APIKey != fromFlags.APIKey ||
				fromEnv.Credential != fromFlags.Credential || fromEnv.ExplicitEndpoint != fromFlags.ExplicitEndpoint {
				t.Fatalf("environment and flag overrides diverged:\n env:   %+v\n flags: %+v", fromEnv, fromFlags)
			}
		})
	}
}

func TestRebindCredential(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "openai")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic")
	ambient := ResolvedRole{BaseURL: DefaultBaseURL("responses"), Protocol: "responses", APIKey: "openai", Credential: CredentialSource{Kind: CredentialProviderEnv, Env: "OPENAI_API_KEY"}}
	profile := ResolvedRole{BaseURL: customEndpoint, Protocol: "responses", APIKey: "profile", Credential: CredentialSource{Kind: CredentialProfileEnv, Env: "PROFILE_KEY"}}
	for name, test := range map[string]struct {
		role     ResolvedRole
		endpoint string
		protocol string
		crossed  bool
		wantKey  string
	}{
		"ambient key to custom endpoint":     {ambient, customEndpoint, "responses", false, ""},
		"ambient key to other provider":      {ambient, DefaultBaseURL("anthropic"), "anthropic", true, "anthropic"},
		"ambient key protocol on gateway":    {ambient, DefaultBaseURL("responses"), "anthropic", false, ""},
		"profile key follows explicit flag":  {profile, "https://flag.example/v1", "responses", false, "profile"},
		"profile key stays with provider":    {profile, DefaultBaseURL("anthropic"), "anthropic", true, "anthropic"},
		"ambient key at the default address": {ambient, DefaultBaseURL("chat"), "chat", false, "openai"},
	} {
		t.Run(name, func(t *testing.T) {
			role := test.role
			role.BaseURL, role.Protocol = test.endpoint, test.protocol
			if got := rebindCredential("finder", role, test.crossed); got.APIKey != test.wantKey {
				t.Fatalf("key = %q from %v, want %q", got.APIKey, got.Credential, test.wantKey)
			}
		})
	}
}

func TestCredentialSourceNeverIncludesSecret(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("PROFILE_KEY", "super-secret-value")
	role := resolveFinderProfile(t, "    model: m\n    api_key_env: PROFILE_KEY\n")
	if got := role.Credential.String(); got != "PROFILE_KEY (profile api_key_env)" || strings.Contains(got, "super-secret-value") {
		t.Fatalf("credential source = %q", got)
	}
	literal := resolveFinderProfile(t, "    model: m\n    api_key: super-secret-value\n")
	if strings.Contains(literal.Credential.String(), "super-secret-value") {
		t.Fatal("literal api_key leaked into the credential description")
	}
}

// resolveFinderProfile resolves the finder role from one profile body.
func resolveFinderProfile(t *testing.T, profile string) ResolvedRole {
	t.Helper()
	cfg, err := Load(writeConfig(t, "version: 1\ninference_profiles:\n  finder:\n"+profile+"  verifier:\n    model: v\n"))
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	return role
}

// clearProviderEnv blanks the variables resolution reads so one subtest's
// settings cannot leak into the next.
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL",
		"ROAST_FINDER_API_KEY", "ROAST_FINDER_BASE_URL", "ROAST_FINDER_PROTOCOL", "ROAST_FINDER_MODEL", "ROAST_FINDER_MAX_OUTPUT_TOKENS",
	} {
		t.Setenv(name, "")
	}
}

func TestMain(m *testing.M) {
	testutil.ClearAmbientEnvironment()
	os.Exit(m.Run())
}
