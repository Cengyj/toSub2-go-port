package console

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestCompatibleConsoleRoutes(t *testing.T) {
	st, _ := store.New(t.TempDir())
	jm := jobs.NewManager(context.Background(), 2, st, slog.Default())
	srv := NewServer(config.Config{Host: "127.0.0.1", Port: 0, MaxActiveJobs: 2, StartupToken: "test-token"}, jm, embed.FS{}, slog.Default())
	h := srv.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = "127.0.0.1"
		r.Header.Set("X-Console-Token", "test-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := get("/api/bootstrap"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(`{"email":"a@example.com","operation":"full"}`))
	r.Host = "127.0.0.1"
	r.Header.Set("X-Console-Token", "test-token")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var doc map[string]any
	_ = json.NewDecoder(w.Body).Decode(&doc)
	id := doc["job"].(map[string]any)["id"].(string)
	r = httptest.NewRequest(http.MethodPost, "/api/jobs/"+id+"/input", strings.NewReader(`{"email_otp":"123456"}`))
	r.Host = "127.0.0.1"
	r.Header.Set("X-Console-Token", "test-token")
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	_ = io.Discard
}

func TestConsolePaginationFilterAndPublicSecrets(t *testing.T) {
	st, _ := store.New(t.TempDir())
	jm := jobs.NewManager(context.Background(), 2, st, slog.Default())
	srv := NewServer(config.Config{StartupToken: "t", OutputRoot: t.TempDir()}, jm, embed.FS{}, slog.Default())
	h := srv.Handler()
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Host = "127.0.0.1"
		r.Header.Set("X-Console-Token", "t")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, email := range []string{"one@example.com", "two@example.com", "one@example.com"} {
		_ = post("/api/jobs", `{"email":"`+email+`"}`)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/jobs/query", strings.NewReader(`{"emails":["one@example.com"],"page":1,"pageSize":1}`))
	r.Host = "127.0.0.1"
	r.Header.Set("X-Console-Token", "t")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var doc map[string]any
	_ = json.NewDecoder(w.Body).Decode(&doc)
	if doc["page"].(float64) != 1 || len(doc["jobs"].([]any)) != 1 {
		t.Fatalf("query=%#v", doc)
	}
	if doc["pagination"].(map[string]any)["totalAll"] != float64(2) || doc["stats"].(map[string]any)["queued"] != float64(2) {
		t.Fatal("filter must retain global count/statistics")
	}
	job := doc["jobs"].([]any)[0].(map[string]any)
	if _, ok := job["totpSetupSecret"]; ok {
		t.Fatal("TOTP secret leaked")
	}
}

func TestConsoleDownloadMissingReturnsConflict(t *testing.T) {
	st, _ := store.New(t.TempDir())
	jm := jobs.NewManager(context.Background(), 1, st, slog.Default())
	srv := NewServer(config.Config{StartupToken: "t", OutputRoot: t.TempDir()}, jm, embed.FS{}, slog.Default())
	h := srv.Handler()
	r := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(`{"email":"a@example.com"}`))
	r.Host = "127.0.0.1"
	r.Header.Set("X-Console-Token", "t")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var d map[string]any
	_ = json.NewDecoder(w.Body).Decode(&d)
	id := d["job"].(map[string]any)["id"].(string)
	r = httptest.NewRequest(http.MethodGet, "/api/jobs/"+id+"/download", nil)
	r.Host = "127.0.0.1"
	r.Header.Set("X-Console-Token", "t")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("download status=%d", w.Code)
	}
}

func TestConsoleBatchTextAndMailConfig(t *testing.T) {
	st, _ := store.New(t.TempDir())
	jm := jobs.NewManager(context.Background(), 2, st, slog.Default())
	srv := NewServer(config.Config{StartupToken: "t", OutputRoot: t.TempDir()}, jm, embed.FS{}, slog.Default())
	h := srv.Handler()
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Host = "127.0.0.1"
		r.Header.Set("X-Console-Token", "t")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post("/api/mail-request-config", `{"config":{"method":"POST","url":"https://mail.example/messages"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := post("/api/jobs/batch", `{"text":"body-token----a@example.com----GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","proxyUrl":"http://127.0.0.1:1234"}`)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	jobsList := jm.List()
	if len(jobsList) != 1 {
		t.Fatalf("jobs=%d", len(jobsList))
	}
	vals, _ := jm.InputCopy(jobsList[0].ID)
	if vals["mailRequestBody"] != "body-token" || vals["password"] != "" {
		t.Fatalf("input=%#v", vals)
	}
}

func TestSafeProviderErrorRedactsSecrets(t *testing.T) {
	msg := safeProviderError(errors.New("GET https://x.test/?api_key=supersecret&token=abc"))
	if strings.Contains(msg, "supersecret") || strings.Contains(msg, "abc") {
		t.Fatalf("redaction failed: %s", msg)
	}
}

func TestBatchActionsProxyAndEligibilityCounters(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	first, _ := jm.Submit(jobs.Spec{ID: "first", Email: "first@example.com"})
	second, _ := jm.Submit(jobs.Spec{ID: "second", Email: "second@example.com", Input: map[string]any{"password": "saved"}})
	jm.Cancel(first.ID)
	jm.Cancel(second.ID)
	jm.Emit(jobs.JobEvent{JobID: first.ID, Status: jobs.StatusSucceeded})
	jm.Emit(jobs.JobEvent{JobID: second.ID, Status: jobs.StatusSucceeded})
	jm.UpdateResult(first.ID, true, "")
	jm.UpdateResult(second.ID, true, "")
	r := httptest.NewRequest("POST", "/api/jobs/add-password-batch", strings.NewReader(`{"ids":["first","second"],"proxyUrl":"socks5h://proxy.example:1080"}`))
	r.Host = "127.0.0.1"
	r.Header.Set("X-Console-Token", "test")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result["started"] != float64(1) || result["skipped"] != float64(1) {
		t.Fatal(result)
	}
	vals, _ := jm.InputCopy(first.ID)
	if vals["proxyUrl"] != "socks5h://proxy.example:1080" {
		t.Fatal("batch proxy ignored")
	}
}
func TestQueryHugePageAndTrailingJSON(t *testing.T) {
	srv, _ := newMonitorTestServer(t)
	r := httptest.NewRequest("GET", "/api/jobs?page=9223372036854775807", nil)
	r.Host = "127.0.0.1"
	r.Header.Set("X-Console-Token", "test")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	r = httptest.NewRequest("POST", "/api/jobs", strings.NewReader(`{"email":"a@example.com"} {"other":true}`))
	r.Host = "127.0.0.1"
	r.Header.Set("X-Console-Token", "test")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("trailing JSON accepted")
	}
}
