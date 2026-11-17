package chatgpt

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

func TestSignOutKeepsRegistrationOnNetworkFailure(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })
	e := c.SignOut(context.Background(), "")
	if e == nil || !strings.Contains(e.Error(), "remote revocation was not confirmed") {
		t.Fatal("must explain unconfirmed revocation")
	}
	v, err := c.load(c.selected)
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
	if err := c.save(other); err != nil {
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
