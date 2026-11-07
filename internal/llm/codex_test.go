package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func codexEvents(name, arguments string) []byte {
	body, _ := json.Marshal(codexToolCall{Name: name, Arguments: arguments})
	event, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": string(body)}})
	return append(event, []byte("\n"+`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":30,"output_tokens":5}}`+"\n")...)
}

func TestCodexConversationUsesRoastTools(t *testing.T) {
	calls := 0
	model := &codexModel{cfg: ClientConfig{Model: "test", Protocol: "codex"}}
	model.run = func(ctx context.Context, prompt, schema []byte) ([]byte, error) {
		calls++
		if !json.Valid(schema) || !strings.Contains(string(prompt), `"input_schema"`) {
			t.Fatal("missing structured schema/tool specification")
		}
		if calls == 1 {
			return codexEvents("read", `{}`), nil
		}
		if !strings.Contains(string(prompt), "scoped-content") {
			t.Fatal("tool result was not replayed")
		}
		return codexEvents("submit", `{"answer":"done"}`), nil
	}
	c := &client{cfg: ClientConfig{Model: "test", Protocol: "codex", MaxOutputTokens: 100, MaxAttempts: 1}, model: model}
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) { return "scoped-content", nil })}, &out)
	if err != nil || out.Answer != "done" || calls != 2 {
		t.Fatalf("answer=%q calls=%d err=%v", out.Answer, calls, err)
	}
	u := c.Usage()
	if u.Calls != 2 || u.UncachedInputTokens != 140 || u.CachedInputTokens != 60 || u.OutputTokens != 10 {
		t.Fatalf("usage=%+v", u)
	}
}

func TestCodexStructuredAndCapture(t *testing.T) {
	captured := 0
	cfg := ClientConfig{Model: "test", Protocol: "codex", MaxOutputTokens: 100, MaxAttempts: 1, CaptureResponse: func(r RawResponse) error {
		captured++
		if r.Protocol != "codex" || r.ContentType != "application/json" || strings.Contains(string(r.Body), "thread") {
			t.Fatalf("capture=%+v", r)
		}
		return nil
	}}
	model := &codexModel{cfg: cfg}
	model.run = func(context.Context, []byte, []byte) ([]byte, error) {
		return codexEvents("submit_result", `{"answer":"done"}`), nil
	}
	c := &client{cfg: cfg, model: model}
	var out struct {
		Answer string `json:"answer"`
	}
	// CompleteStructured's final tool takes its name from schemaName.
	err := c.CompleteStructured(t.Context(), "system", "user", "submit_result", &out)
	if err != nil || out.Answer != "done" || captured != 1 {
		t.Fatalf("out=%+v captured=%d err=%v", out, captured, err)
	}
}

func TestCodexCaptureFailureDoesNotDispatch(t *testing.T) {
	model := &codexModel{cfg: ClientConfig{CaptureResponse: func(RawResponse) error { return errors.New("disk full") }}}
	model.run = func(context.Context, []byte, []byte) ([]byte, error) { return codexEvents("read", `{}`), nil }
	stream, _ := model.Stream(t.Context(), fantasy.Call{Tools: []fantasy.Tool{fantasy.FunctionTool{Name: "read"}}})
	for part := range stream {
		if part.Type != fantasy.StreamPartTypeError {
			t.Fatalf("dispatched despite failed capture: %+v", part)
		}
	}
}

