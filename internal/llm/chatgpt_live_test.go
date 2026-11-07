package llm

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

// This is a deliberate opt-in to use Roast's separately authorized account.
// It never accepts token environment variables or reads Codex credentials.
func TestLiveChatGPTTwoTurnReview(t *testing.T) {
	if liveEnv["ROAST_LIVE_CHATGPT"] != "1" {
		t.Skip("set ROAST_LIVE_CHATGPT=1 after roast chatgpt signin to test the direct plan route")
	}
	for _, model := range strings.Split(liveSetting("ROAST_LIVE_CHATGPT_MODELS", "gpt-6.1-sol"), ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
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
}
