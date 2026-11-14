package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/console"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

// The authentication side is the real HTTP/Sentinel/OAuth fixture. This adds
// the actual console import/input/download/upload consumers around its runner.
func runConsoleLoginUploadFixture(t *testing.T, cfg config.Config, manager *jobs.Manager, runner jobs.Runner, vault credentials.Store, email string) {
	t.Helper()
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	local := httptest.NewUnstartedServer(nil)
	address := local.Listener.Addr().(*net.TCPAddr)
	cfg.Host, cfg.Port = address.IP.String(), address.Port
	cfg.StartupToken = "synthetic-console-token"
	app := console.NewServer(cfg, manager, staticFiles, nil, vault)
	defer app.Close()
	manager.SetRunner(runner)
	manager.Start()
	local.Config.Handler = app.Handler()
	local.Start()
	defer local.Close()
	request := func(method, path string, body any, want int) []byte {
		t.Helper()
		var data []byte
		if body != nil {
			var err error
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequestWithContext(t.Context(), method, local.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Console-Token", cfg.StartupToken)
		req.Header.Set("Origin", local.URL)
		req.Header.Set("Content-Type", "application/json")
		resp, err := local.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err = io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("local %s %s: status=%d err=%v body=%s", method, path, resp.StatusCode, err, data)
		}
		return data
	}
	request("POST", "/api/jobs/batch", map[string]any{"text": email}, 201)
	list := manager.List()
	if len(list) != 1 || list[0].Status != "imported" {
		t.Fatal("HTTP import started authentication")
	}
	id := list[0].ID
	request("POST", "/api/jobs/"+id+"/relogin", map[string]any{}, 200)
	deadline := time.Now().Add(20 * time.Second)
	for {
		job, _ := manager.Get(id)
		if job.Status == "email_otp" {
			break
		}
		if job.FinishedAt != nil || time.Now().After(deadline) {
			t.Fatalf("fresh login did not request input: %s %s", job.Status, job.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	request("POST", "/api/jobs/"+id+"/input", map[string]any{"action": "email_otp", "value": "123456"}, 200)
	request("POST", "/api/jobs/"+id+"/input", map[string]any{"action": "email_otp", "value": "123456"}, 409)
	for {
		job, _ := manager.Get(id)
		if job.FinishedAt != nil {
			if !job.ResultSaved || job.OperationStatus != "succeeded" {
				t.Fatalf("OAuth result not saved: %s", job.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("OAuth completion timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	saved, err := os.ReadFile(filepath.Join(cfg.OutputRoot, id, "sub2api-import-oauth.json"))
	if err != nil || !bytes.Contains(saved, []byte("fixture-refresh")) {
		t.Fatal("committed grant missing")
	}
	assertNoTaskCheckpoint(t, filepath.Join(cfg.OutputRoot, id, "login-checkpoint.json"))
	public := request("GET", "/api/jobs/"+id, nil, 200)
	for _, secret := range []string{"fixture-refresh", "fixture-access", "fixture-session", "123456"} {
		if bytes.Contains(public, []byte(secret)) {
			t.Fatal("public projection exposed authentication material")
		}
	}
	if downloaded := request("GET", "/api/jobs/"+id+"/download", nil, 200); !bytes.Contains(downloaded, []byte("fixture-refresh")) {
		t.Fatal("authorized download lost grant")
	}
	// The default quality policy is disabled; its gate must pass without
	// creating model traffic. Enabled/blocked policy is covered by real
	// application + console gate fixtures with independent request counters.
	var policy struct{ Enabled, BeforeUpload bool }
	if json.Unmarshal(request("GET", "/api/account-quality/settings", nil, 200), &policy) != nil || policy.Enabled || policy.BeforeUpload {
		t.Fatal("fixture unexpectedly enabled model traffic")
	}
	var mu sync.Mutex
	var calls []string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/admin/accounts":
			io.WriteString(w, `{"data":{"items":[],"pages":1,"total":0}}`)
		case "POST /api/v1/admin/accounts/batch":
			var payload struct {
				Accounts []struct {
					GroupIDs    []int `json:"group_ids"`
					ProxyID     *int  `json:"proxy_id"`
					Concurrency *int  `json:"concurrency"`
					LoadFactor  *int  `json:"load_factor"`
					Priority    *int  `json:"priority"`
				} `json:"accounts"`
			}
			encoded, err := io.ReadAll(r.Body)
			if err != nil || json.Unmarshal(encoded, &payload) != nil || len(payload.Accounts) != 1 {
				t.Error("invalid numeric upload fields")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			account := payload.Accounts[0]
			if !reflect.DeepEqual(account.GroupIDs, []int{7}) || account.Concurrency == nil || *account.Concurrency != 2 {
				t.Error("browser upload options were not preserved")
			}
			if !bytes.Contains(encoded, []byte("fixture-refresh")) || bytes.Contains(encoded, []byte("fixture-session")) {
				t.Error("upload changed committed grant boundary")
			}
			io.WriteString(w, `{"created":1}`)
		default:
			t.Errorf("unexpected downstream mutation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	defer remote.Close()
	// Match the browser's saved text options, including its blank defaults.
	result := request("POST", "/api/sub2api/upload", map[string]any{"ids": []string{id}, "config": map[string]any{
		"baseUrl": remote.URL, "adminApiKey": "synthetic-admin", "groupIds": []string{"7"},
		"proxyId": "", "concurrency": "2", "loadFactor": "", "priority": "",
	}}, 200)
	var receipt struct {
		OK       bool
		Uploaded int
	}
	if json.Unmarshal(result, &receipt) != nil || !receipt.OK || receipt.Uploaded != 1 {
		t.Fatalf("upload receipt not successful: %s", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"GET /api/v1/admin/accounts", "POST /api/v1/admin/accounts/batch"}) {
		t.Fatalf("remote actions repeated: %v", calls)
	}
}
