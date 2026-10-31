package application

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

func writePrivateGrantFixture(t *testing.T, path, document string) {
	t.Helper()
	// Seed the platform's private mode/DACL before replacing the contents.
	// Tests can then retain exact numbers or deliberately malformed raw JSON.
	if err := protocol.WritePrivateJSON(path, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSavedAuthorizationUploadRejectsUnsafeOrPendingGrant(t *testing.T) {
	for _, scenario := range []string{"valid", "root-symlink", "directory-symlink", "permissions", "pending-refresh", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			var requests atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"data":{"items":[],"pages":1,"total":0}}`))
					return
				}
				_, _ = w.Write([]byte(`{"created":1}`))
			}))
			defer remote.Close()
			disk, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager := jobs.NewManager(t.Context(), 1, disk, nil)
			manager.SetCredentialStore(credentials.NewMemoryStore())
			t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
			job, err := manager.Submit(jobs.Spec{ID: "upload-safety", Email: "upload@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			job, err = manager.QueueOperation(job.ID, "relogin", map[string]any{"uploadAfterRelogin": true, "sub2apiConfig": map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-admin"}})
			if err != nil {
				t.Fatal(err)
			}
			root := disk.Root
			directory := filepath.Join(root, job.ID)
			path := filepath.Join(directory, "sub2api-import-oauth.json")
			writePrivateGrantFixture(t, path, `{"type":"sub2api-data","accounts":[{"platform":"openai","type":"oauth","credentials":{"email":"upload@example.test","access_token":"synthetic-access"}}]}`)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "root-symlink":
				root = filepath.Join(t.TempDir(), "linked-root")
				err = os.Symlink(disk.Root, root)
			case "directory-symlink":
				moved := filepath.Join(disk.Root, "moved-private")
				if err := os.Rename(directory, moved); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(moved, directory)
			case "permissions":
				if runtime.GOOS == "windows" {
					t.Skip("POSIX mode regression; Windows DACL validation is covered by privatefile tests")
				}
				err = os.Chmod(path, 0644)
			case "pending-refresh":
				err = protocol.WritePrivateJSON(filepath.Join(directory, ".sub2api-import-oauth.json.refresh-grant-recovery.json"), map[string]any{"synthetic": true})
			case "cancelled":
				cancel()
			}
			if err != nil {
				if runtime.GOOS == "windows" && (scenario == "root-symlink" || scenario == "directory-symlink") {
					t.Skip("Windows symlink fixture requires host support")
				}
				t.Fatal(err)
			}
			err = UploadSavedAuthorization(ctx, root, manager, job, nil)
			if scenario == "valid" {
				if err != nil || requests.Load() == 0 {
					t.Fatal("private committed grant could not upload", err)
				}
				return
			}
			var outcome interface{ UploadOutcomeUncertain() bool }
			if err == nil || requests.Load() != 0 || !errors.As(err, &outcome) || outcome.UploadOutcomeUncertain() {
				t.Fatalf("unsafe or pending grant reached upstream: err=%v requests=%d", err, requests.Load())
			}
			if scenario == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("upload lost cancellation cause", err)
			}
		})
	}
}
