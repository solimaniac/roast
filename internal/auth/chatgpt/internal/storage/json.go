// Package storage provides private JSON files and cross-process locks for ChatGPT credentials.
package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ReadJSON decodes an owner-only file.
func ReadJSON(path string, v any) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := checkPrivate(path, st, false); err != nil {
		return err
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

// WriteJSON atomically replaces a file with an owner-only JSON document.
// Credential-store callers must hold the session lock through this call.
func WriteJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := privateTemp(filepath.Dir(path))
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
	return commitPrivate(f.Name(), path)
}

// CleanupTemps removes abandoned atomic-write files without reading their data.
// The caller must hold the store's session lock: all writers, including older
// Roast versions, hold that lock until their temporary file has been removed.
func CleanupTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".credentials-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		// Never follow symlinks/reparse points or remove directories and files
		// outside the existing private-storage boundary.
		if err := checkPrivate(path, info, false); err != nil {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
