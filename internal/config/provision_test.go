package config

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const provisionTemplate = `version: 1
provisioning:
  import_legacy_credential: true
inference_profiles:
  finder:
    endpoint: https://models.example/v1
    model: finder-model
    protocol: chat
  verifier:
    endpoint: https://models.example/v1
    model: verifier-model
    protocol: responses
roles:
  finder:
    profile: finder
  verifier:
    profile: verifier
upload:
  enabled: true
  backend: presigned
  endpoint: https://uploads.example
`

func TestProvisionMigratesLegacyCredentialWithoutRemoteSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(provisionTemplate))
	}))
	defer server.Close()
	legacy := filepath.Join(t.TempDir(), "legacy.yaml")
	if err := os.WriteFile(legacy, []byte("gateway:\n  base_url: https://old.example/v1\n  api_key: legacy-secret\nupload:\n  enabled: true\nroles:\n  finder:\n    model: old-finder\n    api: chat\n  verifier:\n    model: old-verifier\n    api: responses\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "roast", "config.yaml")
	if err := Provision(t.Context(), server.URL, destination, legacy); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(destination)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InferenceProfiles["finder"].APIKey != "legacy-secret" || cfg.Upload.Token != "legacy-secret" {
		t.Fatal("legacy credential was not carried into inference and broker upload")
	}
	if cfg.Upload.Backend != "presigned" || cfg.Upload.Endpoint != "https://uploads.example" {
		t.Fatalf("upload = %#v", cfg.Upload)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, err=%v", info.Mode().Perm(), err)
	}
}

func TestProvisionDoesNotImportLegacyCredentialWithoutExplicitInstruction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Replace(provisionTemplate, "provisioning:\n  import_legacy_credential: true\n", "", 1)))
	}))
	defer server.Close()
	legacy := filepath.Join(t.TempDir(), "legacy.yaml")
	if err := os.WriteFile(legacy, []byte("gateway:\n  api_key: legacy-secret\nroles:\n  finder:\n    model: old\n    api: chat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "config.yaml")
	if err := Provision(t.Context(), server.URL, destination, legacy); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(destination)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InferenceProfiles["finder"].APIKey != "" || cfg.Upload.Token != "" {
		t.Fatal("legacy credential crossed into a template which did not request migration")
	}
}

func TestProvisionRejectsCrossOriginRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(provisionTemplate)) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	if err := Provision(t.Context(), source.URL, filepath.Join(t.TempDir(), "config.yaml"), ""); err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatalf("expected cross-origin redirect rejection, got %v", err)
	}
}

func TestProvisionInPlaceBacksUpLegacyConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("configuration fetch must not send the legacy credential")
		}
		_, _ = w.Write([]byte(provisionTemplate))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	legacy := []byte("gateway:\n  api_key: legacy-secret\nroles:\n  finder:\n    model: old\n    api: chat\ngate:\n  auto_install: false\nsourcebot:\n  url: https://retired-search.example\n  token: discarded-retrieval-key\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Provision(t.Context(), server.URL, path, path); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".pre-v1")
	if err != nil || string(backup) != string(legacy) {
		t.Fatalf("backup mismatch, err=%v", err)
	}
	needs, err := NeedsProvision(path)
	if err != nil || needs {
		t.Fatalf("installed config still needs provisioning: needs=%v err=%v", needs, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"sourcebot", "auto_install", "discarded-retrieval-key", "retired-search.example"} {
		if strings.Contains(string(raw), retired) {
			t.Errorf("retired setting %q was copied into the provisioned config", retired)
		}
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InferenceProfiles["finder"].APIKey != "legacy-secret" || cfg.Upload.Token != "legacy-secret" {
		t.Fatal("legacy gateway credential was not imported into the opted-in template")
	}
}

func TestProvisionInPlaceReusesMatchingLegacyBackup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(provisionTemplate))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	legacy := []byte("gateway:\n  api_key: legacy-secret\nroles:\n  finder:\n    model: old\n    api: chat\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pre-v1", legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Provision(t.Context(), server.URL, path, path); err != nil {
		t.Fatal(err)
	}
	needs, err := NeedsProvision(path)
	if err != nil || needs {
		t.Fatalf("installed config still needs provisioning: needs=%v err=%v", needs, err)
	}
}

func TestProvisionInPlaceRejectsMismatchedLegacyBackup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(provisionTemplate))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	legacy := []byte("gateway:\n  api_key: legacy-secret\nroles:\n  finder:\n    model: old\n    api: chat\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pre-v1", []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Provision(t.Context(), server.URL, path, path)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected mismatched backup rejection, got %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != string(legacy) {
		t.Fatalf("legacy config changed after rejected backup: %q, err=%v", after, readErr)
	}
}

