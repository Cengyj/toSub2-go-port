// UI fixture serves synthetic data only; it cannot contact upstream services.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func main() {
	assets := flag.String("assets", "web/dist", "built browser assets")
	flag.Parse()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	var mu sync.Mutex
	offline := false
	hold, batchOutcome := "", "partial"
	release := make(chan struct{})
	pending, canceled := map[string]int{}, map[string]int{}
	batchRequests := []map[string]any{}
	requests := map[string]int{}
	monitor := map[string]any{"configured": false, "enabled": false, "running": false, "intervalMinutes": 5, "lastResult": map[string]any{"checked": 5, "started": 1, "excludedPlan": 2, "ineligible": 1}}
	jobs := []map[string]any{}
	for i, state := range []string{"completed", "imported", "completed", "resume_available", "failed", "completed"} {
		email := fmt.Sprintf("synthetic-%d@example.invalid", i+1)
		if i == 0 {
			email = "long.synthetic.account.for.layout.verification+no-real-data@extraordinarily-long-example-domain.invalid"
		}
		j := map[string]any{"id": fmt.Sprintf("fixture-%d", i+1), "email": email, "status": state, "authorizationMethod": "chatgpt", "planType": []string{"business_premium", "unknown", "prolite", "enterprise", "team", "pro"}[i], "createdAt": "2026-10-12T08:00:00Z", "lastOperationAt": "2026-10-13T03:00:00Z", "canForceRelogin": true, "canResetTotp": true, "canSetupTotp": true, "canRetry": true, "canRegenerate": true, "canAddPassword": true, "canDownload": state == "completed", "hasTotpKey": true, "hasPassword": true, "mailRequestMethod": "GET", "lastOperationType": "relogin", "operationStatus": "succeeded"}
		if i == 1 {
			j["lastOperationType"] = "account_import"
		}
		if i == 2 {
			j["authorizationMethod"] = "bps"
			j["operationStatus"] = "failed"
			j["operationError"] = strings.Repeat("Synthetic network interruption; previous authorization remains available. ", 5)
		}
		if i == 3 {
			j["authorizationMethod"] = "bps"
			j["operationStatus"] = "interrupted"
			j["canResume"] = true
		}
		if i == 4 {
			j["lastOperationType"] = "reset_2fa"
			j["operationStatus"] = "failed"
			j["totpWorkflowStatus"] = "failed"
			j["totpWorkflowStep"] = "logout"
			j["totpResetPhase"] = "credential_persisted"
			j["logoutAllDevicesAfterTotp"] = true
			j["logoutAllDevicesStatus"] = "unknown"
			j["reloginAfterTotp"] = true
			j["totpWorkflowError"] = "Synthetic device logout outcome unknown; verify before continuing"
		}
		jobs = append(jobs, j)
	}
	write := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/__fixture/state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			query := r.URL.Query()
			if query.Has("offline") {
				offline = query.Get("offline") == "true"
			}
			if query.Has("hold") {
				hold = query.Get("hold")
				close(release)
				release = make(chan struct{})
			}
			if query.Has("batch") {
				batchOutcome = query.Get("batch")
			}
		}
		write(w, 200, map[string]any{"offline": offline, "requests": requests, "hold": hold, "pending": pending, "canceled": canceled, "batchOutcome": batchOutcome, "batchRequests": batchRequests})
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests[r.Method+" "+r.URL.Path]++
		if offline {
			write(w, 503, map[string]any{"error": "Synthetic service unavailable"})
			return
		}
		readType := ""
		if r.URL.Path == "/api/sub2api/options" {
			readType = "sub2"
		}
		if r.URL.Path == "/api/sms-providers/smsbower/options" {
			readType = "sms"
		}
		delayed := readType != "" && hold == readType
		if delayed {
			unblock := release
			pending[readType]++
			mu.Unlock()
			select {
			case <-r.Context().Done():
				mu.Lock()
				pending[readType]--
				canceled[readType]++
				return
			case <-unblock:
				mu.Lock()
				pending[readType]--
			}
		}
		switch r.URL.Path {
		case "/api/bootstrap":
			features := map[string]any{}
			for _, name := range []string{"retry", "regenerate", "phoneContext", "batchDownload", "bulkActions", "batchImportOnly", "pagination", "uniqueEmail", "queue", "sourceExport", "cancelAll", "sub2apiUpload", "sub2apiMonitor", "totpSetup", "totpReset", "logoutAllDevicesAfterTotp", "passwordAdd", "forceRelogin", "reloginAutoUpload", "accountInfo"} {
				features[name] = true
			}
			features["smsProviders"] = []any{map[string]any{"id": "luban", "name": "合成接码平台", "description": "仅用于 UI 验收，不产生外部请求", "fields": []any{map[string]any{"key": "apiKey", "label": "API Key"}}}, map[string]any{"id": "smsbower", "name": "合成 SMSBower", "description": "仅查询内存合成价格", "optionsEndpoint": "/api/sms-providers/smsbower/options", "fields": []any{map[string]any{"key": "apiKey", "label": "API Key", "type": "password"}, map[string]any{"key": "country", "label": "国家与价格", "type": "price-select", "defaultValue": "999"}, map[string]any{"key": "maxPrice", "type": "hidden", "defaultValue": "0.1", "required": false}}}}
			write(w, 200, map[string]any{"token": "synthetic-console-token", "features": features})
		case "/api/jobs", "/api/jobs/query":
			write(w, 200, map[string]any{"jobs": jobs, "selection": jobs, "pagination": map[string]any{"page": 1, "pageSize": 20, "total": len(jobs), "totalPages": 1}, "stats": map[string]any{"active": 0, "queued": 0, "completed": 3, "available": 3}})
		case "/api/mail-request-config":
			write(w, 200, map[string]any{"ok": true})
		case "/api/sub2api/monitor":
			if r.Method == http.MethodPost {
				var body struct {
					Enabled bool           `json:"enabled"`
					Config  map[string]any `json:"config"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					write(w, 400, map[string]any{"error": "invalid fixture config"})
					return
				}
				monitor["configured"], monitor["enabled"], monitor["config"] = true, body.Enabled, body.Config
			}
			write(w, 200, monitor)
		case "/api/sub2api/options":
			name := "Synthetic current group"
			if delayed {
				name = "Synthetic stale delayed group"
			}
			write(w, 200, map[string]any{"groups": []any{map[string]any{"id": 7, "name": name}}, "proxies": []any{}})
		case "/api/sms-providers/smsbower/options":
			name := "Synthetic current country"
			if delayed {
				name = "Synthetic stale delayed country"
			}
			write(w, 200, map[string]any{"options": []any{map[string]any{"country": "999", "name": name, "price": 0.1, "count": 10}}})
		case "/api/jobs/relogin-batch", "/api/jobs/setup-2fa-batch", "/api/jobs/add-password-batch":
			var body struct {
				IDs []string `json:"ids"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.IDs) == 0 {
				write(w, 400, map[string]any{"error": "empty synthetic batch"})
				return
			}
			batchRequests = append(batchRequests, map[string]any{"path": r.URL.Path, "ids": body.IDs})
			queued, failed, unknown, skipped := 0, 0, 0, 0
			accepted, failures := []any{}, []any{}
			for index, id := range body.IDs {
				if batchOutcome == "unknown" {
					unknown++
					failures = append(failures, map[string]any{"id": id, "error": "合成目录同步结果未知，请先核实", "uncertain": true})
					continue
				}
				switch index {
				case 0:
					queued++
					accepted = append(accepted, map[string]any{"id": id})
					for _, job := range jobs {
						if job["id"] == id {
							job["status"] = "queued"
							job["operationStatus"] = "queued"
							job["canForceRelogin"] = false
							job["canResetTotp"] = false
							job["canAddPassword"] = false
						}
					}
				case 1:
					failed++
					failures = append(failures, map[string]any{"id": id, "error": "合成任务写入失败"})
				case 2:
					unknown++
					failures = append(failures, map[string]any{"id": id, "error": "合成目录同步结果未知，请先核实", "uncertain": true})
				default:
					skipped++
				}
			}
			status, outcome := 200, "partial"
			if batchOutcome == "unknown" {
				status, outcome = 409, "unknown"
			}
			write(w, status, map[string]any{"ok": false, "selected": len(body.IDs), "queued": queued, "started": queued, "failed": failed, "unknown": unknown, "skipped": skipped, "uncertain": unknown > 0, "outcome": outcome, "jobs": accepted, "failures": failures, "error": "合成批量提交未完全确认"})
		case "/api/sub2api/upload":
			write(w, 502, map[string]any{"ok": false, "error": "Synthetic remote outcome unknown", "outcome": "partial", "uncertain": true, "selected": 3, "uploaded": 3, "skipped": 0, "result": map[string]any{"created": 0, "updated": 1, "unknown": 1, "not_attempted": 1}})
		default:
			if strings.HasSuffix(r.URL.Path, "/logs") {
				write(w, 200, map[string]any{"logs": "2026-10-13T03:00:00Z [info] Synthetic operation saved\n2026-10-13T03:01:00Z [warning] Synthetic remote outcome requires verification", "entries": []any{}})
				return
			}
			write(w, 200, map[string]any{"ok": true, "started": 1, "skipped": 0})
		}
	})
	files := http.FileServer(http.Dir(*assets))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && r.URL.Query().Get("fixture-storage-fail") == "1" {
			html, err := os.ReadFile(filepath.Join(*assets, "index.html"))
			if err != nil {
				http.Error(w, "missing synthetic assets", 500)
				return
			}
			// Explicit test fault injection affects only this disposable origin.
			fault := `<script>const originalSetItem=Storage.prototype.setItem;Storage.prototype.setItem=function(key,value){if(key==='chatgpt-onboarding.account-proxy-v1')throw new DOMException('Synthetic storage failure','QuotaExceededError');return originalSetItem.call(this,key,value);};</script>`
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(strings.Replace(string(html), "<head>", "<head>"+fault, 1)))
			return
		}
		files.ServeHTTP(w, r)
	})
	fmt.Printf("Synthetic UI fixture: http://%s/\n", listener.Addr())
	if err := http.Serve(listener, mux); err != nil {
		panic(err)
	}
}
