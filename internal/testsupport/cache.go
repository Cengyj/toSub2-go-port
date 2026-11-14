// Package testsupport contains test-process setup. Production packages must not
// import it: account locks always retain their normal user-wide namespace.
package testsupport

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// CacheNamespaceEnv is inherited by fixture subprocesses so they contend for
// exactly the same account locks as their parent. Independent test invocations
// create different namespaces without requiring a caller-supplied environment.
const CacheNamespaceEnv = "TOSUB2_TEST_CACHE_NAMESPACE"

// Run isolates os.UserCacheDir for suites that exercise default account locks.
// It is called only from TestMain, before tests or worker goroutines start.
// Native credentials integration deliberately does not use this setup.
func Run(m *testing.M) (code int) {
	root := os.Getenv(CacheNamespaceEnv)
	owned := root == ""
	if owned {
		var err error
		root, err = os.MkdirTemp("", "tosub2-test-cache-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "create test cache namespace:", err)
			return 1
		}
		defer func() {
			if err := os.RemoveAll(root); err != nil {
				fmt.Fprintln(os.Stderr, "remove test cache namespace:", err)
				code = 1
			}
		}()
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		fmt.Fprintln(os.Stderr, "test cache namespace is not an existing real directory")
		return 1
	}
	key, value := "XDG_CACHE_HOME", filepath.Join(root, "cache")
	switch runtime.GOOS {
	case "darwin":
		// Darwin's os.UserCacheDir uses HOME/Library/Caches, not XDG.
		// This is confined to these test processes; the credentials suite keeps
		// the real user's HOME and performs its own native Keychain isolation.
		key, value = "HOME", filepath.Join(root, "home")
	case "windows":
		key = "LOCALAPPDATA"
	}
	if err := os.MkdirAll(value, 0700); err != nil {
		fmt.Fprintln(os.Stderr, "prepare test cache namespace:", err)
		return 1
	}
	if err := os.Setenv(key, value); err != nil {
		fmt.Fprintln(os.Stderr, "select test cache namespace:", err)
		return 1
	}
	if err := os.Setenv(CacheNamespaceEnv, root); err != nil {
		fmt.Fprintln(os.Stderr, "share test cache namespace:", err)
		return 1
	}
	return m.Run()
}
