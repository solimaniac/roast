package chatgpt

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

func TestModelsCatalog(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != oauth.APIBaseURL+"/models" || r.Header.Get("Authorization") != "Bearer access" {
			t.Error("incorrect catalog request")
		}
		return jsonResponse(http.StatusOK, map[string]any{"models": []Model{{Slug: "model", DisplayName: "Example model", Visibility: "list"}}})
	})
	models, err := c.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].Slug != "model" {
		t.Fatal("catalog not parsed")
	}
}

func TestNewDoesNotCreateCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Token(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatal("missing login not classified")
	}
	if _, err = os.Stat(c.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only action created credential directory")
	}
}
