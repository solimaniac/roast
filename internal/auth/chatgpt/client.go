// Package chatgpt owns Roast's independent Sign in with ChatGPT registrations.
// The client uses its own credentials and fixed provider endpoints.
package chatgpt

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/loopback"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

// APIBaseURL is the shared endpoint for ChatGPT OAuth and inference.
const APIBaseURL = oauth.APIBaseURL

var ErrAuth = oauth.ErrAuth
var ErrNetwork = oauth.ErrNetwork

type Client struct {
	dir              string
	selected         string
	provider         oauth.Client
	now              func() time.Time
	signInDiagnostic loopback.Diagnostics
}

// New opens Roast's registration store under the operating-system config
// directory and pins the currently selected registration for token requests.
func New() (*Client, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	c := &Client{
		dir:      filepath.Join(dir, "roast", "chatgpt"),
		provider: oauth.NewClient(),
		now:      time.Now,
	}
	i, err := c.loadIndex()
	if err != nil {
		return nil, err
	}
	c.selected = i.Active
	return c, nil
}

type Model = oauth.Model

func (c *Client) Models(ctx context.Context) ([]Model, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}
	return c.provider.Models(ctx, token)
}
