package console

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

const recoverySidecar = ".sub2api-import-oauth.json.refresh-grant-recovery.json"
const recoveryCanary = "Synthetic-private-rotated-grant-do-not-serve"

type unavailableRecoveryVault struct{ calls atomic.Int32 }

func (v *unavailableRecoveryVault) Load(string) (credentials.Credentials, error) {
	v.calls.Add(1)
	return credentials.Credentials{}, errors.New("locked synthetic credential service")
}
func (v *unavailableRecoveryVault) Save(string, credentials.Credentials) error {
	v.calls.Add(1)
	return errors.New("locked synthetic credential service")
}
func (v *unavailableRecoveryVault) Delete(string) error {
	v.calls.Add(1)
	return errors.New("locked synthetic credential service")
}

func refreshRecoveryHTTPFixture(t *testing.T, operation string, output bool) (*jobs.Manager, *httptest.Server, *unavailableRecoveryVault, string) {
	t.Helper()
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	st, _ := store.New(t.TempDir())
	dir, _ := store.New(filepath.Join(st.Root, "pending-refresh"))
	write := func(name string, v any) {
		t.Helper()
		if err := dir.WriteJSON(context.Background(), name, v); err != nil {
			t.Fatal(err)
		}
	}
	write("job-meta.json", map[string]any{"email": "refresh@example.test", "status": "failed", "last_operation_type": operation, "last_error": "REFRESH_OUTPUT_PENDING: local save interrupted", "has_password": true})
	if output {
		write("sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": "refresh@example.test", "refresh_token": "old-visible-download-token"}}}})
	}
	write(recoverySidecar, map[string]any{"version": 1, "type": "tosub2-refresh-grant", "tokens": map[string]any{"access_token": recoveryCanary, "refresh_token": recoveryCanary}, "source": map[string]any{"sensitive": recoveryCanary}})
	// The jobs layer deliberately does not parse the private grant. Strict
	// validation and durable completion belong to the production runner tests.
	vault := &unavailableRecoveryVault{}
	m := jobs.NewManager(context.Background(), 1, st, nil)
	m.SetCredentialStore(vault)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if vault.calls.Load() != 0 {
		t.Fatal("pending local refresh restoration read locked credentials")
	}
	s := NewServer(config.Config{OutputRoot: st.Root, StartupToken: "recovery-test-token"}, m, embed.FS{}, nil, credentials.NewMemoryStore())
	s.static = fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>static console</html>")}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(s.Close)
	return m, srv, vault, dir.Root
}
func refreshRecoveryRequest(t *testing.T, srv *httptest.Server, method, path string, body any) (int, []byte, http.Header) {
	t.Helper()
	var input []byte
	if body != nil {
		input, _ = json.Marshal(body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(input))
	req.Header.Set("X-Console-Token", "recovery-test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), recoveryCanary) {
		t.Fatalf("private refresh sidecar exposed by %s %s", method, path)
	}
	return resp.StatusCode, b, resp.Header
}

func TestRestoredPendingRefreshRetryAndRegeneratePreserveLocalOperation(t *testing.T) {
	for _, operation := range []string{"refresh", "regenerate", "reauthorize"} {
		for _, action := range []string{"retry", "regenerate"} {
			t.Run(operation+"_"+action, func(t *testing.T) {
				m, srv, vault, _ := refreshRecoveryHTTPFixture(t, operation, true)
				j, _ := m.Get("pending-refresh")
				if j.Status != jobs.StatusFailed || !j.ResultSaved || j.Operation != operation {
					t.Fatal("restart lost failed refresh state or old download")
				}
				code, b, _ := refreshRecoveryRequest(t, srv, "GET", "/api/jobs", nil)
				if code != 200 {
					t.Fatal(code)
				}
				var page struct{ Jobs []map[string]any }
				if err := json.Unmarshal(b, &page); err != nil || len(page.Jobs) != 1 {
					t.Fatal("cannot inspect restored job", err)
				}
				if page.Jobs[0]["canRetry"] != true || page.Jobs[0]["canRegenerate"] != true || page.Jobs[0]["canDownload"] != true {
					t.Fatal("pending saved grant has no recovery action")
				}
				code, b, _ = refreshRecoveryRequest(t, srv, "POST", "/api/jobs/pending-refresh/"+action, map[string]any{})
				if code != 200 && code != 202 {
					t.Fatalf("recovery action %d: %s", code, b)
				}
				want := operation
				if action == "regenerate" {
					want = "regenerate"
				}
				j, _ = m.Get(j.ID)
				if j.Operation != want || j.Status != jobs.StatusQueued {
					t.Fatalf("saved grant routed into %q instead of %q", j.Operation, want)
				}
				if vault.calls.Load() != 0 {
					t.Fatal("local recovery action required locked credentials")
				}
				operations := make(chan string, 1)
				m.SetRunner(func(_ context.Context, j *jobs.Job, _ func(jobs.JobEvent)) error {
					operations <- j.Operation
					return errors.New("fixture stops before remote I/O")
				})
				m.Start()
				defer m.Shutdown(context.Background())
				select {
				case got := <-operations:
					if got != want {
						t.Fatal("scheduler changed recovery operation")
					}
				case <-time.After(time.Second):
					t.Fatal("recovery operation did not run")
				}
			})
		}
	}
}

func TestRefreshRecoverySidecarNeverEntersPublicResponsesOrDownloads(t *testing.T) {
	for _, output := range []bool{false, true} {
		t.Run(map[bool]string{false: "output_missing", true: "old_output_exists"}[output], func(t *testing.T) {
			_, srv, _, dir := refreshRecoveryHTTPFixture(t, "refresh", output)
			selected := map[string]any{"ids": []string{"pending-refresh"}}
			requests := []struct {
				method, path string
				body         any
			}{
				{"GET", "/api/jobs", nil}, {"POST", "/api/jobs/query", map[string]any{"emails": []string{"refresh@example.test"}}},
				{"GET", "/api/jobs/pending-refresh/logs", nil}, {"POST", "/api/jobs/export-source", selected},
				{"GET", "/pending-refresh/" + recoverySidecar, nil}, {"GET", "/" + recoverySidecar, nil},
			}
			for _, request := range requests {
				refreshRecoveryRequest(t, srv, request.method, request.path, request.body)
			}
			for _, path := range []string{"/api/jobs/pending-refresh/download", "/api/jobs/pending-refresh/download?file=" + recoverySidecar} {
				code, b, h := refreshRecoveryRequest(t, srv, "GET", path, nil)
				want := 404
				if output {
					want = 200
				}
				if code != want {
					t.Fatal("single download did not use fixed output filename", code)
				}
				if output && (!json.Valid(b) || !strings.Contains(string(b), "old-visible-download-token") || h.Get("Content-Disposition") == "") {
					t.Fatal("ordinary download broken")
				}
			}
			code, b, _ := refreshRecoveryRequest(t, srv, "POST", "/api/jobs/download-batch", selected)
			want := 409
			if output {
				want = 200
			}
			if code != want {
				t.Fatal("batch used an unexpected file", code, string(b))
			}
			data, err := os.ReadFile(filepath.Join(dir, recoverySidecar))
			if err != nil || !strings.Contains(string(data), recoveryCanary) {
				t.Fatal("read-only responses modified private grant")
			}
		})
	}
}
