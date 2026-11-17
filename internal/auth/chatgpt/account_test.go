package chatgpt

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

func TestSignOutKeepsRegistrationOnNetworkFailure(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })
	e := c.SignOut(context.Background(), "")
	if e == nil || !strings.Contains(e.Error(), "remote revocation was not confirmed") {
		t.Fatal("must explain unconfirmed revocation")
	}
	v, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	if v.AccessToken != "" || v.RefreshToken != "" || v.Subject != "subject" {
		t.Fatal("wrong signout state")
	}
}

func TestSelectedRegistrationPinnedAndIndependent(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	other := credential{Account: Account{ClientID: "oaiapp_other", Subject: "another", ExpiresAt: time.Now().Add(time.Hour)}, AccessToken: "other-access", RefreshToken: "other-refresh", Scopes: oauth.Scope}
	if err := c.saveCredential(&other); err != nil {
		t.Fatal(err)
	}
	i, err := c.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	i.Clients = append(i.Clients, other.ClientID)
	if err := c.saveIndex(i); err != nil {
		t.Fatal(err)
	}
	if err := c.Select(context.Background(), other.ClientID); err != nil {
		t.Fatal(err)
	}
	token, err := c.Token(context.Background())
	if err != nil || token != "access" {
		t.Fatal("account switch changed in-flight client")
	}
	accounts, err := c.Status(context.Background())
	if err != nil || len(accounts) != 2 || !accounts[1].Active || !accounts[1].PlanEnabled {
		t.Fatal("status did not reflect selected account")
	}
}

func TestSignOutRemovesInterruptedCredentialWrites(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	orphan := filepath.Join(c.dir, ".credentials-interrupted")
	if err := storage.WriteJSON(orphan, map[string]string{"refresh_token": "synthetic-orphan"}); err != nil {
		t.Fatal(err)
	}
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })
	if err := c.SignOut(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "signed out locally") {
		t.Fatalf("sign-out error = %v", err)
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan remained after sign-out: %v", err)
	}
	saved, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	if saved.signedIn() || saved.RefreshToken != "" || saved.Pending != nil {
		t.Fatal("sign-out retained credentials")
	}
}
