package chatgpt

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

func TestPrivateFilesAndLockCancellation(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	if runtime.GOOS != "windows" {
		if e := os.Chmod(c.path(c.selected), 0644); e != nil {
			t.Fatal(e)
		}
		if _, e := c.Token(context.Background()); e == nil {
			t.Fatal("accepted public credential")
		}
		if err := os.Chmod(c.path(c.selected), 0600); err != nil {
			t.Fatal(err)
		}
	}

	err := storage.WithLock(context.Background(), c.dir, "session", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if _, err := c.Token(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock ignored cancellation: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLockReleaseAndStablePreparedHost(t *testing.T) {
	c := testClient(t)
	id, dir, err := c.PrepareHost(context.Background())
	if err != nil || id == "" || dir != c.dir {
		t.Fatal("host not prepared")
	}
	second, _, err := c.PrepareHost(context.Background())
	if err != nil || id != second {
		t.Fatal("host identity changed")
	}
	i, err := c.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(i.Clients) != 0 || i.Active != "" {
		t.Fatal("prepare-host created registration")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.locked(ctx, func() error { return nil }); err != nil {
		t.Fatalf("session lock remained held: %v", err)
	}
}
