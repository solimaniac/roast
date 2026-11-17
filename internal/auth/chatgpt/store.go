package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

// pendingRefresh retains a rotated response until identity validation succeeds.
type pendingRefresh struct {
	Response   oauth.TokenResponse `json:"response"`
	ReceivedAt time.Time           `json:"received_at"`
}

type credential struct {
	Pending *pendingRefresh `json:"pending_refresh,omitempty"`
	Account
	Generation        string `json:"generation,omitempty"`
	IDToken           string `json:"id_token,omitempty"`
	AccessToken       string `json:"access_token,omitempty"`
	RefreshToken      string `json:"refresh_token,omitempty"`
	Scopes            string `json:"scope,omitempty"`
	EarliestRefreshAt int64  `json:"earliest_refresh_at,omitempty"`
}

type index struct {
	HostID           string   `json:"host_id"`
	Active           string   `json:"active"`
	Clients          []string `json:"clients"`
	SelectionVersion uint64   `json:"selection_version,omitempty"`
}

func randomValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// setActive assigns a fresh equality token on every selection write. An older
// writer can erase the revision, so deterministic increments could recreate a
// snapshot still held by a pending authorization. Call under the session lock.
func (i *index) setActive(id string) error {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		if v := binary.BigEndian.Uint64(b[:]); v != 0 && v != i.SelectionVersion {
			i.Active = id
			i.SelectionVersion = v
			return nil
		}
	}
}

func (c *Client) path(id string) string {
	h := sha256.Sum256([]byte(id))
	return filepath.Join(c.dir, hex.EncodeToString(h[:])+".json")
}

func (c *Client) locked(ctx context.Context, fn func() error) error {
	return storage.WithLock(ctx, c.dir, "session", fn)
}

// Authorization waits never own the session lock. Separate registration locks
// prevent overlapping OAuth flows from consuming or replacing each other's
// credentials, while other accounts can sign in and use the store normally.
func (c *Client) authorizationLocked(ctx context.Context, id string, fn func() error) error {
	name := "authorization-new"
	if id != "" {
		h := sha256.Sum256([]byte(id))
		name = "authorization-" + hex.EncodeToString(h[:])
	}
	return storage.WithLock(ctx, c.dir, name, fn)
}

func (c *Client) loadIndex() (index, error) {
	var i index
	err := storage.ReadJSON(filepath.Join(c.dir, "accounts.json"), &i)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return i, err
}
func (c *Client) saveIndex(i index) error {
	return storage.WriteJSON(filepath.Join(c.dir, "accounts.json"), i)
}
func (c *Client) load(id string) (credential, error) {
	var v credential
	err := storage.ReadJSON(c.path(id), &v)
	if err == nil && v.ClientID != id {
		return v, errors.New("ChatGPT registration identity mismatch")
	}
	return v, err
}
func (c *Client) save(v credential) error {
	// Every write, including a repeated sign-out, invalidates an OAuth snapshot.
	// A fresh opaque generation also works with records written by older Roast.
	generation, err := randomValue()
	if err != nil {
		return err
	}
	v.Generation = generation
	return storage.WriteJSON(c.path(v.ClientID), v)
}
