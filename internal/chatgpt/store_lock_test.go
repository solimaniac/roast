package chatgpt

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreAndAuthorizationLocksRejectSymlinks(t *testing.T) {
	hash := sha256.Sum256([]byte("saved-account"))
	for _, name := range []string{"session", "authorization-new", "authorization-" + hex.EncodeToString(hash[:])} {
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			target := filepath.Join(c.dir, "target")
			if err := os.WriteFile(target, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(c.dir, name+".lock")); err != nil {
				t.Fatal(err)
			}
			entered := false
			err := c.lockedFile(context.Background(), name, func() error { entered = true; return nil })
			if err == nil || entered {
				t.Fatal("accepted a symlink as a lock file")
			}
		})
	}
}

func TestStoreLockReleasedOnProcessDeath(t *testing.T) {
	// The helper can only see the synthetic test store supplied by this test.
	if os.Getenv("ROAST_TEST_LOCK_HELPER") == "1" {
		c := &Client{dir: os.Getenv("ROAST_TEST_LOCK_DIR")}
		err := c.lockedFile(context.Background(), os.Getenv("ROAST_TEST_LOCK_NAME"), func() error {
			fmt.Println("locked")
			time.Sleep(time.Minute)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, name := range []string{"session", "authorization-new"} {
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestStoreLockReleasedOnProcessDeath$")
			cmd.Env = append(os.Environ(), "ROAST_TEST_LOCK_HELPER=1", "ROAST_TEST_LOCK_DIR="+c.dir, "ROAST_TEST_LOCK_NAME="+name)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			ready := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				ready <- scanner.Scan() && scanner.Text() == "locked"
			}()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("helper failed to acquire lock")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("helper did not report lock acquisition")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			err = c.lockedFile(ctx, name, func() error { return nil })
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("child process did not exclude parent: %v", err)
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			waited = true
			ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err = c.lockedFile(ctx, name, func() error { return nil }); err != nil {
				t.Fatalf("kernel lock survived process death: %v", err)
			}
		})
	}
}
