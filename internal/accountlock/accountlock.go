// Package accountlock serializes account operations across local processes.
// OS handle locks are released after a crash; lock files must never be unlinked
// while another process could still hold an open handle to their inode.
package accountlock

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"tosub2-go-port/internal/privatefile"
)

var ErrBusy = errors.New("ACCOUNT_OPERATION_BUSY: 同账号已有另一个本机进程正在操作，请等待该操作结束后恢复")

// Acquire uses a user-wide namespace, independent of the selected output root.
// The origin distinguishes independent services, including local test servers.
func Acquire(origin, email string) (func() error, error) {
	identity, err := accountIdentity(origin, email)
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, errors.New("ACCOUNT_LOCK_UNAVAILABLE: 无法取得本机账号互斥锁目录")
	}
	return acquire(filepath.Join(cache, "toSub2", "account-locks"), identity)
}

func accountIdentity(origin, email string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || strings.TrimSpace(email) == "" {
		return "", errors.New("ACCOUNT_LOCK_INVALID: account and valid service origin are required")
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host + "|" + strings.ToLower(strings.TrimSpace(email)), nil
}

func acquire(dir, identity string) (func() error, error) {
	if err := privatefile.EnsureDir(dir); err != nil {
		return nil, errors.New("ACCOUNT_LOCK_UNAVAILABLE: 无法保护本机账号互斥锁目录")
	}
	path := filepath.Join(dir, fmt.Sprintf("%x.lock", sha256.Sum256([]byte(identity))))
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		file, err := privatefile.CreateTemp(dir, ".account-lock-")
		if err != nil {
			return nil, err
		}
		temporary := file.Name()
		if err := file.Close(); err != nil {
			_ = os.Remove(temporary)
			return nil, err
		}
		// No-replace publication retains the original inode for all contenders.
		err = os.Link(temporary, path)
		_ = os.Remove(temporary)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, errors.New("ACCOUNT_LOCK_INVALID: lock path must be a regular private file")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || privatefile.Verify(file) != nil {
		_ = file.Close()
		return nil, errors.New("ACCOUNT_LOCK_INVALID: lock identity or private permissions changed")
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	var once sync.Once
	var closeErr error
	return func() error { once.Do(func() { closeErr = file.Close() }); return closeErr }, nil
}
