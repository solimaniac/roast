package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WithLock runs fn with an exclusive file lock across Roast processes.
// The timeout bounds lock acquisition.
func WithLock(ctx context.Context, dir, name string, fn func() error) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	// Kernel-owned advisory locks are released even if Roast is killed. Never
	// unlink this file: doing so would allow separate processes to lock two inodes.
	lock := filepath.Join(dir, name+".lock")
	if st, err := os.Lstat(lock); err == nil {
		if err := checkPrivate(lock, st, false); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := privateLock(lock)
	if err != nil {
		return err
	}
	defer f.Close()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if err := waitCtx.Err(); err != nil {
			return fmt.Errorf("waiting for ChatGPT lock: %w", err)
		}
		ok, err := tryLock(f)
		if err != nil {
			return err
		}
		if ok {
			break
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("waiting for ChatGPT lock: %w", waitCtx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}

	defer unlockFile(f)
	return fn()
}
