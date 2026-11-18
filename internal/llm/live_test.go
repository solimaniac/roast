package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

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