func TestCopyExclusiveRemovesPartialDestination(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "backup")
	err := copyExclusive(destination, []byte("legacy config"), 0o600, func(file *os.File, raw []byte) error {
		if _, err := file.Write(raw[:4]); err != nil {
			return err
		}
		return errors.New("injected persistence failure")
	})
	if err == nil || !strings.Contains(err.Error(), "injected persistence failure") {
		t.Fatalf("expected injected failure, got %v", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial destination was not removed: %v", err)
	}
}

func TestProvisionRejectsTemplatePromptOverrides(t *testing.T) {
	for _, field := range []string{"finder_code", "finder_prose", "verifier"} {
		t.Run(field, func(t *testing.T) {
			body := provisionTemplate + "prompts:\n  " + field + ": /etc/hosts\n"
			if _, err := ValidateProvisionDocument([]byte(body)); err == nil || !strings.Contains(err.Error(), "prompts."+field) {
				t.Fatalf("template prompt override accepted: %v", err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			destination := filepath.Join(t.TempDir(), "config.yaml")
			if err := Provision(t.Context(), server.URL, destination, ""); err == nil {
				t.Fatal("template with prompt overrides was installed")
			}
			if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected template wrote a config: %v", err)
			}
		})
	}
}

func TestPlanProvisionWritesNothingUntilInstall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(provisionTemplate)) }))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "roast", "config.yaml")
	plan, err := PlanProvision(t.Context(), server.URL, destination, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planning wrote the config: %v", err)
	}
	if !plan.TemplateUpload() || plan.Config.Upload.Endpoint != "https://uploads.example" || len(plan.ProtocolUpdates) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
	if err := plan.Install(); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(destination); err != nil {
		t.Fatal(err)
	}
}

func TestInstallRefusesConfigChangedAfterPlanning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(nativeTemplate())) }))
	defer server.Close()
	for name, change := range map[string]func(path string) error{
		"edited":  func(path string) error { return os.WriteFile(path, []byte(compatibilityConfig+"# edit\n"), 0o600) },
		"removed": os.Remove,
		"created": func(string) error { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if name != "created" {
				if err := os.WriteFile(path, []byte(compatibilityConfig), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := PlanProvision(t.Context(), server.URL, path, "")
			if name == "created" {
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("version: 1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err != nil || len(plan.ProtocolUpdates) != 2 || plan.TemplateUpload() {
					t.Fatalf("protocol update plan = %#v, err=%v", plan, err)
				}
				if err := change(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := plan.Install(); err == nil {
				t.Fatal("install overwrote a config that changed after planning")
			}
		})
	}
}

func TestProvisionFetchFailuresAreFetchErrors(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusBadGateway) }))
	defer failing.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(provisionTemplate)) }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer redirect.Close()
	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("version: 1\nunknown: true\n")) }))
	defer invalid.Close()
	for name, test := range map[string]struct {
		url   string
		fetch bool
	}{
		"http status":        {failing.URL, true},
		"connection refused": {closedURL, true},
		"plaintext remote":   {"http://config.example/roast", false},
		"cross-origin":       {redirect.URL, false},
		"invalid template":   {invalid.URL, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := PlanProvision(t.Context(), test.url, filepath.Join(t.TempDir(), "config.yaml"), "")
			var fetch *FetchError
			if err == nil || errors.As(err, &fetch) != test.fetch {
				t.Fatalf("err=%v, fetch error=%v, want %v", err, errors.As(err, &fetch), test.fetch)
			}
		})
	}
}

func TestProvisionKeepsUnsetTemplateEndpointsUnset(t *testing.T) {
	template := "version: 1\ninference_profiles:\n  finder:\n    model: m\n  verifier:\n    model: v\n    protocol: anthropic\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(template)) }))
	defer server.Close()
	plan, err := PlanProvision(t.Context(), server.URL, filepath.Join(t.TempDir(), "config.yaml"), "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Config.InferenceProfiles["finder"].BaseURL != "" || plan.Config.InferenceProfiles["verifier"].Endpoint() != DefaultBaseURL("anthropic") {
		t.Fatalf("profiles = %#v", plan.Config.InferenceProfiles)
	}
}