func TestCodexRejectsIncompleteAndUnsafeEvents(t *testing.T) {
	cases := map[string][]byte{
		"empty":                nil,
		"missing-finish":       []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"{}"}}`),
		"unknown-tool":         codexEvents("shell", `{}`),
		"invalid-arguments":    codexEvents("read", `null`),
		"array-arguments":      codexEvents("read", `[]`),
		"failed-after-message": append(codexEvents("read", `{}`), []byte(`{"type":"turn.failed"}`)...),
		"native-tool":          append([]byte(`{"type":"item.started","item":{"type":"command_execution"}}`+"\n"), codexEvents("read", `{}`)...),
		"missing-usage":        []byte(`{"type":"turn.completed"}`),
		"empty-usage":          []byte(`{"type":"turn.completed","usage":{}}`),
		"negative-usage":       []byte(`{"type":"turn.completed","usage":{"input_tokens":-1,"cached_input_tokens":0,"output_tokens":0}}`),
		"invalid-cache-usage":  []byte(`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":2,"output_tokens":0}}`),
		"message-after-finish": append(codexEvents("read", `{}`), []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"{}"}}`)...),
		"malformed":            []byte("not json"),
		"unknown-event":        []byte(`{"type":"future-event"}`),
		"duplicate-finish":     append(codexEvents("read", `{}`), []byte(`{"type":"turn.completed","usage":{}}`)...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseCodexEvents(data, map[string]bool{"read": true})
			if err == nil {
				t.Fatal("accepted unsafe/incomplete output")
			}
			if strings.Contains(name, "usage") && !strings.Contains(err.Error(), "completion accounting") {
				t.Fatalf("expected accounting validation, got %v", err)
			}
		})
	}
}

func TestCodexLaunchIsIsolatedAndSubscriptionOnly(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	t.Setenv("CODEX_API_KEY", "must-not-leak")
	t.Setenv("OPENAI_BASE_URL", "https://attacker.invalid")
	t.Setenv("CODEX_HOME", "/approved/codex")
	t.Setenv("HTTPS_PROXY", "http://managed-proxy.invalid:8080")
	t.Setenv("UNRELATED_SECRET", "must-not-leak")
	for _, entry := range codexEnvironment() {
		if strings.Contains(entry, "must-not-leak") || strings.Contains(entry, "attacker") {
			t.Fatal("ambient credentials/endpoint inherited")
		}
	}
	if !slices.Contains(codexEnvironment(), "CODEX_HOME=/approved/codex") {
		t.Fatal("CLI cannot locate its own login")
	}
	if !slices.Contains(codexEnvironment(), "HTTPS_PROXY=http://managed-proxy.invalid:8080") {
		t.Fatal("host network routing was not preserved")
	}
	args := codexArgs("test", "/isolated", "/isolated/schema.json")
	for _, arg := range []string{"--ignore-user-config", "--ephemeral", "read-only", `forced_login_method="chatgpt"`, `model_provider="openai"`, `features.shell_tool=false`, `features.hooks=false`, `features.plugins=false`, `features.apps=false`, `web_search="disabled"`, `project_doc_max_bytes=0`} {
		if !slices.Contains(args, arg) {
			t.Fatalf("missing safety argument %s", arg)
		}
	}
	for _, arg := range args {
		if strings.Contains(arg, "dangerously") {
			t.Fatal("unsafe sandbox bypass")
		}
	}
}

func TestCodexOutputIsBounded(t *testing.T) {
	var b boundedCodexBuffer
	if _, err := b.Write(make([]byte, maxCapturedResponseSize)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("x")); err == nil {
		t.Fatal("accepted oversized output")
	}
}

// Exercise the real subprocess boundary without credentials or network calls.
func fakeCodex(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCodexSubprocessBoundary(t *testing.T) {
	fakeCodex(t, `if [ "$1" = login ]; then echo 'Logged in using ChatGPT'; exit 0; fi
if [ "$OPENAI_API_KEY" != '' ] || [ "$UNRELATED_SECRET" != '' ]; then exit 9; fi
if [ "$1" != exec ]; then exit 8; fi
found_schema=no
while [ "$#" -gt 0 ]; do
 if [ "$1" = '--output-schema' ]; then shift; test -f "$1" || exit 7; found_schema=yes; fi
 shift
