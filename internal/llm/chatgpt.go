package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	fantasyopenai "charm.land/fantasy/providers/openai"
	"github.com/charmbracelet/openai-go/option"

	"github.com/bluesky-social/roast/internal/chatgpt"
)

const chatGPTBaseURL = "https://api.openai.com/v1"
const chatGPTToolNamespace = "roast"

func newChatGPTClient(ctx context.Context, cfg ClientConfig) (Client, error) {
	if err := validateChatGPTConfig(cfg); err != nil {
		return nil, err
	}
	auth, err := chatgpt.New()
	if err != nil {
		return nil, fmt.Errorf("%w: initialize Sign in with ChatGPT: %w", ErrAuth, err)
	}
	return chatGPTClient(ctx, cfg, auth.Token, http.DefaultTransport)
}

func validateChatGPTConfig(cfg ClientConfig) error {
	if cfg.APIKey != "" || cfg.SessionHeader != "" || (cfg.BaseURL != "" && cfg.BaseURL != chatGPTBaseURL) {
		return errors.New("chatgpt uses Roast-managed OAuth and the fixed public Responses endpoint; API keys, session headers, and endpoint overrides are not accepted")
	}
	return nil
}

// chatGPTClient keeps Roast's native Fantasy agent, prompt history, tool schemas,
// execution policy, and response capture. The transport only applies the
// documented ChatGPT plan-usage wire contract. There is no Codex harness.
func chatGPTClient(ctx context.Context, cfg ClientConfig, token func(context.Context) (string, error), base http.RoundTripper) (Client, error) {
	if err := validateChatGPTConfig(cfg); err != nil {
		return nil, err
	}
	headers := sdkEnvironmentHeaders("responses")
	capture := &captureTransport{cfg: cfg}
	capture.base = &chatGPTTransport{base: base, token: token}
	httpClient := &http.Client{Transport: capture, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	opts := append(deleteHeaders(headers), option.WithMaxRetries(0))
	provider, err := fantasyopenai.New(
		// A nonempty placeholder prevents the SDK from selecting ambient API
		// credentials. The transport replaces it on every request, after refresh.
		fantasyopenai.WithAPIKey("roast-managed-oauth"),
		fantasyopenai.WithBaseURL(chatGPTBaseURL),
		fantasyopenai.WithHTTPClient(httpClient),
		fantasyopenai.WithUserAgent(userAgent()),
		fantasyopenai.WithSDKOptions(opts...),
		fantasyopenai.WithUseResponsesAPI(),
		fantasyopenai.WithResponsesAPIFunc(func(string) bool { return true }),
	)
	if err != nil {
		return nil, err
	}
	model, err := provider.LanguageModel(ctx, cfg.Model)
	if err != nil {
		return nil, err
	}
	return &client{cfg: cfg, model: &chatGPTModel{LanguageModel: model}, transport: capture}, nil
}

type chatGPTTransport struct {
	base  http.RoundTripper
	token func(context.Context) (string, error)
}

func (t *chatGPTTransport) RoundTrip(original *http.Request) (*http.Response, error) {
	// Reject before retrieving a credential, including redirects and SDK
	// surprises. This route must never become a configurable token proxy.
	if original.Method != http.MethodPost || original.URL.String() != chatGPTBaseURL+"/responses" {
		return nil, errors.New("chatgpt permits only POST https://api.openai.com/v1/responses")
	}
	owner, _ := original.Context().Value(responseBodyKey{}).(*responseBody)
	fail := func(err error) error {
		if owner != nil {
			owner.failure = err
		}
		return err
	}
	body, err := io.ReadAll(io.LimitReader(original.Body, defaultInputByteCeiling*4+1))
	_ = original.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(body) > defaultInputByteCeiling*4 {
		return nil, fail(errors.New("ChatGPT request exceeds the bounded input size"))
	}
	if state, ok := original.Context().Value(chatGPTCallKey{}).(*chatGPTCall); ok {
		body, err = chatGPTPromptRequest(body, state.prompt)
		if err != nil {
			return nil, fail(err)
		}
	}
	body, err = chatGPTRequest(body)
	if err != nil {
		return nil, fail(err)
	}
	accessToken, err := t.token(original.Context())
	if err != nil {
		if errors.Is(err, chatgpt.ErrAuth) {
			return nil, fail(fmt.Errorf("%w: %w", ErrAuth, err))
		}
		return nil, fail(fmt.Errorf("ChatGPT credential: %w", err))
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, fail(fmt.Errorf("%w: no ChatGPT access token; run roast chatgpt signin", ErrAuth))
	}
	req := original.Clone(original.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+accessToken)
	// These headers have no role in this account's OAuth context.
	req.Header.Del("OpenAI-Organization")
	req.Header.Del("OpenAI-Project")
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fail(readErr)
		}
		// Keep the original body shape, status, and request ID for diagnostics,
		// but never retain an echoed bearer credential in a diagnostic.
		data = bytes.ReplaceAll(data, []byte(accessToken), []byte("[redacted]"))
		resp.Body = io.NopCloser(bytes.NewReader(data))
		fail(newChatGPTError(resp.StatusCode, resp.Header.Get("X-Request-Id"), data))
		return resp, nil
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "text/event-stream" {
		_ = resp.Body.Close()
		return nil, fail(errors.New("ChatGPT returned a non-streaming response; no tools were executed"))
	}
	state, _ := original.Context().Value(chatGPTCallKey{}).(*chatGPTCall)
	resp.Body = &chatGPTStreamBody{
		call: state,
		body: resp.Body, reader: bufio.NewReader(resp.Body), fail: fail,
		requestID: resp.Header.Get("X-Request-Id"),
	}
	return resp, nil
}

