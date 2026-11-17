package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var errPrivateACL = errors.New("ChatGPT storage must be owned by and accessible only to the current Windows user")

func privateSecurity() (*windows.SecurityAttributes, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	// Protect against inherited permissions, including at creation time. Child
	// files inherit only this user's access; do not grant Users or Everyone.
	sid := u.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

func checkPrivate(path string, st os.FileInfo, directory bool) error {
	if st.Mode()&os.ModeSymlink != 0 || (directory && !st.IsDir()) || (!directory && !st.Mode().IsRegular()) {
		return errPrivateACL
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	// Reject junctions and other reparse points as well as ordinary symlinks.
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory ||
		(!directory && info.NumberOfLinks != 1) {
		return errPrivateACL
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return checkPrivateDescriptor(sd)
}

func checkPrivateDescriptor(sd *windows.SECURITY_DESCRIPTOR) error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(u.User.Sid) {
		return errPrivateACL
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errPrivateACL
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return errPrivateACL
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(u.User.Sid) {
			return errPrivateACL
		}
	}
	return nil
}

func EnsureDir(dir string) error {
	// Only the final directory stores credentials; create it with its ACL in
	// place, rather than briefly exposing it through inherited permissions.
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	sa, err := privateSecurity()
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(p, sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	return checkPrivate(dir, st, true)
}

func createPrivate(path string, disposition uint32) (*os.File, error) {
	sa, err := privateSecurity()
	if err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, sa, disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	st, err := f.Stat()
	if err == nil {
		err = checkPrivate(path, st, false)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func privateLock(path string) (*os.File, error) { return createPrivate(path, windows.OPEN_ALWAYS) }

func privateTemp(dir string) (*os.File, error) {
	for i := 0; i < 10; i++ {
		var suffix [32]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, err
		}
		f, err := createPrivate(filepath.Join(dir, ".credentials-"+hex.EncodeToString(suffix[:])), windows.CREATE_NEW)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		return f, err
	}
	return nil, errors.New("cannot create private ChatGPT credential file")
}

func commitPrivate(from, to string) error {
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	dst, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	// Windows cannot fsync directories. Flush the file before this same-volume
	// replacement and request write-through for the move instead.
	// A concurrent New can briefly hold os.ReadFile's non-delete-sharing
	// handle on accounts.json without taking the session lock. Retry only
	// that transient failure; never remove the old credential before replacing.
	deadline := time.Now().Add(time.Second)
	for {
		err := windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		// MoveFileEx can also report ACCESS_DENIED for an open destination
		// lacking delete sharing. Genuine access failures remain bounded.
		retryable := errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
		if !retryable || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
