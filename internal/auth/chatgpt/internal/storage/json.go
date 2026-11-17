package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
