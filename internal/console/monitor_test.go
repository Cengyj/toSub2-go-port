package console

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/sub2api"
)

type monitorMemoryVault struct {
	values map[string]credentials.Credentials
}

func (v *monitorMemoryVault) Save(id string, c credentials.Credentials) error {
	v.values[id] = c
	return nil
}
func (v *monitorMemoryVault) Load(id string) (credentials.Credentials, error) {
	c, ok := v.values[id]
	if !ok {
		return c, os.ErrNotExist
	}
	return c, nil
}
func (v *monitorMemoryVault) Delete(id string) error { delete(v.values, id); return nil }

func newMonitorTestServer(t *testing.T) (*Server, *jobs.Manager) {
	t.Helper()
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	jm := jobs.NewManager(context.Background(), 1, st, slog.Default())
	srv := NewServer(config.Config{OutputRoot: root, StartupToken: "test"}, jm, embed.FS{}, slog.Default())
	srv.monitorVault = &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	t.Cleanup(srv.Close)
	return srv, jm
}

func TestManualAuthorizationExplicitlyClearsPermanentMonitorFailure(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	j, _ := jm.Submit(jobs.Spec{ID: "recovered", Email: "recover@example.com"})
	jm.Cancel(j.ID)
	jm.UpdateAutoRepair(j.ID, nil, "https://remote.example", "account_disabled", true)
	hooks := srv.monitorHooks(sub2api.Config{BaseURL: "https://remote.example"})
	decision, err := hooks.Evaluate(context.Background(), j.Email, nil)
	if err != nil || decision.Action != sub2api.ActionBlocked || decision.ClearPermanent {
		t.Fatal("blocked job unexpectedly clears permanent state")
	}
	jm.RecordAuthorization(j.ID, false, "manual authorization succeeded", nil)
	jm.UpdateResult(j.ID, true, "")
	jm.Emit(jobs.JobEvent{JobID: j.ID, Status: jobs.StatusSucceeded})
	current, _ := jm.Get(j.ID)
	if current.AutoRepairBlocked || current.AutoRepairBlockedReason != "" {
		t.Fatal("successful authorization retained local permanent failure")
	}
	decision, err = hooks.Evaluate(context.Background(), j.Email, nil)
	if err != nil || !decision.ClearPermanent || decision.Action == sub2api.ActionBlocked {
		t.Fatal("successful manual recovery did not explicitly clear persisted monitor block")
	}
}
func TestMonitorAPIConfigSecretsAndDisableRestore(t *testing.T) {
	srv, _ := newMonitorTestServer(t)
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/sub2api/monitor", strings.NewReader(body))
		r.Host = "127.0.0.1"
		r.Header.Set("X-Console-Token", "test")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	w := post(`{"enabled":true,"config":{"baseUrl":"https://example.com","adminApiKey":"ULTRA_SECRET","groupIds":[7]}}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ULTRA_SECRET") {
		t.Fatal("secret response")
	}
	b, err := os.ReadFile(srv.monitorFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ULTRA_SECRET") {
		t.Fatal("plaintext monitor key")
	}
	vault := srv.monitorVault.(*monitorMemoryVault)
	if vault.values[monitorCredentialID("https://example.com")].Password != "ULTRA_SECRET" {
		t.Fatal("key not in secure store")
	}
	w = post(`{"enabled":false}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	restored := sub2api.NewMonitorService(nil, sub2api.MonitorConfig{}, sub2api.MonitorHooks{}, srv.monitorFile())
	if restored.Configuration().Enabled || restored.Configuration().BaseURL != "https://example.com" {
		t.Fatal(restored.Configuration())
	}
	if w := post(`{broken`); w.Code != 400 {
		t.Fatal("invalid JSON accepted", w.Code)
	}
}
func TestMonitorPendingPartialUploadAndNoRepeatedLogin(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	var mu sync.Mutex
	gets := map[string]int{}
	fail := true
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "GET" {
			gets[r.URL.Path]++
		}
		if r.URL.Path == "/api/v1/admin/accounts/3" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Path == "/api/v1/admin/accounts/2/schedulable" && fail {
			w.WriteHeader(503)
			return
		}
		if r.Method == "GET" {
			_, _ = fmt.Fprintf(w, `{"data":{"id":%s,"credentials":{"specific":"keep"},"extra":{"keep":true}}}`, filepath.Base(r.URL.Path))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer remote.Close()
	j, err := jm.Submit(jobs.Spec{ID: "repair", Email: "test@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	jm.Emit(jobs.JobEvent{JobID: j.ID, Status: jobs.StatusSucceeded})
	jm.UpdateResult(j.ID, true, "")
	jm.UpdateAutoRepair(j.ID, []int{1, 2, 3}, sub2api.BackendIdentity(remote.URL), "", false)
	dir := filepath.Join(srv.cfg.OutputRoot, j.ID)
	_ = os.MkdirAll(dir, 0700)
	_ = os.WriteFile(filepath.Join(dir, "sub2api-import-oauth.json"), []byte(`{"accounts":[{"credentials":{"email":"test@example.com","access_token":"NEW_SECRET"}}]}`), 0600)
	cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "ADMIN_SECRET"}
	current, _ := jm.Get(j.ID)
	n, err := srv.syncRepairOutput(context.Background(), cfg, current)
	if n != 1 || err == nil {
		t.Fatalf("partial=%d err=%v", n, err)
	}
	current, _ = jm.Get(j.ID)
	if len(current.AutoRepairPendingAccountIDs) != 1 || current.AutoRepairPendingAccountIDs[0] != 2 {
		t.Fatal(current.AutoRepairPendingAccountIDs)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	n, err = srv.syncRepairOutput(context.Background(), cfg, current)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	current, _ = jm.Get(j.ID)
	if len(current.AutoRepairPendingAccountIDs) != 0 || current.AutoRepairLastSuccessAt == nil || current.Operation != "" {
		t.Fatalf("pending=%v operation=%s", current.AutoRepairPendingAccountIDs, current.Operation)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets["/api/v1/admin/accounts/1"] != 1 || gets["/api/v1/admin/accounts/2"] != 2 || gets["/api/v1/admin/accounts/3"] != 1 {
		t.Fatal(gets)
	}
	b, _ := json.Marshal(current)
	if strings.Contains(string(b), "SECRET") {
		t.Fatal("secret metadata")
	}
}
func TestMonitorShutdownCancelsRequest(t *testing.T) {
	srv, _ := newMonitorTestServer(t)
	started := make(chan struct{})
	canceled := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(canceled) }))
	defer remote.Close()
	if err := srv.configureMonitor(sub2api.Config{BaseURL: remote.URL, APIKey: "key"}, true, false); err != nil {
		t.Fatal(err)
	}
	srv.StartMonitor(context.Background())
	<-started
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor shutdown blocked")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request not canceled")
	}
}