func TestProvisionAcceptsTemplateSessionHeader(t *testing.T) {
	sessionTemplate := strings.Replace(provisionTemplate, "    protocol: chat\n", "    protocol: chat\n    session_header: x-opencode-session\n", 1)
	for name, test := range map[string]struct {
		template string
		want     string
		legacy   bool
	}{
		"set":                  {template: sessionTemplate, want: "x-opencode-session"},
		"set, legacy import":   {template: sessionTemplate, want: "x-opencode-session", legacy: true},
		"unset, legacy import": {template: provisionTemplate, legacy: true},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(test.template)) }))
			defer server.Close()
			legacy := ""
			if test.legacy {
				legacy = filepath.Join(t.TempDir(), "legacy.yaml")
				if err := os.WriteFile(legacy, []byte("gateway:\n  api_key: legacy-secret\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			destination := filepath.Join(t.TempDir(), "config.yaml")
			if err := Provision(t.Context(), server.URL, destination, legacy); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(destination)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.InferenceProfiles["finder"].SessionHeader != test.want || cfg.InferenceProfiles["verifier"].SessionHeader != "" {
				t.Fatalf("profiles = %#v, want finder session_header %q", cfg.InferenceProfiles, test.want)
			}
			// A re-encoded config without the setting must stay readable by
			// releases that predate it.
			if raw, err := os.ReadFile(destination); err != nil || (test.want == "" && strings.Contains(string(raw), "session_header")) {
				t.Fatalf("installed config mentions an unset session_header (read error: %v)", err)
			}
		})
	}
	rejected := strings.Replace(provisionTemplate, "    protocol: chat\n", "    protocol: chat\n    session_header: Authorization\n", 1)
	if _, err := ValidateProvisionDocument([]byte(rejected)); err == nil || !strings.Contains(err.Error(), "inference_profiles.finder.session_header") {
		t.Fatalf("template naming a reserved session header: err = %v", err)
	}
}

func TestProvisionRejectsUnversionedOrSecretRemoteConfig(t *testing.T) {
	for name, body := range map[string]string{
		"unversioned": strings.Replace(provisionTemplate, "version: 1\n", "", 1),
		"secret":      strings.Replace(provisionTemplate, "    protocol: chat\n", "    protocol: chat\n    api_key: leaked\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			if err := Provision(t.Context(), server.URL, filepath.Join(t.TempDir(), "config.yaml"), ""); err == nil {
				t.Fatal("expected provisioning failure")
			}
		})
	}
}

const controlTemplate = `version: 1
inference_profiles:
  finder:
    endpoint: https://models.example/v1
    model: finder-model
    protocol: chat
    api_key_env: FINDER_KEY
    session_header: x-gateway-session
  verifier:
    model: verifier-model
roles:
  finder:
    profile: finder
    author_routes:
      openai: verifier
  verifier:
    profile: verifier
review:
  excludes:
    - vendor
upload:
  enabled: true
  endpoint: https://uploads.example
  bucket: reviews
  prefix: roast-runs
  region: us-east-1
`

func TestProvisionRejectsTemplateControlCharacters(t *testing.T) {
	if _, err := ValidateProvisionDocument([]byte(controlTemplate)); err != nil {
		t.Fatalf("clean template rejected: %v", err)
	}
	for name, test := range map[string]struct{ old, new, where string }{
		"ESC in bucket":        {"bucket: reviews", `bucket: "reviews\e[2K\rupload: off"`, "upload.bucket"},
		"C1 in prefix":         {"prefix: roast-runs", `prefix: "roast\x9b2Jruns"`, "upload.prefix"},
		"DEL in region":        {"region: us-east-1", `region: "us-east-1\x7f"`, "upload.region"},
		"NEL in upload":        {"endpoint: https://uploads.example", `endpoint: "https://uploads.example\u0085"`, "upload.endpoint"},
		"ESC in endpoint":      {"endpoint: https://models.example/v1", `endpoint: "https://models.example/v1\e[8m"`, "inference_profiles.finder.endpoint"},
		"newline in model":     {"model: finder-model", `model: "finder\nmodel"`, "inference_profiles.finder.model"},
		"ESC in session hdr":   {"session_header: x-gateway-session", `session_header: "x-gateway\e[2Ksession"`, "inference_profiles.finder.session_header"},
		"ESC in profile name":  {"  verifier:\n    model:", "  \"verifier\\e]0;title\\a\":\n    model:", "inference profile name"},
		"ESC in route key":     {"openai: verifier", `"openai\e[31m": verifier`, "roles.finder.author_routes key"},
		"tab in route profile": {"openai: verifier", `openai: "verifier\t"`, "roles.finder.author_routes.openai"},
		"ESC in role profile":  {"    profile: verifier\n", "    profile: \"verifier\\e\"\n", "roles.verifier.profile"},
		"ESC in role name":     {"roles:\n", "roles:\n  \"other\\e\":\n    profile: finder\n", "role name"},
		"ESC in exclude":       {"    - vendor", `    - "vendor\e"`, "review.excludes[0]"},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(controlTemplate, test.old, test.new, 1)
			if body == controlTemplate {
				t.Fatalf("replacement %q did not apply", test.old)
			}
			_, err := ValidateProvisionDocument([]byte(body))
			if err == nil || !strings.Contains(err.Error(), "control characters") || !strings.Contains(err.Error(), test.where) {
				t.Fatalf("err = %v, want a control-character error naming %s", err, test.where)
			}
			if hasControlCharacter(err.Error()) {
				t.Fatalf("error repeats the control character: %q", err.Error())
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			destination := filepath.Join(t.TempDir(), "config.yaml")
			if err := Provision(t.Context(), server.URL, destination, ""); err == nil {
				t.Fatal("template with a control character was installed")
			}
			if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected template wrote a config: %v", err)
			}
		})
	}
}