// The preview does not accept ordinary Responses defaults. Keep this boundary
// explicit so SDK updates cannot accidentally enable storage or API billing.
func chatGPTRequest(body []byte) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("decode ChatGPT request: %w", err)
	}
	if request == nil {
		return nil, errors.New("ChatGPT request must be an object")
	}
	request["include"] = json.RawMessage(`["reasoning.encrypted_content"]`)
	for _, key := range []string{
		"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata", "moderation", "multi_agent", "prompt",
		"prompt_cache_retention", "safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user", "previous_response_id",
	} {
		delete(request, key)
	}
	request["store"], request["stream"] = json.RawMessage("false"), json.RawMessage("true")
	var input []map[string]json.RawMessage
	if err := json.Unmarshal(request["input"], &input); err != nil || input == nil {
		return nil, errors.New("ChatGPT input must be an array containing the conversation history")
	}
	for _, item := range input {
		if string(item["role"]) == `"system"` {
			item["role"] = json.RawMessage(`"developer"`)
		}
		if string(item["type"]) == `"function_call"` {
			// Fantasy currently drops the namespace from function-call history.
			// All calls admitted by the stream guard belong to this namespace.
			item["namespace"] = json.RawMessage(`"roast"`)
		}
	}
	request["input"], _ = json.Marshal(input)
	var tools []map[string]json.RawMessage
	if raw := request["tools"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("decode ChatGPT tools: %w", err)
		}
	}
	for _, tool := range tools {
		if string(tool["type"]) != `"function"` {
			return nil, errors.New("ChatGPT Roast supports only local function tools")
		}
	}
	if len(tools) > 0 {
		request["tools"], _ = json.Marshal([]any{map[string]any{
			"type": "namespace", "name": chatGPTToolNamespace,
			"description": "Roast code review and final submission tools.", "tools": tools,
		}})
	}
	return json.Marshal(request)
}

// chatGPTError preserves admission and streaming error details and implements
// Fantasy's retry policy. Usage exhaustion and unsupported requests must not
// spin or silently switch to API-key billing.
type chatGPTError struct {
	StatusCode int
	RequestID  string
	Body       string
	Code       string
	Param      string
}

func newChatGPTError(status int, requestID string, body []byte) *chatGPTError {
	e := &chatGPTError{StatusCode: status, RequestID: requestID, Body: string(body)}
	var wire struct {
		Code  string `json:"code"`
		Param string `json:"param"`
		Error *struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &wire)
	e.Code, e.Param = wire.Code, wire.Param
	if wire.Error != nil {
		e.Code, e.Param = wire.Error.Code, wire.Error.Param
	}
	return e
}

func (e *chatGPTError) Error() string {
	detail := fmt.Sprintf("ChatGPT plan usage failed (HTTP %d, code %q, param %q, request %q): %s", e.StatusCode, e.Code, e.Param, e.RequestID, e.Body)
	switch e.Code {
	case "subscription_sharing_usage_limit_exceeded":
		detail += "; pause new plan-usage requests and check https://chatgpt.com/settings/usage (the reset time and scope of this limit are not known)"
	case "subscription_sharing_user_not_eligible":
		detail += "; this user, workspace, or policy is not eligible; repeating OAuth will not resolve eligibility"
	case "subscription_sharing_unsupported_capability":
		detail += "; remove the unsupported capability rather than retrying the same request"
	}
	return detail
}

