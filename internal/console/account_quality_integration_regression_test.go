package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/modelprobe"
)

func TestAccountQualityAPIRequiresCompleteExplicitPolicy(t *testing.T) {
	for _, kind := range []string{"missing-enabled", "missing-beforeUpload", "missing-blockDegraded", "missing-blockFailed", "null-beforeUpload", "duplicate-beforeUpload", "unknown-field"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := qualityAPIFixture(t, nil)
			settings := enableQualityAPI(t, s)
			settings.BeforeUpload = true
			settings, err := s.quality.Service.Update(t.Context(), settings)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(accountInsightJSON(t, settings)), &payload); err != nil {
				t.Fatal(err)
			}
			switch {
			case strings.HasPrefix(kind, "missing-"):
				delete(payload, strings.TrimPrefix(kind, "missing-"))
			case kind == "null-beforeUpload":
				payload["beforeUpload"] = nil
			case kind == "unknown-field":
				payload["beforeUplaod"] = false
			}
			body := accountInsightJSON(t, payload)
			if kind == "duplicate-beforeUpload" {
				body = strings.TrimSuffix(body, "}") + `,"beforeUpload":false}`
			}
			w := accountInsightAPIRequest(s, http.MethodPut, "/api/account-quality/settings", body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("malformed policy accepted: status=%d body=%s", w.Code, w.Body.String())
			}
			saved, err := accountquality.ReadSettings(s.cfg.OutputRoot)
			if err != nil || saved != settings {
				t.Fatalf("rejected request changed effective policy: saved=%+v want=%+v err=%v", saved, settings, err)
			}
		})
	}
}

