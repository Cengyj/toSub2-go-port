package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

type accountInsightAPIFixture struct {
	server   *Server
	provider accountinsight.Provider
	calls    atomic.Int32
}

func newAccountInsightAPIFixture(t *testing.T, query func(context.Context) protocol.InsightResult) *accountInsightAPIFixture {
	t.Helper()
	srv, manager := newMonitorTestServer(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	if srv.insights != nil {
		if err := srv.insights.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	f := &accountInsightAPIFixture{server: srv}
	f.provider = accountinsight.Provider{
		Accounts: func() []accountinsight.Account {
			return []accountinsight.Account{
				{ID: "synthetic-first", AuthorizationMethod: "chatgpt", Revision: "synthetic-first-v1", CanQuery: true},
				{ID: "synthetic-second", AuthorizationMethod: "bps", Revision: "synthetic-second-v1", CanQuery: true},
				{ID: "synthetic-busy", AuthorizationMethod: "bps", Revision: "synthetic-busy-v1", Reason: "账号操作进行中，查询暂缓"},
			}
		},
		Acquire: func(ctx context.Context, id string) (*accountinsight.Lease, error) {
			f.calls.Add(1)
			return &accountinsight.Lease{Context: ctx, Identity: "synthetic-" + id, Validate: ctx.Err, Release: func() {}, Query: func(ctx context.Context, _ protocol.InsightsOptions) protocol.InsightResult {
				if query != nil {
					return query(ctx)
				}
				return protocol.InsightResult{Usage: &protocol.InsightUsage{Source: "synthetic", ObservedAt: time.Now().UTC()}, Errors: []protocol.InsightError{}}
			}}, nil
		},
	}
	service, err := accountinsight.New(srv.cfg.OutputRoot, f.provider)
	if err != nil {
		t.Fatal(err)
	}
	srv.insights = service
	srv.StartAccountInsights(t.Context())
	return f
}

func accountInsightJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func accountInsightAPIRequest(srv *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1"
	r.Header.Set("X-Console-Token", "test")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}
func accountInsightAPISettings(t *testing.T, srv *Server, enabled bool) accountinsight.Settings {
	t.Helper()
	settings := srv.insights.Settings()
	settings.Enabled, settings.Automatic = enabled, false
	w := accountInsightAPIRequest(srv, http.MethodPut, "/api/account-insights/settings", accountInsightJSON(t, settings))
	if w.Code != http.StatusOK {
		t.Fatal("settings update failed", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}
func waitAccountInsightAPI(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("account query did not reach expected state")
}

func TestAccountInsightsAPIReadsAreLocalAndDisabledByDefault(t *testing.T) {
	f := newAccountInsightAPIFixture(t, nil)
	paths := []string{"/api/account-insights/settings", "/api/account-insights/status", "/api/account-insights?ids=synthetic-first,synthetic-second", "/api/account-insights/synthetic-first"}
	for _, enabled := range []bool{false, true} {
		if enabled {
			accountInsightAPISettings(t, f.server, true)
		}
		for _, path := range paths {
			w := accountInsightAPIRequest(f.server, http.MethodGet, path, "")
			if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || !json.Valid(w.Body.Bytes()) {
				t.Fatal("local read failed", path, w.Code)
			}
			if strings.Contains(w.Body.String(), "access_token") || strings.Contains(w.Body.String(), "refresh_token") {
				t.Fatal("query metadata exposes authentication fields")
			}
		}
		settings := f.server.insights.Settings()
		if settings.Enabled != enabled {
			t.Fatal("read unexpectedly changed enabled state")
		}
		if !enabled {
			w := accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", `{"ids":["synthetic-first"]}`)
			if w.Code != http.StatusConflict {
				t.Fatal("disabled service admitted query", w.Code)
			}
		}
		if f.calls.Load() != 0 || f.server.insights.Status().Active != 0 || f.server.insights.Status().Queued != 0 {
			t.Fatal("GET or manual-mode settings update queried upstream")
		}
	}
}

func TestAccountInsightsAPISettingsValidatePersistAndConflict(t *testing.T) {
	f := newAccountInsightAPIFixture(t, nil)
	before := f.server.insights.Settings()
	settings := before
	settings.Enabled, settings.Automatic = true, false
	settings.IntervalMinutes = 7
	settings.IncludeResetCredits = true
	settings.ExcludedAccountIDs = []string{"synthetic-second", "synthetic-first", "synthetic-second"}
	w := accountInsightAPIRequest(f.server, http.MethodPut, "/api/account-insights/settings", accountInsightJSON(t, settings))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var saved accountinsight.Settings
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != before.Revision+1 || saved.IntervalMinutes != 7 || !saved.IncludeResetCredits || !reflect.DeepEqual(saved.ExcludedAccountIDs, []string{"synthetic-first", "synthetic-second"}) {
		t.Fatal("saved settings lost validated values")
	}
	w = accountInsightAPIRequest(f.server, http.MethodPut, "/api/account-insights/settings", accountInsightJSON(t, settings))
	if w.Code != http.StatusConflict {
		t.Fatal("stale settings update overwrote a newer revision", w.Code)
	}
	var conflict struct {
		Settings accountinsight.Settings `json:"settings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &conflict); err != nil || conflict.Settings.Revision != saved.Revision {
		t.Fatal("conflict did not return current revision", err)
	}
	for _, mutate := range []func(*accountinsight.Settings){
		func(c *accountinsight.Settings) { c.IntervalMinutes = 0 },
		func(c *accountinsight.Settings) { c.IntervalMinutes = 1441 },
		func(c *accountinsight.Settings) { c.SchemaVersion = 2 },
		func(c *accountinsight.Settings) { c.ExcludedAccountIDs = []string{" "} },
	} {
		invalid := saved
		mutate(&invalid)
		w = accountInsightAPIRequest(f.server, http.MethodPut, "/api/account-insights/settings", accountInsightJSON(t, invalid))
		if w.Code != http.StatusBadRequest || !reflect.DeepEqual(f.server.insights.Settings(), saved) {
			t.Fatal("invalid configuration altered saved settings", w.Code)
		}
	}
	for _, body := range []string{`{"schemaVersion":1,"intervalMinutes":1.5}`, `{invalid`, accountInsightJSON(t, saved) + ` {}`} {
		if w = accountInsightAPIRequest(f.server, http.MethodPut, "/api/account-insights/settings", body); w.Code != http.StatusBadRequest {
			t.Fatal("malformed settings JSON accepted", w.Code)
		}
	}
	reloaded, err := accountinsight.New(f.server.cfg.OutputRoot, f.provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reloaded.Close(context.Background()) })
	if !reflect.DeepEqual(reloaded.Settings(), saved) || f.calls.Load() != 0 {
		t.Fatal("settings did not survive reload without upstream work")
	}
}

func TestAccountInsightsAPIReceiptIsAdmissionAndCancellationStopsQuery(t *testing.T) {
	entered := make(chan struct{})
	f := newAccountInsightAPIFixture(t, func(ctx context.Context) protocol.InsightResult {
		close(entered)
		<-ctx.Done()
		return protocol.InsightResult{}
	})
	accountInsightAPISettings(t, f.server, true)
	w := accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", `{"ids":["synthetic-first","synthetic-first","synthetic-busy","missing"]}`)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Code, w.Body.String())
	}
	var receipt accountinsight.Receipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.QueryID == "" || !reflect.DeepEqual(receipt.Accepted, []string{"synthetic-first"}) || len(receipt.Skipped) != 2 || len(receipt.Merged) != 0 {
		t.Fatal("batch receipt lost admission categories")
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("accepted query never started")
	}
	w = accountInsightAPIRequest(f.server, http.MethodGet, "/api/account-insights/synthetic-first", "")
	var snapshot accountinsight.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.LastAttempt.QueryID != receipt.QueryID || snapshot.LastAttempt.Status != "running" || snapshot.LastSuccessAt != nil || snapshot.Usage != nil || snapshot.Persisted {
		t.Fatal("queued request was shown as a completed observation")
	}
	w = accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", `{"ids":["synthetic-first"]}`)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Accepted) != 0 || !reflect.DeepEqual(receipt.Merged, []string{"synthetic-first"}) || f.calls.Load() != 1 {
		t.Fatal("duplicate request was not merged")
	}
	w = accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/queries/synthetic-first/cancel", `{}`)
	var cancelled map[string]bool
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &cancelled) != nil || !cancelled["cancelled"] {
		t.Fatal("cancel did not acknowledge active query", w.Code)
	}
	waitAccountInsightAPI(t, func() bool {
		row, _ := f.server.insights.Snapshot("synthetic-first")
		return row.LastAttempt.Status == "cancelled" && f.server.insights.Status().Active == 0
	})
	snapshot, _ = f.server.insights.Snapshot("synthetic-first")
	if snapshot.LastSuccessAt != nil || snapshot.Usage != nil || f.calls.Load() != 1 {
		t.Fatal("cancelled query published an observation or restarted")
	}
	w = accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/queries/missing/cancel", `{}`)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &cancelled) != nil || cancelled["cancelled"] {
		t.Fatal("missing query cancellation was reported successful")
	}
}

func TestAccountInsightsAPIBatchLimitsAndMissingRoutes(t *testing.T) {
	f := newAccountInsightAPIFixture(t, nil)
	accountInsightAPISettings(t, f.server, true)
	for _, body := range []string{`{"ids":[]}`, `{"ids":null}`, `{}`, `{"ids":"wrong"}`, `{broken`, `{"ids":["synthetic-first"]} {}`} {
		w := accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", body)
		if w.Code != http.StatusBadRequest {
			t.Fatal("invalid batch admitted", body, w.Code)
		}
	}
	tooMany := make([]string, 1001)
	for i := range tooMany {
		tooMany[i] = "synthetic-first"
	}
	if w := accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", accountInsightJSON(t, map[string]any{"ids": tooMany})); w.Code != http.StatusBadRequest {
		t.Fatal("oversize batch admitted", w.Code)
	}
	if w := accountInsightAPIRequest(f.server, http.MethodGet, "/api/account-insights?ids="+strings.Join(tooMany[:101], ","), ""); w.Code != http.StatusBadRequest {
		t.Fatal("oversize cache read admitted", w.Code)
	}
	w := accountInsightAPIRequest(f.server, http.MethodGet, "/api/account-insights?ids=synthetic-first,missing,synthetic-first", "")
	var batch struct {
		Snapshots map[string]accountinsight.Snapshot `json:"snapshots"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &batch) != nil || len(batch.Snapshots) != 1 || batch.Snapshots["synthetic-first"].ID != "synthetic-first" {
		t.Fatal("cache batch returned missing or duplicate accounts")
	}
	for _, request := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/api/account-insights/missing", http.StatusNotFound},
		{http.MethodPost, "/api/account-insights/queries//cancel", http.StatusBadRequest},
		{http.MethodPost, "/api/account-insights/queries/first/second/cancel", http.StatusBadRequest},
		{http.MethodDelete, "/api/account-insights/settings", http.StatusNotFound},
		{http.MethodPost, "/api/account-insights/synthetic-first", http.StatusNotFound},
	} {
		if w := accountInsightAPIRequest(f.server, request.method, request.path, `{}`); w.Code != request.status {
			t.Fatal("unexpected route result", request.path, w.Code)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("invalid request reached upstream")
	}
}

func TestAccountInsightsAPIRequiresTokenAndSameOrigin(t *testing.T) {
	f := newAccountInsightAPIFixture(t, nil)
	settings := f.server.insights.Settings()
	settings.Enabled, settings.Automatic = true, false
	requests := []struct{ method, path, body string }{
		{http.MethodGet, "/api/account-insights/settings", ""},
		{http.MethodPut, "/api/account-insights/settings", accountInsightJSON(t, settings)},
		{http.MethodPost, "/api/account-insights/refresh", `{"ids":["synthetic-first"]}`},
	}
	for _, attack := range []string{"missing-token", "wrong-token", "cross-origin", "cross-site", "foreign-host"} {
		for _, request := range requests {
			r := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
			r.Host = "127.0.0.1"
			r.Header.Set("X-Console-Token", "test")
			switch attack {
			case "missing-token":
				r.Header.Del("X-Console-Token")
			case "wrong-token":
				r.Header.Set("X-Console-Token", "synthetic-wrong")
			case "cross-origin":
				r.Header.Set("Origin", "https://foreign.example.test")
			case "cross-site":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			case "foreign-host":
				r.Host = "foreign.example.test"
			}
			w := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatal("query route bypassed console boundary", attack, request.method, w.Code)
			}
		}
	}
	if f.calls.Load() != 0 || f.server.insights.Settings().Enabled {
		t.Fatal("unauthorized request mutated settings or queried upstream")
	}
}

func TestAccountInsightsAPIUnavailableReturnsServiceUnavailable(t *testing.T) {
	f := newAccountInsightAPIFixture(t, nil)
	if err := f.server.insights.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.server.insights = nil
	if w := accountInsightAPIRequest(f.server, http.MethodGet, "/api/account-insights/settings", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatal("unavailable storage was treated as success", w.Code)
	}
}

func TestAccountInsightsBootstrapFeatureTracksAvailability(t *testing.T) {
	f := newAccountInsightAPIFixture(t, nil)
	for _, available := range []bool{true, false} {
		if !available {
			if err := f.server.insights.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.server.insights = nil
		}
		w := accountInsightAPIRequest(f.server, http.MethodGet, "/api/bootstrap", "")
		var payload struct {
			Features map[string]any `json:"features"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &payload) != nil || payload.Features["accountInsights"] != available || payload.Features["sub2apiMonitor"] != true || payload.Features["totpReset"] != true {
			t.Fatal("bootstrap feature changed established capabilities or query availability")
		}
	}
}

func TestAccountInsightsAccountDeletePrunesCacheAndReportsPartialCleanup(t *testing.T) {
	for _, failCleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "cleanup-failed"}[failCleanup], func(t *testing.T) {
			srv, manager := newMonitorTestServer(t)
			t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
			job, err := manager.Submit(jobs.Spec{ID: "synthetic-delete", Email: "delete@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(job.ID))
			file := filepath.Join(srv.cfg.OutputRoot, "account-insights", hex.EncodeToString(sum[:])+".json")
			if err := protocol.WritePrivateJSON(file, map[string]any{"schemaVersion": 1, "snapshot": map[string]any{"id": job.ID}}); err != nil {
				t.Fatal(err)
			}
			if failCleanup {
				dir := filepath.Dir(file)
				if err := os.Rename(dir, dir+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dir, []byte("synthetic unavailable cache directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			w := accountInsightAPIRequest(srv, http.MethodPost, "/api/jobs/delete-batch", accountInsightJSON(t, map[string]any{"ids": []string{job.ID}}))
			var payload struct {
				Deleted      int    `json:"deleted"`
				Completed    bool   `json:"accountDeletionCompleted"`
				CacheCleanup string `json:"cacheCleanup"`
				Outcome      string `json:"outcome"`
				Error        string `json:"error"`
			}
			if json.Unmarshal(w.Body.Bytes(), &payload) != nil || payload.Deleted != 1 {
				t.Fatal("delete response lost completed deletion count", w.Code, w.Body.String())
			}
			if _, exists := manager.Get(job.ID); exists {
				t.Fatal("cleanup failure pretended to roll back account deletion")
			}
			if failCleanup {
				if w.Code != http.StatusServiceUnavailable || !payload.Completed || payload.CacheCleanup != "pending" || payload.Outcome != "partial" || !strings.Contains(payload.Error, "账号已删除") {
					t.Fatal("partial cache cleanup was not explicit", w.Code)
				}
			} else if w.Code != http.StatusOK {
				t.Fatal("successful cleanup failed", w.Code)
			} else if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatal("deleted account cache retained", err)
			}
		})
	}
}
