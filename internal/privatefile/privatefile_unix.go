//go:build !windows

package privatefile

import (
	"errors"
	"os"
)

func mkdirAllPrivate(path string) error { return os.MkdirAll(path, 0700) }
func validateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private directory must be a real directory")
	}
	return nil
}
func restrictDirectory(path string) error {
	if err := validateDirectory(path); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if err = file.Chmod(0700); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("directory mode is not 0700")
	}
	return nil
}
func createPrivate(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Chmod(0600); err == nil {
		err = verifyPrivate(file)
	}
	if err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return file, nil
}
func verifyPrivate(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("private file must be regular with mode 0600")
	}
	return nil
}