func TestMonitorQueuesDuplicateRemoteIDsOnlyForAutomatedAuthorization(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"items":[{"id":1,"email":"auto@example.com","group_ids":[7],"credentials":{"refresh_token":"REMOTE_SECRET"}},{"id":2,"email":"auto@example.com","group_ids":[7]},{"id":3,"email":"manual@example.com","group_ids":[7]}]}}`))
	}))
	defer remote.Close()
	auto, _ := jm.Submit(jobs.Spec{ID: "auto", Email: "auto@example.com", Input: map[string]any{"password": "PASSWORD"}})
	jm.Cancel(auto.ID)
	jm.RecordAuthorization(auto.ID, true, "", map[string]bool{"password": true})
	manual, _ := jm.Submit(jobs.Spec{ID: "manual", Email: "manual@example.com", Input: map[string]any{"password": "PASSWORD"}})
	jm.Cancel(manual.ID)
	if err := srv.configureMonitor(sub2api.Config{BaseURL: remote.URL, APIKey: "ADMIN_KEY", GroupIDs: []int{7}}, true, false); err != nil {
		t.Fatal(err)
	}
	sum, err := srv.monitorCheck(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Started != 2 || sum.Ineligible != 1 {
		t.Fatalf("%+v", sum)
	}
	job, _ := jm.Get(auto.ID)
	if len(job.AutoRepairPendingAccountIDs) != 2 || job.Operation != "relogin" || job.Status != jobs.StatusQueued {
		t.Fatalf("%+v", job)
	}
	b, err := os.ReadFile(filepath.Join(srv.cfg.OutputRoot, "job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ADMIN_KEY") || strings.Contains(string(b), "REMOTE_SECRET") || strings.Contains(string(b), "PASSWORD") {
		t.Fatal("monitor leaked credentials into metadata")
	}
}
