package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestCancellationReportsFailedPersistenceAndCanBeConfirmedAfterRepair(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "single"
		if all {
			name = "all"
		}
		t.Run(name, func(t *testing.T) {
			srv, manager := newMonitorTestServer(t)
			job, err := manager.Submit(jobs.Spec{ID: "cancel-persistence", Email: "cancel@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(srv.cfg.OutputRoot, "job-meta.json")
			backup := metadata + ".fixture"
			if err := os.Rename(metadata, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(metadata, 0700); err != nil {
				t.Fatal(err)
			}
			path := "/api/jobs/" + job.ID + "/cancel"
			if all {
				path = "/api/jobs/cancel-all"
			}
			request := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, path, nil)
				r.Host = "127.0.0.1"
				r.Header.Set("X-Console-Token", "test")
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, r)
				return w
			}
			w := request()
			if w.Code != http.StatusConflict {
				t.Fatalf("failed cancellation save reported HTTP %d success", w.Code)
			}
			var receipt struct {
				OK        bool   `json:"ok"`
				Uncertain bool   `json:"uncertain"`
				Error     string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || receipt.OK || !receipt.Uncertain || !strings.Contains(receipt.Error, "重启") {
				t.Fatalf("missing durable cancellation warning: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), srv.cfg.OutputRoot) {
				t.Fatal("cancellation error leaked private storage path")
			}
			current, ok := manager.Get(job.ID)
			if !ok || current.Status != jobs.StatusCanceled || manager.QueuePosition(job.ID) != 0 {
				t.Fatal("failed persistence allowed canceled work to remain executable")
			}
			if err := os.Remove(metadata); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(backup, metadata); err != nil {
				t.Fatal(err)
			}
			if w = request(); w.Code != http.StatusOK {
				t.Fatalf("cannot confirm cancellation after storage repair: %d", w.Code)
			}
			st, err := store.New(srv.cfg.OutputRoot)
			if err != nil {
				t.Fatal(err)
			}
			restored := jobs.NewManager(t.Context(), 1, st, nil)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			if current, ok = restored.Get(job.ID); !ok || current.Status != jobs.StatusCanceled || restored.QueuePosition(job.ID) != 0 {
				t.Fatal("confirmed cancellation was lost after restart")
			}
		})
	}
}
