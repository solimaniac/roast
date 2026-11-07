package chatgpt

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// newHostID uses the supported UUID URI format without creating a key pair.
func newHostID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // RFC 9562 UUID version 4.
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 9562 variant 10xx.
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func legacyHostID(id string) bool {
	if !strings.HasPrefix(id, "roast-") || len(id) != len("roast-")+64 {
		return false
	}
	raw := strings.TrimPrefix(id, "roast-")
	_, err := hex.DecodeString(raw)
	return err == nil && raw == strings.ToLower(raw)
}

// ensureHost runs under the store lock. Only an unused legacy host can migrate:
// even an orphaned registration file must prevent a silent identity replacement.
func (c *Client) ensureHost(i *index) error {
	if i.HostID != "" && !legacyHostID(i.HostID) {
		return nil
	}
	registered := i.Active != "" || len(i.Clients) > 0
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "accounts.json" && strings.HasSuffix(entry.Name(), ".json") {
			registered = true
		}
	}
	if registered {
		return errors.New("ChatGPT host identity is missing or uses a legacy unsupported format, but registrations exist; refusing to replace the registered host identity")
	}
	id, err := newHostID()
	if err != nil {
		return err
	}
	i.HostID = id
	return c.saveIndex(*i)
}
