package accountlock

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var errDirectoryIdentity = errors.New("ACCOUNT_LOCK_UNAVAILABLE: cannot resolve directory identity")

// DirectoryIdentity resolves ancestor symlinks and existing directory spelling
// for a directory's lock identity.
// It also handles a missing root without creating it, so a lease taken before
// the first write keeps the same identity after directory creation. This is
// only a coordination key; callers must still enforce their file path policy.
func DirectoryIdentity(root string) (string, error) {
	if root == "" {
		return "", errDirectoryIdentity
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", errDirectoryIdentity
	}
	ancestor := abs
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			physical, identity, err := existingDirectoryIdentity(resolved)
			if err != nil {
				return "", errDirectoryIdentity
			}
			abs = identity
			for i := len(missing) - 1; i >= 0; i-- {
				component, err := directoryIdentityComponent(physical, missing[i])
				if err != nil {
					return "", errDirectoryIdentity
				}
				abs = filepath.Join(abs, component)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(ancestor) == ancestor {
			return "", errDirectoryIdentity
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = filepath.Dir(ancestor)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(abs))), nil
}

// EvalSymlinks on Unix preserves the caller's spelling on case-insensitive
// filesystems. Read the actual entry and compare file identities instead of
// assuming that lowercasing names is valid on every filesystem.
func existingDirectoryIdentity(path string) (physical, identity string, err error) {
	parent := filepath.Dir(path)
	if parent == path {
		return path, path, nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", errDirectoryIdentity
	}
	parentPath, parentIdentity, err := existingDirectoryIdentity(parent)
	if err != nil {
		return "", "", err
	}
	entries, err := os.ReadDir(parentPath)
	if err != nil {
		return "", "", errDirectoryIdentity
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate, err := entry.Info()
		if err != nil || !os.SameFile(info, candidate) {
			continue
		}
		component, err := directoryIdentityComponent(parentPath, entry.Name())
		if err != nil {
			return "", "", errDirectoryIdentity
		}
		return filepath.Join(parentPath, entry.Name()), filepath.Join(parentIdentity, component), nil
	}
	return "", "", errDirectoryIdentity
}
