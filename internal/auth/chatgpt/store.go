package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

type index struct {
	HostID           string   `json:"host_id"`
	Active           string   `json:"active"`
	Clients          []string `json:"clients"`
	SelectionVersion uint64   `json:"selection_version,omitempty"`
}

// setActive assigns a fresh equality token on every selection write. An older
// writer can erase the revision, so deterministic increments could recreate a
// snapshot still held by a pending authorization. Call under the session lock.
func (i *index) setActive(id string) {
	var b [8]byte
	for {
		rand.Read(b[:])
		if v := binary.BigEndian.Uint64(b[:]); v != 0 && v != i.SelectionVersion {
			i.Active = id
			i.SelectionVersion = v
			return
		}
	}
}

func (c *Client) credentialPath(id string) string {
	h := sha256.Sum256([]byte(id))
	return filepath.Join(c.dir, hex.EncodeToString(h[:])+".json")
}

func (c *Client) withSessionLock(ctx context.Context, fn func() error) error {
	return storage.WithLock(ctx, c.dir, "session", func() error {
		if err := storage.CleanupTemps(c.dir); err != nil {
			return fmt.Errorf("clean up interrupted ChatGPT credential writes: %w", err)
		}
		return fn()
	})
}

// withAuthorizationLock serializes sign-ins for one registration. Browser
// waits leave the session lock free so other accounts can use the store.
func (c *Client) withAuthorizationLock(ctx context.Context, id string, fn func() error) error {
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

func (c *Client) loadCredential(id string) (credential, error) {
	var v credential
	err := storage.ReadJSON(c.credentialPath(id), &v)
	if err == nil && v.ClientID != id {
		return v, errors.New("ChatGPT registration identity mismatch")
	}
	return v, err
}

// Every write invalidates older sign-in snapshots, even a repeated sign-out.
func (c *Client) saveCredential(v *credential) error {
	v.Generation = rand.Text()
	return storage.WriteJSON(c.credentialPath(v.ClientID), v)
}
