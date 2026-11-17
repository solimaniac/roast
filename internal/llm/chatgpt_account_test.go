package llm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bluesky-social/roast/internal/auth/chatgpt"
)

func TestChatGPTAccountSnapshotKeepsInitializationFailure(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(name, root)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(configDir, "roast", "chatgpt")
	initial, err := chatgpt.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := initial.PrepareHost(t.Context()); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(index, []byte("invalid index"), 0600); err != nil {
		t.Fatal(err)
	}
	operation := WithChatGPTAccount(t.Context())
	if err := os.WriteFile(index, []byte(`{"active":"account-b","clients":["account-b"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Derived request contexts retain the original failed selection instead of
	// silently adopting an account that appeared later in the operation.
	request, cancel := context.WithCancel(operation)
	defer cancel()
	cfg := ClientConfig{Protocol: "chatgpt", Model: "test-model", MaxOutputTokens: 100}
	if _, err := New(request, cfg); !errors.Is(err, ErrAuth) {
		t.Fatalf("original operation error = %v, want cached authentication failure", err)
	}
	if err := CheckChatGPTLogin(request); err == nil {
		t.Fatal("credential check forgot the original initialization failure")
	}
	if _, err := ChatGPTModels(request); err == nil {
		t.Fatal("catalog check forgot the original initialization failure")
	}
	// Starting another operation captures the new selection even if its parent
	// context carries an old snapshot. Construction does not retrieve a token.
	if _, err := New(WithChatGPTAccount(operation), cfg); err != nil {
		t.Fatalf("next operation failed to capture the repaired index: %v", err)
	}
}
