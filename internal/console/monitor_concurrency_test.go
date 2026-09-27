package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

func TestMonitorInFlightTransferCannotOverwriteManualReloginOrPermanentFailure(t *testing.T) {
	for _, action := range []string{"manual-relogin", "permanent-failure"} {
		for stopAt, step := range []string{"GET", "PUT", "clear-error", "schedulable"} {
			t.Run(action+"/"+step, func(t *testing.T) {
				srv, manager := newMonitorTestServer(t)
				entered, released := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(released) }) }
				var mu sync.Mutex
				requests := []string{}
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/admin/accounts" {
						_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
						return
					}
					mu.Lock()
					requests = append(requests, r.Method+" "+r.URL.Path)
					mu.Unlock()
					if (r.Method == step && r.URL.Path == "/api/v1/admin/accounts/61") || strings.HasSuffix(r.URL.Path, "/"+step) {
						close(entered)
						select {
						case <-released:
						case <-r.Context().Done():
							return
						}
					}
					if r.Method == http.MethodGet {
						_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": 61, "credentials": map[string]any{"email": "owner@example.test"}}})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
				}))
				defer remote.Close()
				defer release()
				cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-admin-key"}
				if err := srv.configureMonitor(cfg, true, false); err != nil {
					t.Fatal(err)
				}
				job := monitorPendingFixture(t, srv, manager, sub2api.BackendIdentity(remote.URL))
				response := make(chan *httptest.ResponseRecorder, 1)
				finished := make(chan struct{})
				go func() { defer close(finished); response <- runMonitorOutputCheck(srv) }()
				defer func() {
					release()
					select {
					case <-finished:
					case <-time.After(3 * time.Second):
						t.Error("monitor request did not release fixture resources")
					}
				}()
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("remote fixture did not reach the blocked lifecycle step")
				}
				if action == "manual-relogin" {
					request := httptest.NewRequest(http.MethodPost, "/api/jobs/"+job.ID+"/relogin", strings.NewReader("{}"))
					request.Host = "127.0.0.1"
					request.Header.Set("X-Console-Token", "test")
					reply := httptest.NewRecorder()
					srv.Handler().ServeHTTP(reply, request)
					if reply.Code != http.StatusOK {
						t.Fatalf("manual relogin failed: %d %s", reply.Code, reply.Body.String())
					}
				} else {
					manager.UpdateAutoRepair(job.ID, nil, "", "account_disabled", true)
				}
				changed, _ := manager.Get(job.ID)
				release()
				select {
				case reply := <-response:
					if reply.Code != http.StatusBadGateway {
						t.Fatalf("superseded transfer reported success: %d", reply.Code)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("superseded monitor transfer did not finish")
				}
				after, _ := manager.Get(job.ID)
				if !reflect.DeepEqual(changed, after) {
					t.Fatal("in-flight transfer overwrote a newer account operation")
				}
				mu.Lock()
				defer mu.Unlock()
				if len(requests) != stopAt+1 {
					t.Fatalf("superseded transfer continued remote lifecycle: %v", requests)
				}
			})
		}
	}
}

func TestMonitorCanceledTransferResumesRemainingLifecycleWithoutNewLogin(t *testing.T) {
	t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "0")
	srv, manager := newMonitorTestServer(t)
	entered, canceled := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	var mu sync.Mutex
	requests := map[string]int{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/api/v1/admin/accounts" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
			return
		}
		mu.Lock()
		requests[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		if r.URL.Path == "/api/v1/admin/accounts/61/clear-error" && first.CompareAndSwap(true, false) {
			close(entered)
			<-r.Context().Done()
			close(canceled)
			return
		}
		if r.Method == http.MethodGet {
			var id int
			_, _ = fmt.Sscanf(r.URL.Path, "/api/v1/admin/accounts/%d", &id)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": id, "credentials": map[string]any{"email": "owner@example.test", "remote_option": true}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer remote.Close()
	cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-admin-key"}
	if err := srv.configureMonitor(cfg, true, false); err != nil {
		t.Fatal(err)
	}
	job := monitorPendingFixture(t, srv, manager, sub2api.BackendIdentity(remote.URL))
	var logins atomic.Int32
	manager.SetRunner(func(context.Context, *jobs.Job, func(jobs.JobEvent)) error { logins.Add(1); return nil })
	manager.Start()
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/api/sub2api/monitor/check", nil).WithContext(ctx)
		request.Host = "127.0.0.1"
		request.Header.Set("X-Console-Token", "test")
		reply := httptest.NewRecorder()
		srv.Handler().ServeHTTP(reply, request)
		response <- reply
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("remote update did not reach the cancelable step")
	}
	cancel()
	select {
	case reply := <-response:
		if reply.Code != http.StatusBadGateway {
			t.Fatal("canceled monitor request reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled request did not stop")
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("remote HTTP request was not canceled")
	}
	interrupted, _ := manager.Get(job.ID)
	if !reflect.DeepEqual(interrupted.AutoRepairPendingAccountIDs, []int{61, 62}) || interrupted.AutoRepairLastSuccessAt != nil || interrupted.AutoRepairLastError == "" {
		t.Fatal("cancellation lost pending targets or incorrectly recorded success")
	}
	mu.Lock()
	if requests["GET /api/v1/admin/accounts/62"] != 0 || requests["POST /api/v1/admin/accounts/61/schedulable"] != 0 {
		t.Error("canceled update continued to another account or lifecycle step")
	}
	mu.Unlock()
	if reply := runMonitorOutputCheck(srv); reply.Code != http.StatusOK {
		t.Fatalf("pending transfer could not resume: %d %s", reply.Code, reply.Body.String())
	}
	completed, _ := manager.Get(job.ID)
	if len(completed.AutoRepairPendingAccountIDs) != 0 || completed.AutoRepairLastSuccessAt == nil || logins.Load() != 0 {
		t.Fatal("resumed transfer repeated login or retained completed targets")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []int{61, 62} {
		base := fmt.Sprintf("/api/v1/admin/accounts/%d", id)
		attempts := 1
		if id == 61 {
			attempts = 2
		}
		if requests["GET "+base] != attempts || requests["PUT "+base] != attempts || requests["POST "+base+"/clear-error"] != attempts || requests["POST "+base+"/schedulable"] != 1 {
			t.Fatal("resumed transfer omitted or duplicated an already-completed lifecycle", requests)
		}
	}
}

func monitorPendingFixture(t *testing.T, srv *Server, manager *jobs.Manager, backend string) *jobs.Job {
	t.Helper()
	job, err := manager.Submit(jobs.Spec{ID: "in-flight-repair", Email: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	manager.Cancel(job.ID)
	manager.Emit(jobs.JobEvent{JobID: job.ID, Status: jobs.StatusSucceeded})
	manager.UpdateResult(job.ID, true, "")
	manager.UpdateAutoRepair(job.ID, []int{61, 62}, backend, "", false)
	if err := writeFlowExport(srv.cfg.OutputRoot, job.ID, job.Email); err != nil {
		t.Fatal(err)
	}
	current, _ := manager.Get(job.ID)
	return current
}
