package llm

import (
	"context"

	"github.com/bluesky-social/roast/internal/chatgpt"
)

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
