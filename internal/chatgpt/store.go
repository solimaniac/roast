// Package chatgpt owns Roast's independent Sign in with ChatGPT registrations.
// It never reads Codex credentials or accepts endpoint or token overrides.
package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Account struct {
	PendingRegistration bool      `json:"pending_registration,omitempty"`
	ClientID            string    `json:"client_id"`
	Subject             string    `json:"subject"`
	Email               string    `json:"email,omitempty"`
	Active              bool      `json:"active"`
	SignedIn            bool      `json:"signed_in"`
	PlanEnabled         bool      `json:"plan_enabled"`
	ExpiresAt           time.Time `json:"expires_at"`
}
type pendingRefresh struct {
	Response   tokenResponse `json:"response"`
	ReceivedAt time.Time     `json:"received_at"`
}

type credential struct {
	Pending *pendingRefresh `json:"pending_refresh,omitempty"`
	Account
	IDToken           string `json:"id_token,omitempty"`
	AccessToken       string `json:"access_token,omitempty"`
	RefreshToken      string `json:"refresh_token,omitempty"`
	Scopes            string `json:"scope,omitempty"`
	EarliestRefreshAt int64  `json:"earliest_refresh_at,omitempty"`
}
type index struct {
	HostID  string   `json:"host_id"`
	Active  string   `json:"active"`
	Clients []string `json:"clients"`
}

func randomValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func (c *Client) path(id string) string {
	h := sha256.Sum256([]byte(id))
	return filepath.Join(c.dir, hex.EncodeToString(h[:])+".json")
}
func secureDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
		return errors.New("ChatGPT credential directory must be a private directory (0700)")
	}
	return nil
}
func readJSON(path string, v any) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return errors.New("ChatGPT credential file must be a private regular file (0600)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if json.Unmarshal(b, v) != nil {
		return errors.New("invalid ChatGPT credential file")
	}
	return nil
}
func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	// Persist the directory entry as well as file contents before considering a
	// rotated refresh token durable (direct auth is supported on Unix only).
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (c *Client) locked(ctx context.Context, fn func() error) error {
	if err := secureDir(c.dir); err != nil {
		return err
	}
	// Kernel-owned advisory locks are released even if Roast is killed. Never
	// unlink this file: doing so would allow separate processes to lock two inodes.
	lock := filepath.Join(c.dir, "session.lock")
	if st, err := os.Lstat(lock); err == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0) {
		return errors.New("ChatGPT lock must be a private regular file")
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		ok, err := tryLock(f)
		if err != nil {
			return err
		}
		if ok {
			break
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("waiting for ChatGPT session lock: %w", waitCtx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}

	return fn()
}
func (c *Client) loadIndex() (index, error) {
	var i index
	err := readJSON(filepath.Join(c.dir, "accounts.json"), &i)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return i, err
}
func (c *Client) saveIndex(i index) error { return writeJSON(filepath.Join(c.dir, "accounts.json"), i) }
func (c *Client) load(id string) (credential, error) {
	var v credential
	err := readJSON(c.path(id), &v)
	if err == nil && v.ClientID != id {
		return v, errors.New("ChatGPT registration identity mismatch")
	}
	return v, err
}
func (c *Client) save(v credential) error { return writeJSON(c.path(v.ClientID), v) }
func (c *Client) Status(ctx context.Context) ([]Account, error) {
	if _, err := os.Lstat(c.dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var out []Account
	err := c.locked(ctx, func() error {
		i, e := c.loadIndex()
		if e != nil {
			return e
		}
		for _, id := range i.Clients {
			v, e := c.load(id)
			if e != nil {
				return e
			}
			a := v.Account
			a.Active = id == i.Active
			a.SignedIn = v.AccessToken != "" || v.IDToken != ""
			a.PlanEnabled = hasScope(v.Scopes, "chatgpt.tokens.use.direct") && hasScope(v.Scopes, "resource.invoke")
			out = append(out, a)
		}
		return nil
	})
	return out, err
}
func (c *Client) Select(ctx context.Context, id string) error {
	return c.locked(ctx, func() error {
		i, e := c.loadIndex()
		if e != nil {
			return e
		}
		v, e := c.load(id)
		if e != nil {
			return e
		}
		if v.AccessToken == "" && v.IDToken == "" {
			return errors.New("account is signed out; run roast chatgpt signin --account CLIENT_ID")
		}
		i.Active = id
		return c.saveIndex(i)
	})
}

// PrepareHost explicitly initializes only this runtime's stable host identity.
// It creates no OAuth client registration or token.
func (c *Client) PrepareHost(ctx context.Context) (hostID, dir string, err error) {
	err = c.locked(ctx, func() error {
		i, e := c.loadIndex()
		if e != nil {
			return e
		}
		if i.HostID == "" {
			r, e := randomValue()
			if e != nil {
				return e
			}
			i.HostID = "roast-" + r
			if e = c.saveIndex(i); e != nil {
				return e
			}
		}
		hostID = i.HostID
		return nil
	})
	return hostID, c.dir, err
}
