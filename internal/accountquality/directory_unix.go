//go:build !windows

package accountquality

import "os"

func verifyDirectory(path string, expected os.FileInfo) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.IsDir() || opened.Mode().Perm() != 0700 || !os.SameFile(expected, opened) {
		return ErrSettingsUnavailable
	}
	return nil
}
