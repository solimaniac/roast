package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func setTestDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsPrivateStorageAndReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tokens.json")
	for _, token := range []string{"first", "rotated"} {
		if err := WriteJSON(path, map[string]string{"token": token}); err != nil {
			t.Fatal(err)
		}
		var saved map[string]string
		if err := ReadJSON(path, &saved); err != nil || saved["token"] != token {
			t.Fatalf("replacement not readable: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary credential files leaked: %v", err)
	}
	// Verify creation-time protection, including files before they hold data.
	f, err := privateTemp(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	for _, path := range []string{dir, f.Name(), path} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatalf("%s inherits unwanted permissions", path)
		}
		if err := checkPrivateDescriptor(sd); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWindowsRejectsInsecureStorage(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range []string{"directory", "credential", "lock"} {
		t.Run(object, func(t *testing.T) {
			dir := testStorageDir(t)
			path := dir
			if object != "directory" {
				path = filepath.Join(dir, "session.lock")
				if err := WriteJSON(path, map[string]string{"token": "synthetic"}); err != nil {
					t.Fatal(err)
				}
			}
			setTestDACL(t, path, "D:P(A;OICI;FA;;;"+u.User.Sid.String()+")(A;OICI;GR;;;WD)")
			var err error
			switch object {
			case "credential":
				var v any
				err = ReadJSON(path, &v)
			case "lock":
				err = WithLock(context.Background(), dir, "session", func() error {
					t.Fatal("entered lock with public ACL")
					return nil
				})
			case "directory":
				err = EnsureDir(path)
			}
			if !errors.Is(err, errPrivateACL) {
				t.Fatalf("accepted public %s: %v", object, err)
			}
		})
	}
}

func TestWindowsRejectsNullOrForeignDACL(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, sddl string }{
		{"inherited DACL", "O:" + u.User.Sid.String() + "D:(A;;FA;;;" + u.User.Sid.String() + ")"},
		{"null DACL", "O:" + u.User.Sid.String() + "D:NO_ACCESS_CONTROL"},
		{"public grant", "O:" + u.User.Sid.String() + "D:P(A;;FA;;;WD)"},
		{"foreign owner", "O:BAD:P(A;;FA;;;" + u.User.Sid.String() + ")"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkPrivateDescriptor(sd); !errors.Is(err, errPrivateACL) {
				t.Fatalf("checkPrivateDescriptor error = %v, want private ACL rejection", err)
			}
		})
	}

}

func TestWindowsReplacementRetriesSharingViolation(t *testing.T) {
	dir := testStorageDir(t)
	path := filepath.Join(dir, "accounts.json")
	if err := WriteJSON(path, "old"); err != nil {
		t.Fatal(err)
	}
	// Match the os.ReadFile handle used by New's unlocked loadIndex.
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() { defer close(stopped); done <- WriteJSON(path, "new") }()
	t.Cleanup(func() {
		_ = reader.Close()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("replacement goroutine did not stop during cleanup")
		}
	})
	select {
	case err := <-done:
		t.Fatalf("replacement returned before the reader closed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replacement did not finish after the reader closed")
	}
	var value string
	if err := ReadJSON(path, &value); err != nil || value != "new" {
		t.Fatalf("replacement not persisted: value=%q error=%v", value, err)
	}
}

func TestWindowsReplacementSharingTimeoutPreservesCredentials(t *testing.T) {
	dir := testStorageDir(t)
	path := filepath.Join(dir, "tokens.json")
	if err := WriteJSON(path, "old"); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	start := time.Now()
	if err := WriteJSON(path, "new"); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("expected bounded sharing violation: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second || elapsed > 3*time.Second {
		t.Fatalf("sharing retry duration = %v, want the one-second retry window within a three-second bound", elapsed)
	}
	var value string
	if err := ReadJSON(path, &value); err != nil || value != "old" {
		t.Fatalf("failed replacement lost the old credential: %q, %v", value, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed replacement leaked temporary credentials: %v", err)
	}
}
