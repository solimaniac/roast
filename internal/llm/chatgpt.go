package llm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	fantasyopenai "charm.land/fantasy/providers/openai"
	"github.com/charmbracelet/openai-go/option"

	"github.com/bluesky-social/roast/internal/auth/chatgpt"
)

const chatGPTBaseURL = chatgpt.APIBaseURL
const chatGPTToolNamespace = "roast"

func newChatGPTClient(ctx context.Context, cfg ClientConfig) (Client, error) {
	if err := validateChatGPTConfig(cfg); err != nil {
		return nil, err
	}
	auth, err := chatGPTAccount(ctx)
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
	// Restoring finalized assistant output can make a small SDK request
	// exceed the limit. Bound the actual wire body before reading credentials.
	if len(body) > defaultInputByteCeiling*4 {
		return nil, fail(errors.New("ChatGPT request exceeds the bounded input size"))
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
