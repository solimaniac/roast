package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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
	return newChatGPTResponsesClient(ctx, cfg, auth.Token, http.DefaultTransport)
}

func validateChatGPTConfig(cfg ClientConfig) error {
	if cfg.APIKey != "" || cfg.SessionHeader != "" || (cfg.BaseURL != "" && cfg.BaseURL != chatGPTBaseURL) {
		return errors.New("chatgpt uses Roast-managed OAuth and the fixed public Responses endpoint; API keys, session headers, and endpoint overrides are not accepted")
	}
	return nil
}

// newChatGPTResponsesClient connects Fantasy's Responses model to the pinned account.
// The outer transport records the same stream that the model consumes.
func newChatGPTResponsesClient(ctx context.Context, cfg ClientConfig, token func(context.Context) (string, error), base http.RoundTripper) (Client, error) {
	headers := sdkEnvironmentHeaders("responses")
	capture := &captureTransport{cfg: cfg, base: &chatGPTTransport{base: base, token: token}}
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

type chatGPTAccountKey struct{}

// The client pins the registration, not its access token. Token refresh and
// sign-out remain effective, but selection changes cannot redirect this work.
// Keep the client private so callers cannot change its authentication state.
type chatGPTAccountSnapshot struct {
	client *chatgpt.Client
	err    error
}

// WithChatGPTAccount captures the selected account for one operation. Call it
// once after resolving the participating roles and before running any of them,
// including when only a lazy verifier uses ChatGPT. A new call always captures
// the current selection, even if ctx belongs to a previous operation.
//
// This reads only the registration index, without fetching tokens or making
// network requests. Initialization errors are deferred until a ChatGPT role is
// used, so an unused verifier cannot fail an otherwise successful review.
func WithChatGPTAccount(ctx context.Context) context.Context {
	client, err := chatgpt.New()
	return context.WithValue(ctx, chatGPTAccountKey{}, chatGPTAccountSnapshot{client, err})
}

func chatGPTAccount(ctx context.Context) (*chatgpt.Client, error) {
	if account, ok := ctx.Value(chatGPTAccountKey{}).(chatGPTAccountSnapshot); ok {
		return account.client, account.err
	}
	return chatgpt.New()
}

// CheckChatGPTLogin validates the operation's pinned account without exposing
// its credentials. Callers without an operation snapshot use the current one.
func CheckChatGPTLogin(ctx context.Context) error {
	auth, err := chatGPTAccount(ctx)
	if err != nil {
		return err
	}
	_, err = auth.Token(ctx)
	return err
}

// ChatGPTModels returns the catalog for the same account used by this
// operation's inference requests and credential check.
func ChatGPTModels(ctx context.Context) ([]chatgpt.Model, error) {
	auth, err := chatGPTAccount(ctx)
	if err != nil {
		return nil, err
	}
	return auth.Models(ctx)
}
