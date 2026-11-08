package config

import (
	"strings"
	"testing"
)

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
	for _, p := range []InferenceProfile{
		{BaseURL: "https://example.com"}, {BaseURL: "https://api.openai.com/v1"}, {APIKey: "secret"}, {APIKeyEnv: "MISSING_KEY"}, {SessionHeader: "X-Session"},
	} {
		p.Protocol, p.Model = "chatgpt", "gpt-test"
		if _, err := normalizeProfile("test", p); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe validation: %v", err)
		}
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
