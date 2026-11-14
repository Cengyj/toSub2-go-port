//go:build windows

package accountquality

import (
	"golang.org/x/sys/windows"
	"os"
	"unsafe"
)

// Read-only equivalent of privatefile's directory DACL contract. Never repair
// an untrusted policy directory while merely deciding whether upload is allowed.
func verifyDirectory(path string, expected os.FileInfo) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ErrSettingsUnavailable
	}
	h, err := windows.CreateFile(name, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return ErrSettingsUnavailable
	}
	file := os.NewFile(uintptr(h), path)
	if file == nil {
		_ = windows.CloseHandle(h)
		return ErrSettingsUnavailable
	}
	defer file.Close()
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &info) != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return ErrSettingsUnavailable
	}
	current, err := file.Stat()
	if err != nil || !os.SameFile(expected, current) {
		return ErrSettingsUnavailable
	}
	var flags uint32
	if windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, &flags, nil, 0) != nil || flags&windows.FILE_PERSISTENT_ACLS == 0 {
		return ErrSettingsUnavailable
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ErrSettingsUnavailable
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return ErrSettingsUnavailable
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return ErrSettingsUnavailable
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return ErrSettingsUnavailable
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		return ErrSettingsUnavailable
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if windows.GetAce(dacl, 0, &ace) != nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
		return ErrSettingsUnavailable
	}
	allowed := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	const fullControl windows.ACCESS_MASK = 0x001f01ff
	if !allowed.Equals(user.User.Sid) || ace.Mask&fullControl != fullControl {
		return ErrSettingsUnavailable
	}
	return nil
}