done
test "$found_schema" = yes || exit 6
cat >/dev/null
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"name\":\"submit_result\",\"arguments\":\"{\\\"answer\\\":\\\"done\\\"}\"}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":5}}'
`)
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	t.Setenv("UNRELATED_SECRET", "must-not-leak")
	c, err := New(t.Context(), ClientConfig{Protocol: "codex", BaseURL: codexEndpoint, Model: "test", MaxOutputTokens: 100, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Answer string `json:"answer"`
	}
	if err := c.CompleteStructured(t.Context(), "system", "user", "submit_result", &out); err != nil || out.Answer != "done" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestCodexRejectsAPIKeyLogin(t *testing.T) {
	fakeCodex(t, "echo 'Logged in using an API key'; exit 0\n")
	if err := CheckCodexLogin(t.Context()); !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "API-key login") {
		t.Fatalf("error=%v", err)
	}
}

func TestCodexCancellationStopsProcess(t *testing.T) {
	fakeCodex(t, "exec sleep 30\n")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	m := &codexModel{cfg: ClientConfig{Model: "test"}}
	_, err := m.runCLI(ctx, []byte("prompt"), []byte(`{}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
}

func TestCodexDiagnosticsDoNotExposeStderr(t *testing.T) {
	fakeCodex(t, "echo 'secret-value Read-only file system' >&2; exit 1\n")
	m := &codexModel{cfg: ClientConfig{Model: "test"}}
	_, err := m.runCLI(t.Context(), []byte("prompt"), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "read-only runtime home") || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("error=%v", err)
	}
}

func TestCodexMissingLoginIsAuthenticationError(t *testing.T) {
	fakeCodex(t, "echo 'Not logged in' >&2; exit 1\n")
	if err := CheckCodexLogin(t.Context()); !errors.Is(err, ErrAuth) {
		t.Fatalf("error=%v", err)
	}
}

func TestCodexMissingExecutableIsNotAuthenticationError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := CheckCodexLogin(t.Context()); err == nil || errors.Is(err, ErrAuth) {
		t.Fatalf("error=%v", err)
	}
}

func TestCodexLoginTimeoutBoundsLauncherPipes(t *testing.T) {
	fakeCodex(t, "sleep 3 &\nwait\n")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := CheckCodexLogin(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("login check waited for a descendant's pipe: %v", elapsed)
	}
}

func TestCodexInferenceAuthenticationDiagnostics(t *testing.T) {
	for _, message := range []string{"401 Unauthorized", "403 Forbidden", "refresh_token_reused", "refresh_token_expired", "refresh_token_invalidated"} {
		err := codexCLIError(errors.New("exit"), "private-detail "+message)
		if !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "private-detail") {
			t.Fatalf("message=%q error=%v", message, err)
		}
	}
	if err := codexCLIError(errors.New("exit"), "429 Too Many Requests"); errors.Is(err, ErrAuth) {
		t.Fatalf("quota error misclassified: %v", err)
	}
	for _, data := range [][]byte{
		[]byte(`{"type":"error","message":"401 Unauthorized private-detail"}`),
		[]byte(`{"type":"turn.failed","error":{"message":"refresh_token_expired private-detail"}}`),
	} {
		_, _, err := parseCodexEvents(data, map[string]bool{"read": true})
		if !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "private-detail") || !codexEventsReportAuthFailure(data) {
			t.Fatalf("error=%v", err)
		}
	}
	if codexEventsReportAuthFailure(codexEvents("submit", `{"answer":"401 Unauthorized"}`)) {
		t.Fatal("generated content treated as an authentication diagnostic")
	}
}

func TestCodexSubprocessAuthErrorFromJSON(t *testing.T) {
	fakeCodex(t, "printf '%s\\n' '{\"type\":\"turn.failed\",\"error\":{\"message\":\"401 Unauthorized private-detail\"}}'; exit 1\n")
	m := &codexModel{cfg: ClientConfig{Model: "test"}}
	_, err := m.runCLI(t.Context(), []byte("prompt"), []byte(`{}`))
	if !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "private-detail") {
		t.Fatalf("error=%v", err)
	}
}