func qualityTrashFixture(t *testing.T, run func(context.Context, string, modelprobe.Options) (modelprobe.Result, error)) (*Server, *jobs.Manager, *accountquality.Service) {
	t.Helper()
	s, manager, root := trashConsole(t)
	for _, spec := range []jobs.Spec{
		{ID: "blocking-quality", Email: "blocking-quality@example.test", ImportOnly: true},
		{ID: "queued-quality", Email: "queued-quality@example.test", ImportOnly: true},
		{ID: "historical-quality", Email: "queued-quality@example.test", ImportOnly: true},
	} {
		if _, err := manager.Submit(spec); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.quality.Service.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	service, err := accountquality.New(root, accountquality.Provider{
		Accounts: func() []accountquality.Account {
			out := []accountquality.Account{}
			for _, j := range manager.List() {
				out = append(out, accountquality.Account{ID: j.ID, Key: j.Email, Revision: fmt.Sprintf("%s|%s", j.CreatedAt, j.UpdatedAt), CanCheck: true})
			}
			return out
		},
		RetainedIDs: manager.RetainedAccountIDs,
		Acquire: func(ctx context.Context, id string) (*accountquality.Lease, error) {
			lease, err := manager.ReserveAccountQuery(ctx, id)
			if err != nil {
				return nil, err
			}
			return &accountquality.Lease{Context: lease.Context(), Identity: "synthetic-" + id, Release: lease.Release, Validate: func() error {
				if !lease.Valid() {
					return jobs.ErrAccountQuerySuperseded
				}
				return nil
			}, Probe: func(ctx context.Context, options modelprobe.Options) (modelprobe.Result, error) {
				return run(ctx, id, options)
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.quality.Service = service
	service.Start(t.Context())
	settings, err := service.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Enabled, settings.Model = true, "synthetic-model"
	if _, err := service.Update(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	return s, manager, service
}

func TestAccountQualityTrashRestoreDoesNotReplayQueuedDetection(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var queuedCalls atomic.Int32
	s, manager, service := qualityTrashFixture(t, func(ctx context.Context, id string, options modelprobe.Options) (modelprobe.Result, error) {
		if id == "blocking-quality" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return modelprobe.Result{}, ctx.Err()
			}
		} else {
			queuedCalls.Add(1)
		}
		return modelprobe.Result{Model: options.Model, RuleVersion: modelprobe.RuleVersion, Verdict: "normal", Score: modelprobe.TotalQuestions, Total: modelprobe.TotalQuestions, CheckedAt: time.Now().UTC()}, nil
	})
	if _, err := service.Refresh(t.Context(), []string{"blocking-quality"}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("blocking check did not start")
	}
	if receipt, err := service.Refresh(t.Context(), []string{"historical-quality"}, false); err != nil || len(receipt.Accepted) != 1 {
		t.Fatal("target was not queued", receipt, err)
	}
	w := trashRequest(s, http.MethodPost, "/api/account-trash/move", `{"ids":["queued-quality"]}`)
	if w.Code != http.StatusOK || len(manager.ListTrash()) != 1 {
		t.Fatalf("move: status=%d body=%s", w.Code, w.Body.String())
	}
	body := accountInsightJSON(t, map[string]any{"ids": []string{manager.ListTrash()[0].ID}})
	w = trashRequest(s, http.MethodPost, "/api/account-trash/restore", body)
	if w.Code != http.StatusOK {
		t.Fatalf("restore: status=%d body=%s", w.Code, w.Body.String())
	}
	close(release)
	waitAccountInsightAPI(t, func() bool { status := service.Status(); return status.Active == 0 && status.Queued == 0 })
	if got := queuedCalls.Load(); got != 0 {
		t.Fatalf("restoring account replayed a detection queued before deletion: calls=%d", got)
	}
}

func TestAccountQualityTrashWaitsForActiveReleaseAndFencesRefresh(t *testing.T) {
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32
	s, manager, service := qualityTrashFixture(t, func(ctx context.Context, _ string, _ modelprobe.Options) (modelprobe.Result, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		close(cancelled)
		// Cancellation does not release ownership until transport cleanup ends.
		<-release
		return modelprobe.Result{}, ctx.Err()
	})
	t.Cleanup(releaseProbe)
	if _, err := service.Refresh(t.Context(), []string{"historical-quality"}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("active check did not start")
	}
	moved := make(chan int, 1)
	go func() {
		moved <- trashRequest(s, http.MethodPost, "/api/account-trash/move", `{"ids":["queued-quality"]}`).Code
	}()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("trash did not cancel the same-email active check")
	}
	if _, exists := manager.Get("historical-quality"); !exists {
		t.Fatal("account hidden while the cancelled probe still owned it")
	}
	refreshed := make(chan *http.Response, 1)
	go func() {
		refreshed <- trashRequest(s, http.MethodPost, "/api/account-quality/refresh", `{"ids":["historical-quality"]}`).Result()
	}()
	select {
	case status := <-moved:
		t.Fatalf("move returned before probe release: status=%d", status)
	case response := <-refreshed:
		_ = response.Body.Close()
		t.Fatal("concurrent refresh escaped the recycle admission fence")
	case <-time.After(30 * time.Millisecond):
	}
	releaseProbe()
	select {
	case status := <-moved:
		if status != http.StatusOK {
			t.Fatalf("move failed after release: status=%d", status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("move did not finish after probe release")
	}
	select {
	case response := <-refreshed:
		defer response.Body.Close()
		var receipt accountquality.Receipt
		if err := json.NewDecoder(response.Body).Decode(&receipt); err != nil || response.StatusCode != http.StatusAccepted || len(receipt.Accepted) != 0 || len(receipt.Skipped) != 1 {
			t.Fatalf("hidden account was admitted: status=%d receipt=%+v err=%v", response.StatusCode, receipt, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not resume after recycle fence release")
	}
	if calls.Load() != 1 || service.Status().Active != 0 || service.Status().Queued != 0 {
		t.Fatal("old check or ownership survived move", calls.Load(), service.Status())
	}
	body := accountInsightJSON(t, map[string]any{"ids": []string{manager.ListTrash()[0].ID}})
	if w := trashRequest(s, http.MethodPost, "/api/account-trash/restore", body); w.Code != http.StatusOK {
		t.Fatal("restore failed", w.Code)
	}
	lease, err := manager.ReserveAccountQuery(t.Context(), "historical-quality")
	if err != nil {
		t.Fatal("restored account retained old ownership", err)
	}
	lease.Release()
}

func TestAccountQualityTrashCancellationLeavesAccountVisible(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	s, manager, service := qualityTrashFixture(t, func(ctx context.Context, _ string, _ modelprobe.Options) (modelprobe.Result, error) {
		close(entered)
		<-ctx.Done()
		<-release
		return modelprobe.Result{}, ctx.Err()
	})
	t.Cleanup(releaseProbe)
	if _, err := service.Refresh(t.Context(), []string{"historical-quality"}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("probe not started")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/api/account-trash/move", strings.NewReader(`{"ids":["queued-quality"]}`)).WithContext(ctx)
	r.Host = "127.0.0.1"
	r.Header.Set("X-Console-Token", "synthetic-console-token")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusOK || len(manager.ListTrash()) != 0 {
		t.Fatal("unreleased check allowed account movement", w.Code, w.Body.String())
	}
	if _, exists := manager.Get("historical-quality"); !exists {
		t.Fatal("timed-out move hid account")
	}
	if service.Status().Active != 1 {
		t.Fatal("cancellation released ownership before probe cleanup")
	}
	releaseProbe()
	waitAccountInsightAPI(t, func() bool { return service.Status().Active == 0 })
	lease, err := manager.ReserveAccountQuery(t.Context(), "historical-quality")
	if err != nil {
		t.Fatal("failed move leaked account deletion fence", err)
	}
	lease.Release()
}
