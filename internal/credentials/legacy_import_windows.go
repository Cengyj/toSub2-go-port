//go:build windows

package credentials

import "os"

func openLegacyImportRoot(string) (*os.File, error)           { return nil, ErrUnavailable }
func openLegacyImportFile(*os.File, string) (*os.File, error) { return nil, ErrUnavailable }
