package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/modelprobe"
)

func qualityAPIFixture(t *testing.T, run func(context.Context, modelprobe.Options) (modelprobe.Result, error)) (*Server, *atomic.Int32) {
	t.Helper()
	s, manager := newMonitorTestServer(t)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	if s.quality == nil {
		t.Fatal("quality controller not initialized")
	}
	if err := s.quality.Service.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int32)
	service, err := accountquality.New(s.cfg.OutputRoot, accountquality.Provider{
		Accounts: func() []accountquality.Account {
			return []accountquality.Account{{ID: "normal-10", Key: "synthetic-account-a", Revision: "r1", CanCheck: true}, {ID: "second-20", Key: "synthetic-account-b", Revision: "r1", CanCheck: true}, {ID: "unavailable", Key: "synthetic-account-c", Revision: "r1", Reason: "尚无授权"}}
		},
		Acquire: func(ctx context.Context, id string) (*accountquality.Lease, error) {
			return &accountquality.Lease{Context: ctx, Identity: "synthetic-identity-" + id, Validate: ctx.Err, Release: func() {}, Probe: func(ctx context.Context, o modelprobe.Options) (modelprobe.Result, error) {
				calls.Add(1)
				if run != nil {
					return run(ctx, o)
				}
				return modelprobe.Result{Model: o.Model, RuleVersion: modelprobe.RuleVersion, Verdict: "normal", Score: modelprobe.TotalQuestions, Total: modelprobe.TotalQuestions, CheckedAt: time.Now().UTC()}, nil
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.quality.Service = service
	s.StartAccountQuality(t.Context())
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	return s, calls
}

func enableQualityAPI(t *testing.T, s *Server) accountquality.Settings {
	t.Helper()
	settings, err := s.quality.Service.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Enabled = true
	settings.Model = "synthetic-model"
	w := accountInsightAPIRequest(s, http.MethodPut, "/api/account-quality/settings", accountInsightJSON(t, settings))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestAccountQualityAPIDisabledAndReadsNeverGenerateModelContent(t *testing.T) {
	s, calls := qualityAPIFixture(t, nil)
	for _, path := range []string{"/api/account-quality/settings", "/api/account-quality/status", "/api/account-quality?ids=normal-10,second-20"} {
		w := accountInsightAPIRequest(s, http.MethodGet, path, "")
		if w.Code != 200 {
			t.Fatalf("read %s: %d", path, w.Code)
		}
	}
	w := accountInsightAPIRequest(s, http.MethodPost, "/api/account-quality/refresh", `{"ids":["normal-10"]}`)
	if w.Code != http.StatusConflict || calls.Load() != 0 {
		t.Fatalf("disabled check: status=%d calls=%d", w.Code, calls.Load())
	}
	settings, _ := s.quality.Service.Settings()
	if settings.Enabled || settings.BeforeUpload || !settings.BlockDegraded || !settings.BlockFailed {
		t.Fatal("unsafe default policy")
	}
}

func TestAccountQualityAPIExplicitRefreshDeduplicatesAndCancels(t *testing.T) {
	entered := make(chan struct{}, 1)
	s, calls := qualityAPIFixture(t, func(ctx context.Context, o modelprobe.Options) (modelprobe.Result, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return modelprobe.Result{}, ctx.Err()
	})
	enableQualityAPI(t, s)
	for i := 0; i < 2; i++ {
		w := accountInsightAPIRequest(s, http.MethodPost, "/api/account-quality/refresh", `{"ids":["normal-10"]}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
		}
		var receipt accountquality.Receipt
		if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if i == 0 && len(receipt.Accepted) != 1 || i == 1 && len(receipt.Merged) != 1 {
			t.Fatalf("wrong admission: %+v", receipt)
		}
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("probe not started")
	}
	w := accountInsightAPIRequest(s, http.MethodPost, "/api/account-quality/cancel", `{"ids":["normal-10"]}`)
	if w.Code != 200 {
		t.Fatalf("cancel: %d", w.Code)
	}
	waitAccountInsightAPI(t, func() bool { status := s.quality.Service.Status(); return status.Active == 0 && status.Queued == 0 })
	rows, _ := s.quality.Service.Snapshots([]string{"normal-10"})
	if calls.Load() != 1 || rows["normal-10"].LastAttempt.Status != "cancelled" {
		t.Fatalf("cancel outcome: calls=%d row=%+v", calls.Load(), rows["normal-10"])
	}
}

func TestAccountQualityAPIRejectsInvalidScopeAndUnsafeMethods(t *testing.T) {
	s, calls := qualityAPIFixture(t, nil)
	enableQualityAPI(t, s)
	for _, body := range []string{`{}`, `{"ids":[],"all":false}`, `{"ids":["normal-10"],"all":true}`, `{"ids":["../escape"]}`, `{"ids":["line\nfeed"]}`} {
		w := accountInsightAPIRequest(s, http.MethodPost, "/api/account-quality/refresh", body)
		if w.Code != 400 {
			t.Fatalf("unsafe scope accepted: %s => %d", body, w.Code)
		}
	}
	for _, path := range []string{"/api/account-quality/refresh", "/api/account-quality/cancel"} {
		w := accountInsightAPIRequest(s, http.MethodGet, path, "")
		if w.Code != 404 {
			t.Fatalf("GET started side effect: %d", w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/api/account-quality/refresh", strings.NewReader(`{"all":true}`))
	r.Host = "127.0.0.1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 || calls.Load() != 0 {
		t.Fatalf("unauthorized detection: status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestAccountQualityAPIResultsAreCredentialFreeAndCorrectlyScoped(t *testing.T) {
	s, _ := qualityAPIFixture(t, nil)
	enableQualityAPI(t, s)
	w := accountInsightAPIRequest(s, http.MethodPost, "/api/account-quality/refresh", `{"ids":["normal-10"]}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	waitAccountInsightAPI(t, func() bool {
		rows, _ := s.quality.Service.Snapshots([]string{"normal-10"})
		return rows["normal-10"].LastAttempt.Status == "success"
	})
	w = accountInsightAPIRequest(s, http.MethodGet, "/api/account-quality?ids=normal-10", "")
	var result struct {
		Snapshots map[string]accountquality.Snapshot `json:"snapshots"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	row := result.Snapshots["normal-10"]
	if len(result.Snapshots) != 1 || row.LastResult == nil || row.LastResult.Score != modelprobe.TotalQuestions || row.LastResult.Verdict != "normal" {
		t.Fatal("invalid public measurement", w.Body.String())
	}
	for _, private := range []string{"synthetic-identity", "synthetic-account", "access_token", "refresh_token", "prompt"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("private field escaped: %s", private)
		}
	}
}

func TestAccountQualityAPISettingsRejectStaleWritesAndPersistPolicy(t *testing.T) {
	s, _ := qualityAPIFixture(t, nil)
	settings := enableQualityAPI(t, s)
	settings.BeforeUpload = true
	settings.BlockDegraded = true
	settings.BlockFailed = true
	w := accountInsightAPIRequest(s, http.MethodPut, "/api/account-quality/settings", accountInsightJSON(t, settings))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = accountInsightAPIRequest(s, http.MethodPut, "/api/account-quality/settings", accountInsightJSON(t, settings))
	if w.Code != 409 {
		t.Fatalf("stale policy accepted: %d", w.Code)
	}
	saved, err := accountquality.ReadSettings(s.cfg.OutputRoot)
	if err != nil || !saved.BeforeUpload || saved.Model != "synthetic-model" || !saved.BlockFailed {
		t.Fatalf("policy did not persist: %+v %v", saved, err)
	}
}
