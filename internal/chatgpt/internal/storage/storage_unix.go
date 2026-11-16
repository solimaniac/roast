//go:build !windows

package storage

import (
	"errors"
	"os"
	"path/filepath"
)

func checkPrivate(_ string, st os.FileInfo, directory bool) error {
	if st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 ||
		(directory && !st.IsDir()) || (!directory && !st.Mode().IsRegular()) {
		return errors.New("ChatGPT storage must be owner-only (directories 0700, files 0600)")
	}
	return nil
}

func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	return checkPrivate(dir, st, true)
}

func privateTemp(dir string) (*os.File, error) { return os.CreateTemp(dir, ".credentials-*") }
func privateLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
}

func commitPrivate(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	// Persist the replacement directory entry after syncing the file contents.
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
