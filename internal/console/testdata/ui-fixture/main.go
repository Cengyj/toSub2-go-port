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
	"strconv"
	"strings"
	"sync"
	"time"

	"tosub2-go-port/internal/accountexpiry"
)

func main() {
	assets := flag.String("assets", "web/dist", "built browser assets")
	pageSize := flag.Int("page-size", 20, "synthetic accounts per page")
	flag.Parse()
	if *pageSize < 1 {
		panic("page-size must be positive")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	var mu sync.Mutex
	offline := false
	hold, batchOutcome := "", "partial"
	expirationOutcome, infoOutcome := "success", "success"
	release := make(chan struct{})
	pending, canceled := map[string]int{}, map[string]int{}
	batchRequests := []map[string]any{}
	expirationRequests := []map[string]any{}
	requests := map[string]int{}
	cpaSettings := map[string]any{"baseUrl": "", "hasApiKey": false, "configured": false, "revision": 0, "credentialPersistence": "memory_only", "priority": 0, "weight": 1, "monitorEnabled": false, "excludedPlanTypes": []string{}, "intervalMinutes": 5, "cooldownMinutes": 5}
	cpaMonitor := map[string]any{"revision": 0, "credentialPersistence": "memory_only", "configurationError": "", "configured": false, "enabled": false, "running": false, "intervalMinutes": 5, "cooldownMinutes": 5, "excludedPlanTypes": []string{}, "lastResult": map[string]any{"checked": 0, "started": 0, "excludedPlan": 0, "busy": 0, "cooldown": 0, "ineligible": 0, "missing": 0, "failed": 0}}
	syncCPAMonitor := func() {
		for _, field := range []string{"revision", "configured", "excludedPlanTypes", "intervalMinutes", "cooldownMinutes"} {
			cpaMonitor[field] = cpaSettings[field]
		}
		cpaMonitor["enabled"] = cpaSettings["monitorEnabled"]
		cpaMonitor["nextCheckAt"] = ""
		if cpaSettings["monitorEnabled"] == true {
			cpaMonitor["nextCheckAt"] = time.Now().Add(time.Duration(cpaSettings["intervalMinutes"].(int)) * time.Minute).UTC().Format(time.RFC3339)
		}
	}
	monitor := map[string]any{"configured": false, "enabled": false, "running": false, "baseUrl": "", "groupIds": []any{}, "excludedPlanTypes": []string{}, "openaiRequestTimezone": "America/Los_Angeles", "credentialPersistence": "secure_or_environment", "intervalMinutes": 5, "cooldownMinutes": 5, "lastResult": map[string]any{"checked": 5, "started": 1, "excludedPlan": 2, "ineligible": 1}}
	poolMonitor := map[string]any{"target": "sub2api", "revision": 0, "enabled": false, "excludedPlanTypes": []string{}, "intervalMinutes": 5, "cooldownMinutes": 5}
	applyPoolMonitor := func() {
		isCPA := poolMonitor["target"] == "cpa"
		monitor["enabled"] = poolMonitor["enabled"] == true && !isCPA
		cpaSettings["monitorEnabled"] = poolMonitor["enabled"] == true && isCPA
		for _, field := range []string{"excludedPlanTypes", "intervalMinutes", "cooldownMinutes"} {
			monitor[field], cpaSettings[field] = poolMonitor[field], poolMonitor[field]
		}
		monitor["nextCheckAt"] = ""
		if monitor["enabled"] == true {
			monitor["nextCheckAt"] = time.Now().Add(time.Duration(poolMonitor["intervalMinutes"].(int)) * time.Minute).UTC().Format(time.RFC3339)
		}
		syncCPAMonitor()
	}
	publicPoolMonitor := func() map[string]any {
		selected := monitor
		if poolMonitor["target"] == "cpa" {
			selected = cpaMonitor
		}
		result := map[string]any{}
		for field, value := range selected {
			result[field] = value
		}
		for field, value := range poolMonitor {
			result[field] = value
		}
		result["configurationError"] = ""
		result["cpaRevision"] = cpaSettings["revision"]
		result["targets"] = map[string]any{"sub2api": map[string]any{"configured": monitor["configured"]}, "cpa": map[string]any{"configured": cpaSettings["configured"]}}
		return result
	}
	jobs := []map[string]any{}
	for i, state := range []string{"completed", "imported", "completed", "resume_available", "failed", "completed"} {
		email := fmt.Sprintf("synthetic-%d@example.invalid", i+1)
		if i == 0 {
			email = "long.synthetic.account.for.layout.verification+no-real-data@extraordinarily-long-example-domain.invalid"
		}
		j := map[string]any{"id": fmt.Sprintf("fixture-%d", i+1), "email": email, "status": state, "authorizationMethod": "chatgpt", "planType": []string{"business_premium", "unknown", "prolite", "enterprise", "team", "pro"}[i], "createdAt": "2026-10-12T08:00:00Z", "lastOperationAt": "2026-10-13T03:00:00Z", "canForceRelogin": true, "canResetTotp": true, "canSetupTotp": true, "canRetry": true, "canRegenerate": true, "canAddPassword": true, "canDownload": state == "completed", "hasTotpKey": true, "hasPassword": true, "mailRequestMethod": "GET", "lastOperationType": "relogin", "operationStatus": "succeeded"}
		j["manualExpiresAt"], j["subscriptionExpiresAt"], j["expiresAt"], j["expiresAtSource"] = "", "", "", "unknown"
		switch i {
		case 0:
			j["manualExpiresAt"] = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
			j["subscriptionExpiresAt"] = time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
		case 2:
			j["subscriptionExpiresAt"] = time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
		case 3:
			j["manualExpiresAt"] = time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339)
		case 4:
			// This token expiry must never become an account subscription expiry.
			j["tokenExpiresAt"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		case 5:
			j["subscriptionExpiresAt"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		}
		if j["manualExpiresAt"] != "" {
			j["expiresAt"], j["expiresAtSource"] = j["manualExpiresAt"], "manual"
		} else if j["subscriptionExpiresAt"] != "" {
			j["expiresAt"], j["expiresAtSource"] = j["subscriptionExpiresAt"], "subscription"
		}
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
	insights := newInsightFixture()
	quality := newQualityFixture()
	quality.previewCanaryURL = "http://" + listener.Addr().String() + "/preview-escape-canary"
	previewCanaryRequests := 0
	trash := newTrashFixture()
	write := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/preview-escape-canary", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		previewCanaryRequests++
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/__fixture/preview-sandbox", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			write(w, http.StatusMethodNotAllowed, map[string]any{"error": "read-only synthetic preview state"})
			return
		}
		write(w, http.StatusOK, map[string]any{"canaryRequests": previewCanaryRequests, "manualOutcome": quality.manualOutcome})
	})
	mux.HandleFunc("/__fixture/state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			query := r.URL.Query()
			insights.configure(query)
			quality.configure(query)
			trash.configure(query)
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
			if query.Has("expiration") {
				expirationOutcome = query.Get("expiration")
			}
			if query.Has("info") {
				infoOutcome = query.Get("info")
			}
			if query.Has("operationRow1") {
				setTableOperation(jobs[0], query.Get("operationRow1"))
			}
			if mode := query.Get("accountRequests"); mode == "multi" || mode == "recovered" {
				at := time.Now().UTC().Format(time.RFC3339Nano)
				observations := []map[string]any{}
				for index, source := range []string{"subscription", "models", "candy", "pelican", "authentication", "refresh", "totp", "password", "logout"} {
					status := []int{403, 200, 429, 503, 401, 401, 502, 403, 302}[index]
					code := "http_error"
					if status == 200 {
						code = "forbidden"
					}
					observation := map[string]any{"source": source, "status": "failed", "httpStatus": status, "errorCode": code, "message": "合成账户请求诊断，仅用于验证显示；原有用量与授权记录保留。", "finishedAt": at}
					if mode == "recovered" && source == "subscription" {
						observation["status"], observation["httpStatus"], observation["errorCode"], observation["message"] = "success", 0, "", ""
					}
					observations = append(observations, observation)
				}
				jobs[0]["accountRequestObservations"] = observations
			}
			if mode := query.Get("accountRequests"); mode == "latest" || mode == "latest-success" {
				now := time.Now().UTC()
				observations := []map[string]any{
					{"source": "subscription", "status": "failed", "httpStatus": 503, "errorCode": "http_error", "message": "合成较早套餐请求", "finishedAt": now.Add(-3 * time.Second)},
					{"source": "models", "status": "failed", "httpStatus": 403, "errorCode": "forbidden", "message": "合成最新模型请求", "finishedAt": now.Add(-time.Second)},
				}
				if mode == "latest-success" {
					observations = append(observations, map[string]any{"source": "authentication", "status": "success", "httpStatus": 200, "finishedAt": now})
				}
				jobs[0]["accountRequestObservations"] = observations
			}
			if repair := query.Get("repair"); repair == "paused" || repair == "skipped" {
				job := jobs[0]
				job["planType"], job["lastOperationType"] = "free", "automatic_relogin"
				job["canDownload"], job["resultSaved"] = true, true
				monitor["configured"], monitor["enabled"] = true, true
				monitor["excludedPlanTypes"] = []string{"free"}
				poolMonitor["enabled"], poolMonitor["excludedPlanTypes"] = true, []string{"free"}
				if repair == "paused" {
					job["status"], job["operationStatus"] = "completed", "succeeded"
					job["autoRepairPendingAccountIds"] = []int{107}
					job["autoRepairSkippedReason"] = "合成新授权套餐 Free 命中排除，自动回传暂停"
					monitor["lastResult"] = map[string]any{"checked": 1, "autoUploadPaused": 1}
				} else {
					job["status"], job["operationStatus"], job["stage"] = "canceled", "skipped", "auto_repair_skipped"
					job["autoRepairPendingAccountIds"] = []int{}
					job["autoRepairSkippedReason"] = "合成本地套餐 Free 命中排除，队列未执行登录"
					monitor["lastResult"] = map[string]any{"checked": 1, "excludedPlan": 1}
				}
			}
		}
		write(w, 200, map[string]any{"offline": offline, "requests": requests, "hold": hold, "pending": pending, "canceled": canceled, "batchOutcome": batchOutcome, "batchRequests": batchRequests, "expirationOutcome": expirationOutcome, "infoOutcome": infoOutcome, "expirationRequests": expirationRequests, "insights": insights.state(), "quality": quality.state(), "trash": trash.state()})
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
		if strings.HasPrefix(r.URL.Path, "/api/account-trash") {
			readType = "trash"
			if r.Method != http.MethodGet {
				readType = "trash-action"
			}
		}
		if r.URL.Path == "/api/sub2api/options" {
			readType = "sub2"
		}
		if r.URL.Path == "/api/sms-providers/smsbower/options" {
			readType = "sms"
		}
		if strings.HasSuffix(r.URL.Path, "/expiration") {
			readType = "expiration"
		}
		if strings.HasPrefix(r.URL.Path, "/api/account-insights") && r.Method == http.MethodGet {
			readType = "insights"
		}
		if r.URL.Path == "/api/account-insights/refresh" && r.Method == http.MethodPost {
			readType = "insight-refresh"
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
		if trash.handle(w, r, &jobs, write) {
			return
		}
		if insights.handle(w, r, write) {
			return
		}
		if quality.handle(w, r, write) {
			return
		}
		switch r.URL.Path {
		case "/api/bootstrap":
			features := map[string]any{}
			features["accountInsights"] = true
			features["accountQuality"] = true
			features["accountTrash"] = true
			for _, name := range []string{"retry", "regenerate", "phoneContext", "batchDownload", "bulkActions", "batchImportOnly", "pagination", "uniqueEmail", "queue", "sourceExport", "cancelAll", "sub2apiUpload", "cpaUpload", "sub2apiMonitor", "totpSetup", "totpReset", "logoutAllDevicesAfterTotp", "passwordAdd", "forceRelogin", "reloginAutoUpload", "accountInfo"} {
				features[name] = true
			}
			features["smsProviders"] = []any{map[string]any{"id": "luban", "name": "合成接码平台", "description": "仅用于 UI 验收，不产生外部请求", "fields": []any{map[string]any{"key": "apiKey", "label": "API Key"}}}, map[string]any{"id": "smsbower", "name": "合成 SMSBower", "description": "仅查询内存合成价格", "optionsEndpoint": "/api/sms-providers/smsbower/options", "fields": []any{map[string]any{"key": "apiKey", "label": "API Key", "type": "password"}, map[string]any{"key": "country", "label": "国家与价格", "type": "price-select", "defaultValue": "999"}, map[string]any{"key": "maxPrice", "type": "hidden", "defaultValue": "0.1", "required": false}}}}
			write(w, 200, map[string]any{"token": "synthetic-console-token", "features": features})
		case "/api/jobs", "/api/jobs/query":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			filtered := jobs
			if r.URL.Path == "/api/jobs/query" {
				var query struct {
					Page   int      `json:"page"`
					Emails []string `json:"emails"`
				}
				if json.NewDecoder(r.Body).Decode(&query) != nil {
					write(w, 400, map[string]any{"error": "invalid fixture filter"})
					return
				}
				page = query.Page
				if len(query.Emails) > 0 {
					filtered = []map[string]any{}
					for _, job := range jobs {
						for _, email := range query.Emails {
							if strings.EqualFold(email, job["email"].(string)) {
								filtered = append(filtered, job)
								break
							}
						}
					}
				}
			}
			totalPages := max(1, (len(filtered)+*pageSize-1) / *pageSize)
			page = min(max(1, page), totalPages)
			start := (page - 1) * *pageSize
			activeCount, queuedCount, completedCount, availableCount := 0, 0, 0, 0
			for _, job := range jobs {
				if job["status"] == "queued" {
					queuedCount++
				}
				if job["operationStatus"] == "running" {
					activeCount++
				}
				if job["status"] == "completed" {
					completedCount++
				}
				if job["canDownload"] == true {
					availableCount++
				}
			}
			write(w, 200, map[string]any{"jobs": filtered[start:min(start+*pageSize, len(filtered))], "selection": filtered, "pagination": map[string]any{"page": page, "pageSize": *pageSize, "total": len(filtered), "totalAll": len(jobs), "totalPages": totalPages}, "stats": map[string]any{"active": activeCount, "queued": queuedCount, "completed": completedCount, "available": availableCount}})
		case "/api/mail-request-config":
			write(w, 200, map[string]any{"ok": true})
		case "/api/pool-monitor":
			if r.Method == http.MethodPost {
				var body struct {
					Target            string    `json:"target"`
					Revision          *int      `json:"revision"`
					Enabled           *bool     `json:"enabled"`
					ExcludedPlanTypes *[]string `json:"excludedPlanTypes"`
					IntervalMinutes   *int      `json:"intervalMinutes"`
					CooldownMinutes   *int      `json:"cooldownMinutes"`
				}
				decoder := json.NewDecoder(r.Body)
				decoder.DisallowUnknownFields()
				if decoder.Decode(&body) != nil || body.Target != "sub2api" && body.Target != "cpa" || body.Revision == nil || body.Enabled == nil || body.ExcludedPlanTypes == nil || body.IntervalMinutes == nil || body.CooldownMinutes == nil || *body.IntervalMinutes < 1 || *body.IntervalMinutes > 1440 || *body.CooldownMinutes < 1 || *body.CooldownMinutes > 1440 {
					write(w, 400, map[string]any{"error": "invalid synthetic shared monitor policy", "outcome": "failed", "uncertain": false})
					return
				}
				if *body.Revision != poolMonitor["revision"].(int) {
					write(w, 409, map[string]any{"error": "synthetic shared monitor revision conflict", "outcome": "failed", "uncertain": false})
					return
				}
				configured := monitor["configured"] == true
				if body.Target == "cpa" {
					configured = cpaSettings["configured"] == true
				}
				if *body.Enabled && !configured {
					write(w, 400, map[string]any{"error": "请先配置合成服务连接", "outcome": "failed", "uncertain": false})
					return
				}
				poolMonitor["target"], poolMonitor["revision"], poolMonitor["enabled"] = body.Target, *body.Revision+1, *body.Enabled
				poolMonitor["excludedPlanTypes"], poolMonitor["intervalMinutes"], poolMonitor["cooldownMinutes"] = *body.ExcludedPlanTypes, *body.IntervalMinutes, *body.CooldownMinutes
				applyPoolMonitor()
			}
			write(w, 200, publicPoolMonitor())
		case "/api/pool-monitor/check":
			var body struct {
				Revision *int `json:"revision"`
			}
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil || body.Revision == nil || *body.Revision != poolMonitor["revision"].(int) || poolMonitor["enabled"] != true {
				write(w, 409, map[string]any{"error": "请先保存当前合成巡检配置", "outcome": "failed", "uncertain": false})
				return
			}
			selected := monitor
			if poolMonitor["target"] == "cpa" {
				selected = cpaMonitor
			}
			if selected["configured"] != true {
				write(w, 400, map[string]any{"error": "请先配置合成服务连接", "outcome": "failed", "uncertain": false})
				return
			}
			selected["lastCheckAt"] = time.Now().UTC().Format(time.RFC3339)
			if poolMonitor["target"] == "cpa" {
				selected["lastResult"] = map[string]any{"checked": 6, "started": 1, "excludedPlan": 1, "busy": 1, "cooldown": 1, "ineligible": 1, "missing": 1, "failed": 0}
			}
			write(w, 200, publicPoolMonitor())
		case "/api/sub2api/monitor":
			if r.Method == http.MethodPost {
				var body struct {
					Enabled           *bool          `json:"enabled"`
					ExcludedPlanTypes *[]string      `json:"excludedPlanTypes"`
					Config            map[string]any `json:"config"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					write(w, 400, map[string]any{"error": "invalid fixture config"})
					return
				}
				for _, field := range []string{"baseUrl", "groupIds", "excludedPlanTypes", "openaiRequestTimezone"} {
					if value, ok := body.Config[field]; ok {
						monitor[field] = value
					}
				}
				monitor["configured"] = monitor["baseUrl"] != ""
				if body.Enabled != nil {
					if *body.Enabled && monitor["configured"] != true {
						write(w, 400, map[string]any{"error": "请先配置合成 Sub2API 连接", "outcome": "failed", "uncertain": false})
						return
					}
					monitor["enabled"] = *body.Enabled
				}
				if body.ExcludedPlanTypes != nil {
					monitor["excludedPlanTypes"] = *body.ExcludedPlanTypes
				}
				monitor["nextCheckAt"] = ""
				if monitor["enabled"] == true {
					monitor["nextCheckAt"] = time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
				}
			}
			write(w, 200, monitor)
		case "/api/sub2api/monitor/check":
			if r.Method != http.MethodPost || monitor["enabled"] != true {
				write(w, 409, map[string]any{"error": "请先启用合成 Sub2API 巡检", "outcome": "failed", "uncertain": false})
				return
			}
			monitor["lastCheckAt"] = time.Now().UTC().Format(time.RFC3339)
			monitor["result"] = monitor["lastResult"]
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
		case "/api/cpa/settings":
			if r.Method == http.MethodPost {
				var body struct {
					BaseURL           string    `json:"baseUrl"`
					AdminAPIKey       string    `json:"adminApiKey"`
					Revision          int       `json:"revision"`
					Priority          int       `json:"priority"`
					Weight            int       `json:"weight"`
					MonitorEnabled    *bool     `json:"monitorEnabled"`
					ExcludedPlanTypes *[]string `json:"excludedPlanTypes"`
					IntervalMinutes   *int      `json:"intervalMinutes"`
					CooldownMinutes   *int      `json:"cooldownMinutes"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					write(w, 400, map[string]any{"error": "invalid synthetic settings"})
					return
				}
				if body.Revision != cpaSettings["revision"].(int) {
					write(w, 409, map[string]any{"error": "synthetic revision conflict"})
					return
				}
				cpaSettings["baseUrl"], cpaSettings["hasApiKey"], cpaSettings["configured"], cpaSettings["revision"] = body.BaseURL, body.BaseURL != "", body.BaseURL != "", body.Revision+1
				cpaSettings["priority"], cpaSettings["weight"] = body.Priority, body.Weight
				if body.MonitorEnabled != nil {
					cpaSettings["monitorEnabled"] = *body.MonitorEnabled
				}
				if body.BaseURL == "" {
					cpaSettings["monitorEnabled"] = false
				}
				if body.ExcludedPlanTypes != nil {
					cpaSettings["excludedPlanTypes"] = *body.ExcludedPlanTypes
				}
				if body.IntervalMinutes != nil {
					cpaSettings["intervalMinutes"] = *body.IntervalMinutes
				}
				if body.CooldownMinutes != nil {
					cpaSettings["cooldownMinutes"] = *body.CooldownMinutes
				}
				syncCPAMonitor()
			}
			write(w, 200, cpaSettings)
		case "/api/cpa/monitor":
			if r.Method == http.MethodPost {
				var body struct {
					Revision          *int      `json:"revision"`
					Enabled           *bool     `json:"enabled"`
					ExcludedPlanTypes *[]string `json:"excludedPlanTypes"`
					IntervalMinutes   *int      `json:"intervalMinutes"`
					CooldownMinutes   *int      `json:"cooldownMinutes"`
				}
				decoder := json.NewDecoder(r.Body)
				decoder.DisallowUnknownFields()
				if decoder.Decode(&body) != nil || body.Revision == nil || body.Enabled == nil || body.ExcludedPlanTypes == nil || body.IntervalMinutes == nil || body.CooldownMinutes == nil || *body.IntervalMinutes < 1 || *body.IntervalMinutes > 1440 || *body.CooldownMinutes < 1 || *body.CooldownMinutes > 1440 {
					write(w, 400, map[string]any{"error": "invalid synthetic monitor policy", "outcome": "failed", "uncertain": false})
					return
				}
				if *body.Revision != cpaSettings["revision"].(int) {
					write(w, 409, map[string]any{"error": "synthetic revision conflict", "outcome": "failed", "uncertain": false})
					return
				}
				if *body.Enabled && cpaSettings["configured"] != true {
					write(w, 400, map[string]any{"error": "请先配置合成 CPA 连接", "outcome": "failed", "uncertain": false})
					return
				}
				cpaSettings["revision"], cpaSettings["monitorEnabled"] = *body.Revision+1, *body.Enabled
				cpaSettings["excludedPlanTypes"], cpaSettings["intervalMinutes"], cpaSettings["cooldownMinutes"] = *body.ExcludedPlanTypes, *body.IntervalMinutes, *body.CooldownMinutes
				syncCPAMonitor()
			}
			write(w, 200, cpaMonitor)
		case "/api/cpa/monitor/check":
			if r.Method != http.MethodPost || cpaMonitor["enabled"] != true {
				write(w, 400, map[string]any{"error": "请先启用并保存合成监控配置", "outcome": "failed", "uncertain": false})
				return
			}
			cpaMonitor["lastCheckAt"] = time.Now().UTC().Format(time.RFC3339)
			cpaMonitor["lastResult"] = map[string]any{"checked": 6, "started": 1, "excludedPlan": 1, "busy": 1, "cooldown": 1, "ineligible": 1, "missing": 1, "failed": 0}
			write(w, 200, cpaMonitor)
		case "/api/cpa/upload":
			var body struct {
				IDs []string `json:"ids"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				write(w, 400, map[string]any{"error": "invalid synthetic upload"})
				return
			}
			results := []map[string]any{}
			for _, id := range body.IDs {
				results = append(results, map[string]any{"id": id, "outcome": "succeeded"})
			}
			write(w, 200, map[string]any{"ok": true, "target": "cpa", "selected": len(body.IDs), "uploaded": len(body.IDs), "skipped": 0, "result": map[string]any{"success": len(body.IDs), "created": 0, "updated": 0}, "results": results})
		case "/api/sub2api/upload":
			write(w, 502, map[string]any{"ok": false, "error": "Synthetic remote outcome unknown", "outcome": "partial", "uncertain": true, "selected": 3, "uploaded": 3, "skipped": 0, "result": map[string]any{"created": 0, "updated": 1, "unknown": 1, "not_attempted": 1}})
		default:
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/jobs/"), "/")
			if strings.HasPrefix(r.URL.Path, "/api/jobs/") && len(parts) <= 2 {
				for _, job := range jobs {
					if job["id"] != parts[0] {
						continue
					}
					if len(parts) == 1 && r.Method == http.MethodGet {
						write(w, 200, job)
						return
					}
					if len(parts) == 2 && parts[1] == "account-info" && r.Method == http.MethodGet {
						if infoOutcome != "success" || parts[0] == "fixture-5" {
							write(w, 409, map[string]any{"error": "合成凭据暂不可读；到期资料仍可独立编辑"})
							return
						}
						info := map[string]any{}
						for key, value := range job {
							info[key] = value
						}
						info["password"], info["totpSecret"] = "synthetic-password-only", "JBSWY3DPEHPK3PXP"
						write(w, 200, info)
						return
					}
					if len(parts) == 2 && parts[1] == "expiration" && r.Method == http.MethodPost {
						var body struct {
							ExpiresAt *string `json:"expiresAt"`
							TimeZone  string  `json:"timeZone"`
						}
						if json.NewDecoder(r.Body).Decode(&body) != nil || body.ExpiresAt == nil {
							write(w, 400, map[string]any{"error": "合成到期时间请求缺少明确值"})
							return
						}
						expirationRequests = append(expirationRequests, map[string]any{"id": parts[0], "expiresAt": *body.ExpiresAt, "timeZone": body.TimeZone})
						if expirationOutcome == "failed" {
							write(w, 409, map[string]any{"error": "合成磁盘写入失败，已保留原值"})
							return
						}
						expires := ""
						if *body.ExpiresAt != "" {
							parsed, err := accountexpiry.ParseManual(*body.ExpiresAt, body.TimeZone, time.Now())
							if err != nil {
								write(w, 400, map[string]any{"error": err.Error()})
								return
							}
							expires = parsed.Format(time.RFC3339Nano)
						}
						job["manualExpiresAt"], job["expiresAt"], job["expiresAtSource"] = expires, "", "unknown"
						if expires != "" {
							job["expiresAt"], job["expiresAtSource"] = expires, "manual"
						} else if job["subscriptionExpiresAt"] != "" {
							job["expiresAt"], job["expiresAtSource"] = job["subscriptionExpiresAt"], "subscription"
						}
						if expirationOutcome == "unknown" {
							write(w, 503, map[string]any{"error": "合成目录同步结果未知", "uncertain": true})
							return
						}
						write(w, 200, map[string]any{"job": job})
						return
					}
				}
			}
			if strings.HasSuffix(r.URL.Path, "/logs") {
				trash.writeLogs(w, r, parts[0], write)
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