func TestHasControlCharacter(t *testing.T) {
	for _, s := range []string{"\x1b", "a\tb", "a\nb", "\r", "\x00", "\x7f", "\u0080", "\u009b", "\xff"} {
		if !hasControlCharacter(s) {
			t.Errorf("hasControlCharacter(%q) = false", s)
		}
	}
	for _, s := range []string{"", "plain", "modèle ✓", " ", "ÿ", "https://models.example/v1"} {
		if hasControlCharacter(s) {
			t.Errorf("hasControlCharacter(%q) = true", s)
		}
	}
}

const compatibilityConfig = `version: 1
# Keep this local configuration, not the template's defaults.
inference_profiles:
  finder:
    endpoint: https://models.example/v1
    model: claude-opus-5
    protocol: chat # keep the comment
    api_key: local-secret
    max_output_tokens: 8192
  verifier:
    endpoint: https://models.example/v1
    model: gpt-custom
    protocol: responses
    api_key_env: TEST_VERIFIER_KEY
  alternative:
    endpoint: https://models.example/v1/
    model: anthropic/claude-sonnet-5
    protocol: responses
roles:
  finder:
    profile: finder
    pin: true
    author_routes:
      openai: alternative
  verifier:
    profile: verifier
review:
  confidence_floor: 0.7
  excludes: [local-notes.md]
prompts:
  verifier: local-verifier.md
upload:
  enabled: false
  backend: presigned
  endpoint: https://local-upload.example
  token: local-upload-secret
`

func nativeTemplate() string {
	raw := strings.Replace(provisionTemplate, "model: finder-model", "model: claude-new-default", 1)
	return strings.Replace(raw, "protocol: chat", "protocol: anthropic", 1)
}

func TestProvisionUpdatesOnlyMatchingProfileProtocols(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("local credentials reached the template server")
		}
		_, _ = w.Write([]byte(nativeTemplate()))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(compatibilityConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if needs, err := NeedsProvision(path); err != nil || !needs {
		t.Fatalf("needs update=%v, error=%v", needs, err)
	}
	// An existing v1 config must not be parsed as a legacy credential source,
	// even when the automatic enrollment flow passes the same path twice.
	if err := Provision(t.Context(), server.URL, path, path); err != nil {
		t.Fatal(err)
	}
	after, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"finder", "alternative"} {
		profile := before.InferenceProfiles[name]
		profile.Protocol = "anthropic"
		before.InferenceProfiles[name] = profile
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("protocol repair changed unrelated settings")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "# keep the comment") || !strings.Contains(string(raw), "# Keep this local") {
		t.Fatalf("local comments lost, error=%v", err)
	}
	backup, err := os.ReadFile(path + ".pre-protocol-update")
	if err != nil || string(backup) != compatibilityConfig {
		t.Fatalf("original config backup mismatch, error=%v", err)
	}
	for _, file := range []string{path, path + ".pre-protocol-update"} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("config or backup permissions are not owner-only: %s, error=%v", file, err)
		}
	}
	if needs, err := NeedsProvision(path); err != nil || needs {
		t.Fatalf("repaired config still needs update=%v, error=%v", needs, err)
	}
}

