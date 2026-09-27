//go:build windows

package privatefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Only the account running this process is granted access. No Everyone, Users,
// Administrators or inherited ACE is retained. SYSTEM is included only when it
// is itself the process identity (for example a LocalSystem service).
func currentSecurity(directory bool) (*windows.SECURITY_DESCRIPTOR, *windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	sid := user.User.Sid
	sd, err := windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;" + inheritance + ";FA;;;" + sid.String() + ")")
	return sd, sid, err
}
func securityAttributes(sd *windows.SECURITY_DESCRIPTOR) *windows.SecurityAttributes {
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
}
func createPrivate(path string) (*os.File, error) {
	sd, sid, err := currentSecurity(false)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.WRITE_DAC|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, securityAttributes(sd), windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		_ = os.Remove(path)
		return nil, errors.New("invalid private file handle")
	}
	// Repeat restriction on the actual open handle, then verify it before the
	// caller can write. Unsupported/nonpersistent ACL filesystems fail closed.
	if err = restrictHandle(handle, sd, sid, false); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return file, nil
}
func mkdirAllPrivate(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("private directory must be a real directory")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return err
	}
	if err = mkdirAllPrivate(parent); err != nil {
		return err
	}
	sd, _, err := currentSecurity(true)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(name, securityAttributes(sd))
	runtime.KeepAlive(sd)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		// Another process created it after the initial Lstat. It is now an
		// existing parent; a generic export must not take over its ACL.
		return validateDirectory(path)
	}
	if err != nil {
		return err
	}
	return restrictDirectory(path)
}
func validateDirectory(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err = checkFileType(handle, true); err != nil {
		return err
	}
	return checkPersistentACLs(handle)
}
func restrictDirectory(path string) error {
	sd, sid, err := currentSecurity(true)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err = checkFileType(handle, true); err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	err = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, sid, nil, dacl, nil)
	runtime.KeepAlive(sd)
	if err != nil {
		return err
	}
	return verifyHandle(handle, sid, true)
}
func restrictHandle(handle windows.Handle, sd *windows.SECURITY_DESCRIPTOR, sid *windows.SID, directory bool) error {
	if err := checkFileType(handle, directory); err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	err = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	runtime.KeepAlive(sd)
	if err != nil {
		return err
	}
	return verifyHandle(handle, sid, directory)
}
func checkFileType(handle windows.Handle, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("private paths must not be reparse points")
	}
	if (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return errors.New("unexpected private file type")
	}
	return nil
}
func verifyHandle(handle windows.Handle, sid *windows.SID, directory bool) error {
	if err := checkFileType(handle, directory); err != nil {
		return err
	}
	if err := checkPersistentACLs(handle); err != nil {
		return err
	}
	return verifyDACL(handle, sid, directory)
}
func checkPersistentACLs(handle windows.Handle) error {
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return fmt.Errorf("cannot verify persistent ACL support: %w", err)
	}
	if flags&windows.FILE_PERSISTENT_ACLS == 0 {
		return errors.New("filesystem does not support persistent Windows ACLs")
	}
	return nil
}
func verifyDACL(handle windows.Handle, sid *windows.SID, directory bool) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("private DACL still inherits permissions")
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(sid) {
		return errors.New("private file owner is not the current user")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		return errors.New("private DACL must grant only the current user")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err = windows.GetAce(dacl, 0, &ace); err != nil {
		return err
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
		return errors.New("unexpected private access control entry")
	}
	allowed := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	const fileFullControl windows.ACCESS_MASK = 0x001f01ff
	if !allowed.Equals(sid) || ace.Mask&fileFullControl != fileFullControl {
		return errors.New("private access is not restricted to the current user")
	}
	expectedFlags := uint8(0)
	if directory {
		expectedFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	if ace.Header.AceFlags != expectedFlags {
		return errors.New("private access inheritance flags are invalid")
	}
	return nil
}
func verifyPrivate(file *os.File) error {
	_, sid, err := currentSecurity(false)
	if err != nil {
		return err
	}
	return verifyHandle(windows.Handle(file.Fd()), sid, false)
}