func (e *chatGPTError) IsRetryable() bool {
	return e.StatusCode == http.StatusServiceUnavailable && (e.Code == "" || e.Code == "subscription_sharing_usage_unavailable" || e.Code == "subscription_sharing_user_unavailable")
}

// Validate complete SSE frames before passing their unchanged bytes to the
// SDK. In particular, response.incomplete must never masquerade as a successful
// tool submission even if an SDK maps its missing finish reason to "stop".
type chatGPTStreamBody struct {
	body      io.ReadCloser
	reader    *bufio.Reader
	pending   []byte
	completed bool
	err       error
	fail      func(error) error
	requestID string
	call      *chatGPTCall
}

func (b *chatGPTStreamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.pending) == 0 && b.err == nil {
		var event bytes.Buffer
		var data []string
		for {
			line, err := b.readLine()
			if event.Len()+len(line) > maxCapturedResponseSize {
				b.err = b.fail(errors.New("ChatGPT stream event exceeds the bounded size"))
				break
			}
			event.WriteString(line)
			trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if strings.HasPrefix(trimmed, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " "))
			}
			if trimmed == "" || err != nil {
				if len(data) > 0 {
					if validationErr := b.validate([]byte(strings.Join(data, "\n"))); validationErr != nil {
						b.err = b.fail(validationErr)
					}
				}
				if err != nil && b.err == nil {
					b.err = err
					if errors.Is(err, io.EOF) && !b.completed {
						b.err = b.fail(errors.New("ChatGPT stream ended without response.completed; no tools were executed"))
					}
				}
				break
			}
		}
		b.pending = event.Bytes()
	}
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		return n, nil
	}
	return 0, b.err
}

// ReadSlice bounds allocation even when a peer never sends a newline.
func (b *chatGPTStreamBody) readLine() (string, error) {
	var line bytes.Buffer
	for {
		fragment, err := b.reader.ReadSlice('\n')
		if line.Len()+len(fragment) > maxCapturedResponseSize {
			return "", errors.New("ChatGPT stream line exceeds the bounded size")
		}
		line.Write(fragment)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line.String(), err
		}
	}
}

func (b *chatGPTStreamBody) Close() error { return b.body.Close() }

func (b *chatGPTStreamBody) validate(data []byte) error {
	if bytes.Equal(data, []byte("[DONE]")) {
		if !b.completed {
			return errors.New("ChatGPT stream ended before response.completed")
		}
		return nil
	}
	var event struct {
		Type     string          `json:"type"`
		Item     json.RawMessage `json:"item"`
		Response struct {
			Status string            `json:"status"`
			Error  json.RawMessage   `json:"error"`
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("invalid ChatGPT stream event: %w", err)
	}
	checkItem := func(raw json.RawMessage) error {
		var item struct {
			Type      string `json:"type"`
			Namespace string `json:"namespace"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		if item.Type == "function_call" && item.Namespace != chatGPTToolNamespace {
			return errors.New("ChatGPT returned a function outside the Roast tool namespace; no tools were executed")
		}
		if item.Type != "function_call" && item.Type != "message" && item.Type != "reasoning" {
			return fmt.Errorf("ChatGPT returned unsupported output type %q; no tools were executed", item.Type)
		}
		return nil
	}
	if len(event.Item) > 0 {
		if err := checkItem(event.Item); err != nil {
			return err
		}
	}
	for _, item := range event.Response.Output {
		if err := checkItem(item); err != nil {
			return err
		}
	}
	switch event.Type {
	case "response.completed":
		if event.Response.Status != "completed" {
			return errors.New("ChatGPT completion event has a non-completed status")
		}
		b.completed = true
		if b.call != nil {
			b.call.output.Items = event.Response.Output
		}
	case "response.incomplete":
		return errors.New("ChatGPT response was incomplete; no tools were executed (max_output_tokens is unsupported on this route)")
	case "response.failed":
		return newChatGPTError(http.StatusOK, b.requestID, event.Response.Error)
	case "error":
		return newChatGPTError(http.StatusOK, b.requestID, data)
	}
	return nil
}
