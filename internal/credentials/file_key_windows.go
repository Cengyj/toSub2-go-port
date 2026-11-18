//go:build windows

package credentials

import "context"

func readFileKeyPrivate(string, int64) ([]byte, error) { return nil, ErrUnavailable }
func withFileKeyRootLock(context.Context, string, func() error) error {
	return ErrUnavailable
}
func createFileKey(string) error { return ErrUnavailable }
