package application

import (
	"context"
	"io"
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
)

func TestSavedAuthorizationUploadPreservesNumbersAndRejectsTrailingData(t *testing.T) {
	for _, trailing := range []string{"", " \n\t", " {}", " null", " trailing"} {
		t.Run(trailing, func(t *testing.T) {
			valid := strings.TrimSpace(trailing) == ""
			var reads, writes atomic.Int32
			var body string
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					reads.Add(1)
					_, _ = io.WriteString(w, `{"data":{"items":[],"pages":1,"total":0}}`)
					return
				}
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				body = string(data)
				writes.Add(1)
				_, _ = io.WriteString(w, `{"created":1}`)
			}))
			defer remote.Close()
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager := jobs.NewManager(t.Context(), 1, st, nil)
			manager.SetCredentialStore(credentials.NewMemoryStore())
			t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
			job, err := manager.Submit(jobs.Spec{ID: "precision", Email: "precision@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			job, err = manager.QueueOperation(job.ID, "relogin", map[string]any{"uploadAfterRelogin": true, "sub2apiConfig": map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-admin-key"}})
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(st.Root, job.ID)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			document := `{"type":"sub2api-data","accounts":[{"platform":"openai","type":"oauth","credentials":{"email":"precision@example.test","access_token":"synthetic-grant","vendor_sequence":9007199254740993},"extra":{"vendor_configuration":{"seed":9223372036854775807,"ratio":1.0000000000000001}}}]}` + trailing
			writePrivateGrantFixture(t, filepath.Join(dir, "sub2api-import-oauth.json"), document)
			err = UploadSavedAuthorization(t.Context(), st.Root, manager, job, nil)
			if !valid {
				if err == nil || reads.Load() != 0 || writes.Load() != 0 {
					t.Fatalf("invalid saved document reached remote: err=%v reads=%d writes=%d", err, reads.Load(), writes.Load())
				}
				return
			}
			if err != nil || writes.Load() != 1 {
				t.Fatalf("upload failed: err=%v writes=%d", err, writes.Load())
			}
			for _, number := range []string{"9007199254740993", "9223372036854775807", "1.0000000000000001"} {
				if !strings.Contains(body, number) {
					t.Errorf("saved authorization upload changed JSON number %s", number)
				}
			}
		})
	}
}
