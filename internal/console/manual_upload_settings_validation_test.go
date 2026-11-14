package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/jobs"
)

func TestManualUploadInvalidSettingsMakeNoRemoteRequest(t *testing.T) {
	srv, manager, base := prepareSMSFlow(t, 2000)
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer remote.Close()
	manager.SetRunner(func(_ context.Context, job *jobs.Job, _ func(jobs.JobEvent)) error {
		if err := writeFlowExport(srv.cfg.OutputRoot, job.ID, job.Email); err != nil {
			return err
		}
		manager.UpdateResult(job.ID, true, "")
		return nil
	})
	manager.Start()
	job, err := manager.Submit(jobs.Spec{ID: "invalid-options", Email: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	waitConsoleState(t, manager, job.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded })
	for name, invalid := range map[string]map[string]any{
		"negative": {"concurrency": "-1"}, "oversize": {"priority": "10001"},
		"fractional": {"proxyId": "1.5"}, "boolean": {"loadFactor": true},
		"invalid-group": {"groupIds": []string{"0"}},
		"unsafe-group":  {"groupIds": []string{"9007199254740992"}},
		"invalid-model": {"modelWhitelist": []any{"model-one", 3}},
	} {
		t.Run(name, func(t *testing.T) {
			settings := map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-admin-key"}
			for field, value := range invalid {
				settings[field] = value
			}
			status, _ := consoleHTTPPost(t, base, "/api/sub2api/upload", map[string]any{"ids": []string{job.ID}, "config": settings})
			if status != http.StatusBadRequest || requests.Load() != 0 {
				t.Fatalf("invalid settings reached remote: status=%d requests=%d", status, requests.Load())
			}
		})
	}
}