func TestProtocolUpdateRequiresTemplateAuthorization(t *testing.T) {
	for name, template := range map[string]string{
		"different endpoint":           strings.ReplaceAll(nativeTemplate(), "https://models.example/v1", "https://other.example/v1"),
		"compatibility is intentional": provisionTemplate,
		"not an Anthropic model":       strings.Replace(nativeTemplate(), "claude-new-default", "gpt-default", 1),
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ValidateProvisionDocument([]byte(template))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := updateProtocols([]byte(compatibilityConfig), cfg); err == nil || !strings.Contains(err.Error(), "no matching") {
				t.Fatalf("unauthorized protocol update: %v", err)
			}
		})
	}
}

func TestProtocolUpdatePreservesUnmatchedProfiles(t *testing.T) {
	raw := strings.Replace(compatibilityConfig, "    endpoint: https://models.example/v1/", "    endpoint: https://compat.example/v1/", 1)
	template, err := ValidateProvisionDocument([]byte(nativeTemplate()))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := updateProtocols([]byte(raw), template)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decode(updated)
	if err != nil || cfg.InferenceProfiles["alternative"].Protocol != "responses" || cfg.InferenceProfiles["finder"].Protocol != "anthropic" {
		t.Fatalf("unmatched compatibility endpoint changed: %v", err)
	}
}

func TestProtocolUpdateFailurePreservesOriginal(t *testing.T) {
	for _, failure := range []string{"backup mismatch", "symlink"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(compatibilityConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			if failure == "backup mismatch" {
				if err := os.WriteFile(path+".pre-protocol-update", []byte("unrelated"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				link := filepath.Join(t.TempDir(), "link.yaml")
				if err := os.Symlink(path, link); err != nil {
					t.Skipf("cannot create symlink: %v", err)
				}
				path = link
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(nativeTemplate())) }))
			defer server.Close()
			if err := Provision(t.Context(), server.URL, path, path); err == nil {
				t.Fatal("expected safe replacement failure")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != compatibilityConfig {
				t.Fatal("failed migration modified the original config")
			}
		})
	}
}

func TestProtocolUpdateRejectsAliasesThatChangeOtherSettings(t *testing.T) {
	raw := strings.Replace(compatibilityConfig, "protocol: chat # keep", "protocol: &shared chat # keep", 1)
	raw = strings.Replace(raw, "verifier: local-verifier.md", "verifier: *shared", 1)
	template, err := ValidateProvisionDocument([]byte(nativeTemplate()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updateProtocols([]byte(raw), template); err == nil || !strings.Contains(err.Error(), "other settings") {
		t.Fatalf("unsafe alias migration not rejected: %v", err)
	}
}

func TestProtocolUpdateCanAddMissingProtocol(t *testing.T) {
	raw := strings.Replace(compatibilityConfig, "    protocol: chat # keep the comment\n", "", 1)
	template, err := ValidateProvisionDocument([]byte(nativeTemplate()))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := updateProtocols([]byte(raw), template)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decode(updated)
	if err != nil || cfg.InferenceProfiles["finder"].Protocol != "anthropic" {
		t.Fatalf("missing protocol not repaired: %v", err)
	}
}

func TestProtocolUpdateAcknowledgementTracksExactConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(compatibilityConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AcknowledgeProtocolUpdate(path); err != nil {
		t.Fatal(err)
	}
	if needs, err := NeedsProvision(path); err != nil || needs {
		t.Fatalf("declined update prompts again: needs=%v error=%v", needs, err)
	}
	ack, err := os.ReadFile(path + ".protocol-update-ack")
	if err != nil || len(ack) != 65 || strings.Contains(string(ack), "secret") {
		t.Fatalf("invalid or secret-bearing acknowledgement: %v", err)
	}
	info, err := os.Stat(path + ".protocol-update-ack")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("acknowledgement is not private: %v", err)
	}
	if err := os.WriteFile(path, []byte(compatibilityConfig+"# user edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if needs, err := NeedsProvision(path); err != nil || !needs {
		t.Fatalf("config edit did not invalidate acknowledgement: needs=%v error=%v", needs, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(nativeTemplate())) }))
	defer server.Close()
	if err := AcknowledgeProtocolUpdate(path); err != nil {
		t.Fatal(err)
	}
	if err := Provision(t.Context(), server.URL, path, ""); err != nil {
		t.Fatalf("acknowledgement blocked explicit config init: %v", err)
	}
}
