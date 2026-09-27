package browser

import (
	"errors"
	"fmt"
	"os"
	"time"

	"tosub2-go-port/internal/privatefile"
)

func makeProfileDirectory() (string, error) {
	dir, err := os.MkdirTemp("", "tosub2-browser-")
	if err != nil {
		return "", err
	}
	// Chrome stores cookies and session credentials here. In particular,
	// Windows MkdirTemp alone does not set a current-user-only protected DACL.
	// Restrict this newly created child before spawn, never the TEMP parent.
	if err = privatefile.EnsureDir(dir); err != nil {
		return "", errors.Join(fmt.Errorf("protect Chromium profile directory: %w", err), removeProfileDirectory(dir))
	}
	return dir, nil
}

func removeProfileDirectory(dir string) error {
	if dir == "" {
		return nil
	}
	var err error
	// Some Windows filesystem filters keep the just-exited browser's file
	// handles briefly. Retry a bounded interval, then report the failure so
	// a service never silently leaves a credential-bearing profile behind.
	for attempt := 0; attempt < 10; attempt++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		if attempt < 9 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	return fmt.Errorf("remove Chromium profile directory: %w", err)
}
