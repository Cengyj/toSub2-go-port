//go:build windows

package browser

import (
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Must execute on Windows. Cross-compilation validates the API surface only.
func TestChromiumProfileDirectoryHasCurrentUserDACL(t *testing.T) {
	parent := t.TempDir()
	broad, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := broad.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	readSecurity := func(path string) *windows.SECURITY_DESCRIPTOR {
		t.Helper()
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		return sd
	}
	before := readSecurity(parent).String()
	t.Setenv("TMP", parent)
	t.Setenv("TEMP", parent)
	dir, err := makeProfileDirectory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removeProfileDirectory(dir); err != nil {
			t.Error(err)
		}
	})
	if !strings.EqualFold(filepath.Clean(filepath.Dir(dir)), filepath.Clean(parent)) {
		t.Fatal("profile created outside configured TEMP")
	}
	sd := readSecurity(dir)
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("profile DACL is not protected")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		t.Fatal("profile owner differs from current user")
	}
	dacl, _, err = sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatal("profile DACL grants identities other than current user")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err = windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	allowed := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !allowed.Equals(user.User.Sid) || ace.Header.AceFlags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE || ace.Mask&0x001f01ff != 0x001f01ff {
		t.Fatal("profile DACL permissions or inheritance are unsafe")
	}
	if readSecurity(parent).String() != before {
		t.Fatal("browser creation altered TEMP parent permissions")
	}
}
