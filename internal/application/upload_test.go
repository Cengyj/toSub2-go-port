package application

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/sub2api"
)

func TestUploadApplicationRetainsMutationEvidence(t *testing.T) {
	for _, scenario := range []string{"identity-mismatch", "lookup-failed", "rejected", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			var reads, writes atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					reads.Add(1)
					if scenario == "lookup-failed" {
						_, _ = w.Write([]byte(`{}`))
						return
					}
					_, _ = w.Write([]byte(`{"data":{"items":[],"pages":1,"total":0}}`))
					return
				}
				writes.Add(1)
				if scenario == "rejected" {
					_, _ = w.Write([]byte(`{"success":false,"message":"remote-private-canary"}`))
					return
				}
				_, _ = w.Write([]byte(`null`))
			}))
			defer remote.Close()
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager := jobs.NewManager(t.Context(), 1, st, nil)
			manager.SetCredentialStore(credentials.NewMemoryStore())
			t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
			job, err := manager.Submit(jobs.Spec{ID: "application", Email: "application@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			job, err = manager.QueueOperation(job.ID, "relogin", map[string]any{"uploadAfterRelogin": true, "sub2apiConfig": map[string]any{"baseUrl": remote.URL, "adminApiKey": "admin-private-canary"}})
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(st.Root, job.ID)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			document := `{"type":"sub2api-data","accounts":[{"platform":"openai","type":"oauth","credentials":{"email":"application@example.test","access_token":"grant-private-canary"}}]}`
			if scenario == "identity-mismatch" {
				document = strings.ReplaceAll(document, "application@example.test", "other@example.test")
			}
			if err := os.WriteFile(filepath.Join(dir, "sub2api-import-oauth.json"), []byte(document), 0600); err != nil {
				t.Fatal(err)
			}
			err = UploadSavedAuthorization(t.Context(), st.Root, manager, job, nil)
			var outcome interface{ UploadOutcomeUncertain() bool }
			if err == nil || !errors.As(err, &outcome) || outcome.UploadOutcomeUncertain() != (scenario == "unknown") {
				t.Fatalf("incorrect application outcome: %v", err)
			}
			wantWrites := int32(1)
			if scenario == "identity-mismatch" || scenario == "lookup-failed" {
				wantWrites = 0
			}
			if writes.Load() != wantWrites || (scenario == "identity-mismatch" && reads.Load() != 0) {
				t.Fatal("unexpected remote side effect or retry")
			}
			if scenario == "rejected" && !errors.Is(err, sub2api.ErrMutationRejected) {
				t.Fatal("application lost rejection cause")
			}
			if strings.Contains(err.Error(), "private-canary") {
				t.Fatal("application exposed private material")
			}
		})
	}
}
