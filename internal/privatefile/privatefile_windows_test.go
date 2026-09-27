//go:build windows

package privatefile

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// These tests execute actual Windows ACL APIs. Cross-compiling this file does
// not execute these checks; they must be run on Windows (NTFS/ReFS or an ACL-
// capable share). Failure to establish a real DACL is a test failure, not a skip.
func broadWindowsDACL(t *testing.T, path string, directory bool) {
	t.Helper()
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;" + inheritance + ";FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}
func TestWindowsPrivateFileDoesNotInheritEveryoneAccess(t *testing.T) {
	dir := t.TempDir()
	broadWindowsDACL(t, dir, true)
	before := windowsDirectorySecurity(t, dir)
	if err := EnsureParentDir(dir); err != nil {
		t.Fatal(err)
	}
	f, err := CreateTemp(dir, ".private-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = Verify(f); err != nil {
		t.Fatal(err)
	}
	info, _ := f.Stat()
	if info.Size() != 0 {
		t.Fatal("privacy check occurred after writing")
	}
	if _, err = f.Write([]byte("synthetic-file")); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "existing.json")
	if err = os.WriteFile(target, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	broadWindowsDACL(t, target, false)
	if err = os.Rename(f.Name(), target); err != nil {
		t.Fatal(err)
	}
	result, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	if err = Verify(result); err != nil {
		t.Fatal("rename inherited broad destination permissions:", err)
	}
	if after := windowsDirectorySecurity(t, dir); after != before {
		t.Fatal("export creation changed the shared parent's owner or DACL")
	}
}

func windowsDirectorySecurity(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

func TestWindowsNewExportParentsPrivateWithoutChangingAncestor(t *testing.T) {
	parent := t.TempDir()
	broadWindowsDACL(t, parent, true)
	before := windowsDirectorySecurity(t, parent)
	dir := filepath.Join(parent, "new", "nested")
	if err := EnsureParentDir(dir); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{filepath.Dir(dir), dir} {
		name, err := windows.UTF16PtrFromString(target)
		if err != nil {
			t.Fatal(err)
		}
		h, err := windows.CreateFile(name, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, sid, err := currentSecurity(true)
		if err == nil {
			err = verifyHandle(h, sid, true)
		}
		_ = windows.CloseHandle(h)
		if err != nil {
			t.Fatal(err)
		}
	}
	if after := windowsDirectorySecurity(t, parent); after != before {
		t.Fatal("new export parents changed existing ancestor permissions")
	}
}
func TestWindowsPrivateDirectoryRemovesBroadInheritedAccess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	broadWindowsDACL(t, dir, true)
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	_, sid, err := currentSecurity(true)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyHandle(h, sid, true); err != nil {
		t.Fatal(err)
	}
}
func TestWindowsPrivateVerifierRejectsPermissiveAndNullDACL(t *testing.T) {
	for _, nullACL := range []bool{false, true} {
		t.Run(map[bool]string{false: "everyone", true: "null-dacl"}[nullACL], func(t *testing.T) {
			f, err := CreateTemp(t.TempDir(), "fixture-")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if nullACL {
				err = windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil)
			} else {
				broadWindowsDACL(t, f.Name(), false)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = Verify(f); err == nil {
				t.Fatal("unsafe DACL accepted")
			}
		})
	}
}
