//go:build windows

package privatefile

import "golang.org/x/sys/windows"

func replaceDurable(from, to string) error {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// Windows does not provide a universally supported directory-fsync equivalent.
// Request a real directory flush; unsupported filesystems or access modes are
// surfaced as ErrDurabilityUncertain after replacement/removal, never ignored.
// MOVEFILE_WRITE_THROUGH alone is not a guarantee for every same-volume rename.
func syncDirectory(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err = checkFileType(handle, true); err != nil {
		return err
	}
	return windows.FlushFileBuffers(handle)
}
