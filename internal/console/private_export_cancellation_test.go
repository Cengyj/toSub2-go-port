package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestPrivateDownloadCancellationInterruptsVaultWithoutLockingManager(t *testing.T) {
	s, manager := newMonitorTestServer(t)
	manager.SetCredentialStore(credentials.NewMemoryStore())
	job := savedManualUploadFixture(t, s, manager, "cancel-private-download")
	path := "/api/jobs/" + job.ID + "/download"
	before := frontendRequest(s, http.MethodGet, path, nil, "test")
	if before.Code != http.StatusOK || before.Header().Get("Content-Disposition") == "" || !strings.Contains(before.Body.String(), "synthetic-access") {
		t.Fatal("synthetic committed authorization was not initially downloadable", before.Code)
	}
	grantPath := filepath.Join(s.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
	beforeGrant, err := os.ReadFile(grantPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeJobs := manager.List()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	vault := &privateDownloadBlockingVault{
		MemoryStore: credentials.NewMemoryStore(), entered: make(chan context.Context, 1),
		cancelled: make(chan error, 1), release: make(chan struct{}),
	}
	manager.SetCredentialStore(vault)
	ctx, cancel := context.WithCancel(t.Context())
	request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	request.Host = "127.0.0.1"
	request.Header.Set("X-Console-Token", "test")
	response := httptest.NewRecorder()
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		close(vault.release)
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("private download handler survived synthetic fixture cleanup")
		}
	})
	go func() {
		defer close(finished)
		s.Handler().ServeHTTP(response, request)
	}()
	var readContext context.Context
	select {
	case readContext = <-vault.entered:
	case <-finished:
		t.Fatal("private download did not use its context-aware credential gate")
	case <-time.After(time.Second):
		t.Fatal("private download did not reach the protected credential store")
	}
	if readContext.Err() != nil {
		t.Fatal("credential read started with an already canceled context")
	}
	listed := make(chan []*jobs.Job, 1)
	go func() { listed <- manager.List() }()
	select {
	case current := <-listed:
		if !reflect.DeepEqual(beforeJobs, current) {
			t.Fatal("private download changed account metadata while reading credentials")
		}
	case <-time.After(time.Second):
		t.Fatal("protected credential read held the manager mutex and blocked account listing")
	}
	cancel()
	select {
	case err := <-vault.cancelled:
		if !errors.Is(err, context.Canceled) || !errors.Is(readContext.Err(), context.Canceled) {
			t.Fatal("request cancellation did not reach the credential store", err)
		}
	case <-time.After(time.Second):
		t.Fatal("credential lookup ignored private download cancellation")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("private download did not exit after its credential read was canceled")
	}
	if response.Code != http.StatusConflict || response.Header().Get("Content-Disposition") != "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("canceled private download retained success, attachment or cacheable response headers", response.Code)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Error != context.Canceled.Error() {
		t.Fatal("canceled private download did not return the safe cancellation outcome")
	}
	for _, secret := range []string{"synthetic-access", "synthetic-private-vault-detail", job.Email, "sub2api-data"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("canceled private download exposed authorization, identity or storage diagnostics")
		}
	}
	if vault.reads.Load() != 1 || vault.legacyReads.Load() != 0 {
		t.Fatal("private download retried or bypassed the context-aware credential read")
	}
	afterGrant, err := os.ReadFile(grantPath)
	if err != nil || !bytes.Equal(beforeGrant, afterGrant) || !reflect.DeepEqual(beforeJobs, manager.List()) {
		t.Fatal("canceled private download changed the committed grant or operation history", err)
	}
}

type privateDownloadBlockingVault struct {
	*credentials.MemoryStore
	entered     chan context.Context
	cancelled   chan error
	release     chan struct{}
	reads       atomic.Int32
	legacyReads atomic.Int32
}

func (v *privateDownloadBlockingVault) Load(string) (credentials.Credentials, error) {
	v.legacyReads.Add(1)
	return credentials.Credentials{}, errors.New("synthetic-private-vault-detail")
}

func (v *privateDownloadBlockingVault) LoadContext(ctx context.Context, _ string) (credentials.Credentials, error) {
	v.reads.Add(1)
	select {
	case v.entered <- ctx:
	default:
	}
	select {
	case <-ctx.Done():
		select {
		case v.cancelled <- ctx.Err():
		default:
		}
		return credentials.Credentials{}, errors.Join(ctx.Err(), errors.New("synthetic-private-vault-detail"))
	case <-v.release:
		return credentials.Credentials{}, errors.New("synthetic-private-vault-detail")
	}
}

func (v *privateDownloadBlockingVault) SaveContext(ctx context.Context, email string, value credentials.Credentials) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return v.MemoryStore.Save(email, value)
}

func (v *privateDownloadBlockingVault) DeleteContext(ctx context.Context, email string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return v.MemoryStore.Delete(email)
}
