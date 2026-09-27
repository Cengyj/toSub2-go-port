//go:build !windows

package credentials

import "errors"

func protectDPAPI([]byte) ([]byte, error) {
	return nil, errors.New("DPAPI is only available on Windows")
}
func unprotectDPAPI([]byte) ([]byte, error) {
	return nil, errors.New("DPAPI is only available on Windows")
}
