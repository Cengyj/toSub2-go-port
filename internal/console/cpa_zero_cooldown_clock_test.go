package console

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestCPAMonitorZeroCooldownIgnoresRestoredFutureAttempt(t *testing.T) {
	s, manager := newMonitorTestServer(t)
	vault := credentials.NewMemoryStore()
	manager.SetCredentialStore(vault)
	job, entry := cpaRepairFixture(t, s, manager, "cpa-zero-cooldown-clock")
	if err := vault.Save(job.Email, credentials.Credentials{Password: "synthetic-password"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := manager.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	// A wall-clock correction between runs can leave an otherwise valid
	// completed account with its previous attempt timestamp in the future.
	st, err := store.New(s.cfg.OutputRoot)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]json.RawMessage
	if err := st.ReadJSON(t.Context(), "job-meta.json", &metadata); err != nil {
		t.Fatal(err)
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(metadata["jobs"], &records); err != nil || len(records) != 1 {
		t.Fatal("expected one synthetic persisted account", err)
	}
	future := time.Now().UTC().Add(time.Hour)
	records[0]["auto_repair_last_attempt_at"], err = json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	metadata["jobs"], err = json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteJSON(t.Context(), "job-meta.json", metadata); err != nil {
		t.Fatal(err)
	}
	restoredManager := jobs.NewManager(t.Context(), 1, st, nil)
	restoredManager.SetCredentialStore(vault)
	if err := restoredManager.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored := NewServer(s.cfg, restoredManager, embed.FS{}, nil, vault)
	t.Cleanup(func() {
		restored.Close()
		if err := restoredManager.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	current, ok := restoredManager.Get(job.ID)
	if !ok || current.Status != jobs.StatusSucceeded || current.AutoRepairLastAttemptAt == nil || !current.AutoRepairLastAttemptAt.Equal(future) {
		t.Fatal("restart did not retain the completed account and future attempt")
	}
	restoredCredentials, err := restoredManager.LoadCredentialsContext(t.Context(), job.ID)
	if err != nil || restoredCredentials.Password != "synthetic-password" {
		t.Fatal("restart did not load the synthetic password from the credential store", err)
	}
	if !restored.cpaRepairEligible(t.Context(), current) {
		t.Fatal("restored synthetic account lost its automatic authorization prerequisites")
	}
	var reads atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("queue admission attempted a remote mutation")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		reads.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{entry}})
	}))
	defer remote.Close()
	connectSharedMonitors(t, restored, remote.URL, remote.URL)
	policy := poolPolicyBody(restored, "cpa", true)
	policy["excludedPlanTypes"], policy["cooldownMinutes"] = []string{}, 0
	if w := frontendRequest(restored, http.MethodPost, "/api/pool-monitor", policy, "test"); w.Code != http.StatusOK {
		t.Fatalf("zero cooldown policy rejected: %d %s", w.Code, w.Body.String())
	}
	w := frontendRequest(restored, http.MethodPost, "/api/pool-monitor/check", map[string]any{"revision": restored.poolMonitor.Revision}, "test")
	var state struct{ LastResult cpaMonitorSummary }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.LastResult.Checked != 1 || state.LastResult.Started != 1 || state.LastResult.Cooldown != 0 || reads.Load() != 1 {
		t.Fatalf("future attempt blocked zero cooldown: status=%d result=%+v reads=%d", w.Code, state.LastResult, reads.Load())
	}
	queued, _ := restoredManager.Get(job.ID)
	if queued.Status != jobs.StatusQueued || queued.LastOperationType != "automatic_relogin" || queued.UploadTarget != jobs.UploadTargetCPA {
		t.Fatal("zero cooldown did not queue the restored CPA authorization")
	}
}
