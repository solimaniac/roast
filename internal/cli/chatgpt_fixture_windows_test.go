package cli

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// Match the store directory's explicit owner and protected DACL. Elevated
// Windows processes can otherwise create files owned by Administrators.
func protectAccountFixture(t *testing.T, path string) {
	t.Helper()
	const security = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION
	sd, err := windows.GetNamedSecurityInfo(filepath.Dir(path), windows.SE_FILE_OBJECT, security)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		security|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}
