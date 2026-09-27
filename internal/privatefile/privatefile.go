// Package privatefile creates private directories and temporary files before
// callers write secrets. Windows uses a protected current-user DACL; POSIX uses
// 0700/0600. A permission failure never falls back to an ordinary writable file.
package privatefile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrPermissions = errors.New("private file permissions could not be established")

// EnsureDir creates missing parents privately and restricts the requested final
// application-owned directory. Existing ancestors are never changed. Callers
// writing to an arbitrary export path must use EnsureParentDir instead.
func EnsureDir(path string) error {
	absolute, err := directoryPath(path)
	if err != nil {
		return err
	}
	if err = mkdirAllPrivate(absolute); err != nil {
		return fmt.Errorf("%w: create directory: %w", ErrPermissions, err)
	}
	if err = restrictDirectory(absolute); err != nil {
		return fmt.Errorf("%w: restrict directory: %w", ErrPermissions, err)
	}
	return nil
}

// EnsureParentDir creates missing export directories privately without changing
// an existing directory's owner, mode or ACL. File privacy is established by
// CreateTemp itself, even inside a shared parent such as Downloads. Final
// symlinks/reparse points and filesystem/volume roots are still rejected.
func EnsureParentDir(path string) error {
	absolute, err := directoryPath(path)
	if err != nil {
		return err
	}
	if err = mkdirAllPrivate(absolute); err != nil {
		return fmt.Errorf("%w: create export directory: %w", ErrPermissions, err)
	}
	if err = validateDirectory(absolute); err != nil {
		return fmt.Errorf("%w: validate export directory: %w", ErrPermissions, err)
	}
	return nil
}

func directoryPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: invalid directory path", ErrPermissions)
	}
	if filepath.Dir(absolute) == absolute {
		return "", fmt.Errorf("%w: refusing filesystem root", ErrPermissions)
	}
	return absolute, nil
}

// CreateTemp has os.CreateTemp's pattern convention. The Windows descriptor is
// applied by CreateFile itself (before the file can contain data), then read back
// and verified through its handle. Caller owns closing/removing the returned file.
func CreateTemp(dir, pattern string) (*os.File, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	if strings.ContainsAny(pattern, "/\\") {
		return nil, errors.New("temporary filename pattern must not contain a separator")
	}
	var err error
	dir, err = directoryPath(dir)
	if err != nil {
		return nil, err
	}
	if err = validateDirectory(dir); err != nil {
		return nil, fmt.Errorf("%w: validate temporary directory: %w", ErrPermissions, err)
	}
	prefix, suffix := pattern, ""
	if i := strings.LastIndexByte(pattern, '*'); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	for attempt := 0; attempt < 100; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, fmt.Errorf("%w: random temporary filename unavailable", ErrPermissions)
		}
		path := filepath.Join(dir, prefix+hex.EncodeToString(random[:])+suffix)
		file, err := createPrivate(path)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: create temporary file: %w", ErrPermissions, err)
		}
		return file, nil
	}
	return nil, fmt.Errorf("%w: temporary filename collisions", ErrPermissions)
}

// Verify validates the permissions on the open file, including Windows ACL
// persistence and owner identity. It does not inspect or reveal file contents.
func Verify(file *os.File) error {
	if file == nil {
		return fmt.Errorf("%w: nil file", ErrPermissions)
	}
	if err := verifyPrivate(file); err != nil {
		return fmt.Errorf("%w: verify file: %w", ErrPermissions, err)
	}
	return nil
}
