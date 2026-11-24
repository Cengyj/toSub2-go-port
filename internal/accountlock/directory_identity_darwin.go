package accountlock

import (
	"strings"

	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// _PC_CASE_SENSITIVE is a volume property on Darwin. Query the existing
// directory instead of guessing from the OS or its current directory entries;
// the same rule must apply before and after creating a missing descendant.
const directoryCaseSensitive = 11

func directoryIdentityComponent(parent, name string) (string, error) {
	sensitive, err := unix.Pathconf(parent, directoryCaseSensitive)
	if err != nil {
		return "", errDirectoryIdentity
	}
	// APFS treats canonical Unicode forms as the same name while preserving
	// the spelling used at creation. Normalize both existing and missing
	// components so a decomposed first creation cannot change a held key.
	// Do not infer this rule for an unrelated or network filesystem.
	var volume unix.Statfs_t
	if err := unix.Statfs(parent, &volume); err != nil {
		return "", errDirectoryIdentity
	}
	apfs := unix.ByteSliceToString(volume.Fstypename[:]) == "apfs"
	if apfs {
		name = norm.NFC.String(name)
	}
	switch sensitive {
	case 0:
		if apfs {
			// APFS also equates full case folds such as SS/ß and Σ/ς.
			// Simple lowercase misses those aliases. Folding can introduce
			// combining characters, so normalize the folded form as well.
			return norm.NFC.String(cases.Fold().String(name)), nil
		}
		return strings.ToLower(name), nil
	case 1:
		return name, nil
	default:
		return "", errDirectoryIdentity
	}
}
