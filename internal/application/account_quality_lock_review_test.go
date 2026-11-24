//go:build !windows

package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/jobs"
)

// Keep the manager's acquisition boundary real while placing every lock in
// a synthetic temporary directory. accountlock's own tests cover its user-wide
// namespace; these tests cover ownership through upload cleanup and cancellation.
func strictUploadTempLocker(t *testing.T) func(string, string) (func() error, error) {
	t.Helper()
	directory := t.TempDir()
	return func(origin, email string) (func() error, error) {
		u, err := url.Parse(strings.TrimSpace(origin))
		if err != nil || u.Hostname() == "" || u.User != nil || u.Scheme != "https" && u.Scheme != "http" || strings.TrimSpace(email) == "" {
			return nil, errors.New("invalid synthetic account lock identity")
		}
		host, port := strings.ToLower(u.Hostname()), u.Port()
		if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
			port = ""
		}
		if port != "" {
			host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		identity := u.Scheme + "://" + host + "|" + strings.ToLower(strings.TrimSpace(email))
		path := filepath.Join(directory, fmt.Sprintf("%x.lock", sha256.Sum256([]byte(identity))))
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = file.Close()
			if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
				return nil, accountlock.ErrBusy
			}
			return nil, err
		}
		var once sync.Once
		var closeErr error
		return func() error { once.Do(func() { closeErr = file.Close() }); return closeErr }, nil
	}
}

func TestStrictUploadReviewSessionHoldsOSLockUntilCleanup(t *testing.T) {
	for _, base := range []string{"", "https://STRICT-UPLOAD.example.test:443/settings"} {
		t.Run(base, func(t *testing.T) {
			f, controller := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
			controller.cfg.ChatGPTBase = base
			wantOrigin, peerOrigin := base, "https://strict-upload.example.test/other"
			if base == "" {
				wantOrigin, peerOrigin = "https://chatgpt.com", "https://CHATGPT.COM:443/other"
			}
			locker := strictUploadTempLocker(t)
			acquires, releases := 0, 0
			f.manager.SetAccountOperationLease(func(email string) (func() error, error) {
				acquires++
				release, err := locker(wantOrigin, email)
				if err != nil {
					return nil, err
				}
				return func() error {
					// The OS lock must close before the same-manager reservation.
					if lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID); !errors.Is(err, jobs.ErrAccountQueryBusy) {
						if lease != nil {
							lease.Release()
						}
						t.Error("query reservation released before the OS account lock", err)
					}
					releases++
					return release()
				}, nil
			})
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, gate, cleanup := controller.NewUploadSession(parent, []string{f.job.ID})
			defer cleanup()
			for attempt := 0; attempt < 2; attempt++ {
				if err := gate(ctx, f.account); err != nil {
					t.Fatal(err)
				}
			}
			if acquires != 1 {
				t.Fatal("same upload session reacquired its account lock", acquires)
			}
			probeBusy := func() {
				t.Helper()
				release, err := locker(peerOrigin, "  "+strings.ToUpper(f.job.Email)+"  ")
				if release != nil {
					_ = release()
				}
				if !errors.Is(err, accountlock.ErrBusy) {
					t.Fatal("equivalent cross-instance account acquired an active upload lock", err)
				}
			}
			probeBusy()
			cancel()
			if ctx.Err() == nil {
				t.Fatal("upload context did not observe parent cancellation")
			}
			probeBusy()
			cleanup()
			cleanup()
			if releases != 1 {
				t.Fatal("cleanup did not release the OS lock exactly once", releases)
			}
			release, err := locker(peerOrigin, f.job.Email)
			if err != nil {
				t.Fatal("cleanup leaked the OS account lock", err)
			}
			_ = release()
			lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
			if err != nil {
				t.Fatal("cleanup leaked the query reservation", err)
			}
			lease.Release()
		})
	}
}

func TestStrictUploadReviewOSLockBlocksPreflightWithoutLeakingQuery(t *testing.T) {
	f, controller := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	controller.cfg.ChatGPTBase = "https://strict-upload.example.test"
	locker := strictUploadTempLocker(t)
	f.manager.SetAccountOperationLease(func(email string) (func() error, error) { return locker(controller.cfg.ChatGPTBase, email) })
	peerRelease, err := locker("https://STRICT-UPLOAD.example.test:443/settings", strings.ToUpper(f.job.Email))
	if err != nil {
		t.Fatal(err)
	}
	defer peerRelease()
	ctx, gate, cleanup := controller.NewUploadSession(t.Context(), []string{f.job.ID})
	defer cleanup()
	if err := gate(ctx, f.account); err == nil {
		t.Fatal("upload preflight passed while another instance held the account lock")
	}
	if lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID); !errors.Is(err, jobs.ErrAccountQueryBusy) {
		if lease != nil {
			lease.Release()
		}
		t.Fatal("ordinary query bypassed competing OS account ownership", err)
	}
	if err := peerRelease(); err != nil {
		t.Fatal(err)
	}
	lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
	if err != nil {
		t.Fatal("failed OS lock acquisition leaked its query reservation", err)
	}
	lease.Release()
	if err := gate(ctx, f.account); err != nil {
		t.Fatal("upload could not retry after the competing operation released", err)
	}
}

func TestStrictUploadReviewGrantLoadFailureRetainsBothLeases(t *testing.T) {
	f, controller := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	enableQualityFixture(t, controller, nil)
	controller.cfg.ChatGPTBase = "https://strict-upload.example.test"
	locker := strictUploadTempLocker(t)
	f.manager.SetAccountOperationLease(func(email string) (func() error, error) { return locker(controller.cfg.ChatGPTBase, email) })
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	ctx, gate, cleanup := controller.NewUploadSession(t.Context(), []string{f.job.ID})
	defer cleanup()
	if err := gate(ctx, f.account); err == nil {
		t.Fatal("missing committed grant unexpectedly passed upload preflight")
	}
	if lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID); !errors.Is(err, jobs.ErrAccountQueryBusy) {
		if lease != nil {
			lease.Release()
		}
		t.Fatal("grant load failure prematurely released the query reservation", err)
	}
	if release, err := locker(controller.cfg.ChatGPTBase, f.job.Email); !errors.Is(err, accountlock.ErrBusy) {
		if release != nil {
			_ = release()
		}
		t.Fatal("grant load failure prematurely released the OS account lock", err)
	}
	cleanup()
	release, err := locker(controller.cfg.ChatGPTBase, f.job.Email)
	if err != nil {
		t.Fatal("grant failure cleanup leaked the OS account lock", err)
	}
	_ = release()
	lease, err := f.manager.ReserveAccountQuery(t.Context(), f.job.ID)
	if err != nil {
		t.Fatal("grant failure cleanup leaked the query reservation", err)
	}
	lease.Release()
}
