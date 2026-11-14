package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// This fixture contains only generated observations and never contacts a model.
// Its methods run under the fixture server's shared mutex.
type qualityFixture struct {
	settings                      map[string]any
	snapshots                     map[string]map[string]any
	runs                          map[string]time.Time
	runOptions                    map[string]map[string]string
	modelsEmpty, modelsError      bool
	hold, readError, responseLost bool
	qualityBlocked                bool
	outcome                       string
	revision, sequence            int
	requests                      []map[string]any
	manual                        map[string]map[string]any
	manualRuns                    map[string]time.Time
	manualHold                    bool
	manualOutcome                 string
	previewCanaryURL              string
}

func newQualityFixture() *qualityFixture {
	q := &qualityFixture{settings: map[string]any{"schemaVersion": 1, "revision": 1, "enabled": true, "beforeUpload": false, "blockDegraded": true, "blockFailed": true, "model": "synthetic-reasoning-fixture-only", "reasoningEffort": "high", "minimumScore": 1, "timeoutSeconds": 90, "cooldownSeconds": 60, "resultTTLSeconds": 900}, snapshots: map[string]map[string]any{}, runs: map[string]time.Time{}, runOptions: map[string]map[string]string{}, outcome: "normal", revision: 1, requests: []map[string]any{}, manual: map[string]map[string]any{}, manualRuns: map[string]time.Time{}, manualOutcome: "success"}
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("fixture-%d", i)
		snapshot := map[string]any{"id": id, "state": "eligible", "canCheck": true, "stale": false, "persisted": true, "revision": 1, "lastAttempt": map[string]any{"checkId": "", "source": "manual", "status": "never"}}
		q.snapshots[id] = snapshot
		if i == 1 || i == 3 || i == 4 {
			verdict, score := "normal", 1
			if i == 3 {
				verdict, score = "degraded", 0
			}
			snapshot["lastResult"] = map[string]any{"model": "synthetic-model-with-a-very-long-identifier-for-layout-validation", "ruleVersion": "candy-v1", "verdict": verdict, "score": score, "total": 1, "checkedAt": time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339), "durationMs": 3200}
			snapshot["lastAttempt"] = map[string]any{"status": "success", "source": "manual", "checkId": "synthetic-initial", "finishedAt": time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)}
		}
		if i == 2 {
			snapshot["state"], snapshot["canCheck"], snapshot["unavailableReason"] = "unavailable", false, "合成导入账号尚无可用授权"
		}
		if i == 4 {
			// Explicit historical fixture: retain its original six-task scale.
			result := snapshot["lastResult"].(map[string]any)
			result["ruleVersion"], result["score"], result["total"] = "reasoning-six-v1", 6, 6
			snapshot["stale"], snapshot["state"], snapshot["canCheck"] = true, "unavailable", false
			snapshot["staleReason"] = "rule_changed"
			snapshot["unavailableReason"] = "合成恢复流程尚未完成"
			snapshot["lastAttempt"] = map[string]any{"status": "failed", "errorCode": "timeout", "message": strings.Repeat("合成网络超时；保留历史测量，不代表本次通过。", 8), "finishedAt": time.Now().UTC().Format(time.RFC3339)}
		}
		if i == 5 {
			snapshot["lastAttempt"] = map[string]any{"status": "failed", "errorCode": "forbidden", "httpStatus": 403, "message": "合成上游拒绝了本次请求，请核对账号权限和所选模型。", "finishedAt": time.Now().UTC().Format(time.RFC3339)}
		}
		if i == 6 {
			snapshot["lastAttempt"] = map[string]any{"status": "failed", "errorCode": "invalid_response", "httpStatus": 200, "message": "合成响应缺少有效题目结果，测量未知。", "finishedAt": time.Now().UTC().Format(time.RFC3339)}
		}
	}
	return q
}

func (q *qualityFixture) configure(values url.Values) {
	for key, target := range map[string]*bool{"qualityHold": &q.hold, "qualityReadError": &q.readError, "qualityResponseLost": &q.responseLost, "qualityBlocked": &q.qualityBlocked, "qualityManualHold": &q.manualHold, "qualityModelsEmpty": &q.modelsEmpty, "qualityModelsError": &q.modelsError} {
		if values.Has(key) {
			*target = values.Get(key) == "true"
		}
	}
	if values.Has("qualityEnabled") {
		q.settings["enabled"] = values.Get("qualityEnabled") == "true"
	}
	if values.Has("qualityOutcome") {
		q.outcome = values.Get("qualityOutcome")
	}
	if values.Has("qualityManualOutcome") {
		q.manualOutcome = values.Get("qualityManualOutcome")
	}
	if values.Has("qualityHistory") {
		q.configureHistory(values.Get("qualityHistory") == "true")
	}
	if phase := values.Get("qualityPhase"); phase != "" {
		for _, id := range []string{"fixture-1", "fixture-3"} {
			q.snapshots[id]["state"] = phase
			q.snapshots[id]["canCheck"] = phase == "eligible"
			status := map[string]string{"active": "running", "queued": "queued", "eligible": "success", "unavailable": "failed"}[phase]
			q.snapshots[id]["lastAttempt"] = map[string]any{"status": status, "checkId": "synthetic-phase", "source": "manual"}
		}
	}
}

// List-only samples share the existing latest measurement time. Re-enabling
// replaces this fixed set instead of appending synthetic probe executions.
func (q *qualityFixture) configureHistory(enabled bool) {
	snapshot := q.snapshots["fixture-1"]
	if !enabled {
		delete(snapshot, "probeHistory")
		return
	}
	result, _ := snapshot["lastResult"].(map[string]any)
	latest, err := time.Parse(time.RFC3339Nano, fmt.Sprint(result["checkedAt"]))
	if err != nil {
		return
	}
	model := fmt.Sprint(result["model"])
	history := []map[string]any{
		{"id": "synthetic-history-pass", "kind": "candy", "status": "success", "verdict": "normal", "score": 1, "total": 1},
		{"id": "synthetic-history-low", "kind": "candy", "status": "success", "verdict": "degraded", "score": 0, "total": 1},
		{"id": "synthetic-history-drawing", "kind": "pelican", "status": "success"},
		{"id": "synthetic-history-limited", "kind": "candy", "status": "failed", "errorCode": "rate_limited", "httpStatus": 429},
		{"id": "synthetic-history-latest", "kind": "candy", "status": "success", "verdict": "normal", "score": 1, "total": 1},
	}
	for index, entry := range history {
		entry["model"] = model
		entry["finishedAt"] = latest.Add(time.Duration(index-len(history)+1) * time.Minute).Format(time.RFC3339Nano)
	}
	snapshot["probeHistory"] = history
}

func (q *qualityFixture) tick() {
	q.tickManual()
	if q.hold {
		return
	}
	for id, started := range q.runs {
		snapshot := q.snapshots[id]
		if time.Since(started) < 2*time.Second {
			continue
		}
		if time.Since(started) < 6*time.Second {
			snapshot["state"] = "active"
			snapshot["lastAttempt"].(map[string]any)["status"] = "running"
			continue
		}
		delete(q.runs, id)
		now := time.Now().UTC().Format(time.RFC3339)
		if q.outcome == "failed" || q.outcome == "unsupported" {
			code := "timeout"
			if q.outcome == "unsupported" {
				code = "unsupported"
			}
			snapshot["lastAttempt"] = map[string]any{"status": "failed", "errorCode": code, "message": "合成检测失败，历史测量仍保留。", "finishedAt": now}
		} else {
			score, verdict := 1, "normal"
			if q.outcome == "degraded" {
				score, verdict = 0, "degraded"
			}
			snapshot["lastResult"] = map[string]any{"model": q.runOptions[id]["model"], "ruleVersion": "candy-v1", "verdict": verdict, "score": score, "total": 1, "checkedAt": now, "durationMs": 6000}
			snapshot["lastAttempt"] = map[string]any{"status": "success", "source": "manual", "finishedAt": now}
			snapshot["stale"] = false
			delete(snapshot, "staleReason")
			if selected := q.runOptions[id]; selected["model"] != fmt.Sprint(q.settings["model"]) || selected["reasoningEffort"] != fmt.Sprint(q.settings["reasoningEffort"]) {
				snapshot["stale"], snapshot["staleReason"] = true, "options_changed"
			}
		}
		snapshot["state"], snapshot["canCheck"] = "eligible", true
		q.revision++
		snapshot["revision"] = q.revision
	}
}

func (q *qualityFixture) status() map[string]any {
	active, queued := 0, 0
	for _, snapshot := range q.snapshots {
		if snapshot["state"] == "active" {
			active++
		}
		if snapshot["state"] == "queued" {
			queued++
		}
	}
	active += len(q.manualRuns)
	return map[string]any{"enabled": q.settings["enabled"], "beforeUpload": q.settings["beforeUpload"], "running": true, "queued": queued, "active": active, "settingsAvailable": true, "revision": q.revision, "settingsRevision": q.settings["revision"]}
}
func (q *qualityFixture) state() map[string]any {
	return map[string]any{"settings": q.settings, "status": q.status(), "requests": q.requests, "hold": q.hold, "outcome": q.outcome, "qualityBlocked": q.qualityBlocked, "manual": q.manual, "manualHold": q.manualHold, "manualOutcome": q.manualOutcome}
}

func (q *qualityFixture) handle(w http.ResponseWriter, r *http.Request, write func(http.ResponseWriter, int, any)) bool {
	if q.qualityBlocked && r.URL.Path == "/api/sub2api/upload" && r.Method == http.MethodPost {
		var value struct {
			IDs []string `json:"ids"`
		}
		if json.NewDecoder(r.Body).Decode(&value) != nil {
			write(w, 400, map[string]any{"error": "invalid synthetic upload request"})
			return true
		}
		selected := len(value.IDs)
		q.requests = append(q.requests, map[string]any{"path": r.URL.Path, "ids": value.IDs, "qualityBlocked": true})
		write(w, http.StatusConflict, map[string]any{"ok": false, "outcome": "blocked", "qualityBlocked": true, "uncertain": false, "selected": selected, "uploaded": 0, "skipped": selected, "result": map[string]any{"success": 0, "created": 0, "updated": 0}, "error": "合成模型检测未达到上传要求；本批次尚未向 Sub2API 发起写入"})
		return true
	}
	if !strings.HasPrefix(r.URL.Path, "/api/account-quality") {
		return false
	}
	q.tick()
	if r.Method == http.MethodGet && q.readError {
		write(w, 503, map[string]any{"error": "合成检测缓存不可读；保留最近结果"})
		return true
	}
	switch r.URL.Path {
	case "/api/account-quality/models":
		id := r.URL.Query().Get("id")
		if r.Method != http.MethodGet || q.snapshots[id] == nil {
			write(w, 400, map[string]any{"error": "invalid synthetic model account"})
			break
		}
		if q.modelsError {
			write(w, 503, map[string]any{"error": "合成模型目录暂不可读", "code": "models_unavailable"})
			break
		}
		source := "codex-models"
		if id == "fixture-3" || id == "fixture-4" {
			source = "bps-access"
		}
		models := []map[string]any{}
		if !q.modelsEmpty {
			models = []map[string]any{
				{"id": "synthetic-reasoning-fixture-only", "displayName": "推理模型 · 合成", "reasoningEfforts": []string{"low", "medium", "high", "xhigh"}},
				{"id": "synthetic-fast-fixture-only", "displayName": "轻量模型 · 合成", "reasoningEfforts": []string{"low", "medium", "high"}},
				{"id": "synthetic-visual-fixture-only", "displayName": "视觉模型 · 合成", "reasoningEfforts": []string{"medium", "high"}},
			}
		}
		result := map[string]any{"models": models, "source": source, "fetchedAt": time.Now().UTC().Format(time.RFC3339Nano)}
		if q.modelsEmpty {
			result["message"] = "合成账号未返回可用模型"
		}
		write(w, 200, result)
	case "/api/account-quality/manual", "/api/account-quality/manual/cancel":
		q.handleManual(w, r, write)
	case "/api/account-quality/settings":
		if r.Method == http.MethodPut {
			var value map[string]any
			if json.NewDecoder(r.Body).Decode(&value) != nil {
				write(w, 400, map[string]any{"error": "invalid fixture settings"})
				return true
			}
			if value["enabled"] == true && strings.TrimSpace(fmt.Sprint(value["model"])) == "" {
				write(w, 400, map[string]any{"error": "启用检测需明确填写可用模型"})
				return true
			}
			if fmt.Sprint(value["minimumScore"]) != "1" {
				write(w, 400, map[string]any{"error": "糖果题答对通过，正确题数必须为1"})
				return true
			}
			previous, _ := strconv.Atoi(fmt.Sprint(q.settings["revision"]))
			if fmt.Sprint(value["revision"]) != fmt.Sprint(previous) {
				write(w, 409, map[string]any{"error": "合成配置版本冲突"})
				return true
			}
			value["revision"] = previous + 1
			q.settings = value
			q.revision++
		}
		write(w, 200, q.settings)
	case "/api/account-quality":
		list := map[string]map[string]any{}
		for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
			if snapshot := q.snapshots[id]; snapshot != nil {
				list[id] = snapshot
			}
		}
		write(w, 200, map[string]any{"snapshots": list, "status": q.status()})
	case "/api/account-quality/status":
		write(w, 200, q.status())
	case "/api/account-quality/refresh", "/api/account-quality/cancel":
		var value struct {
			IDs     []string        `json:"ids"`
			All     bool            `json:"all"`
			Options json.RawMessage `json:"options"`
		}
		if json.NewDecoder(r.Body).Decode(&value) != nil {
			write(w, 400, map[string]any{"error": "invalid fixture request"})
			return true
		}
		selectedOptions, ok := q.selectedOptions(value.Options)
		if !ok {
			write(w, 400, map[string]any{"error": "invalid synthetic probe options"})
			break
		}
		q.requests = append(q.requests, map[string]any{"path": r.URL.Path, "ids": value.IDs, "all": value.All, "options": selectedOptions})
		if value.All {
			value.IDs = []string{"fixture-1", "fixture-2", "fixture-3", "fixture-4", "fixture-5", "fixture-6"}
		}
		accepted, merged, skipped := []string{}, []string{}, []map[string]any{}
		q.sequence++
		checkID := fmt.Sprintf("synthetic-check-%d", q.sequence)
		for _, id := range value.IDs {
			snapshot := q.snapshots[id]
			if strings.HasSuffix(r.URL.Path, "/cancel") {
				if _, exists := q.runs[id]; exists {
					delete(q.runs, id)
					snapshot["state"], snapshot["canCheck"] = "eligible", true
					snapshot["lastAttempt"] = map[string]any{"status": "cancelled", "finishedAt": time.Now().UTC().Format(time.RFC3339)}
					accepted = append(accepted, id)
				} else {
					merged = append(merged, id)
				}
				continue
			}
			if snapshot == nil || q.settings["enabled"] != true {
				skipped = append(skipped, map[string]any{"id": id, "reason": "检测已关闭或账号不存在"})
				continue
			}
			if _, exists := q.runs[id]; exists {
				merged = append(merged, id)
				continue
			}
			if snapshot["canCheck"] != true {
				skipped = append(skipped, map[string]any{"id": id, "reason": snapshot["unavailableReason"]})
				continue
			}
			q.runs[id] = time.Now()
			q.runOptions[id] = selectedOptions
			snapshot["state"], snapshot["canCheck"] = "queued", false
			snapshot["lastAttempt"] = map[string]any{"status": "queued", "source": "manual", "checkId": checkID}
			accepted = append(accepted, id)
		}
		q.revision++
		if q.responseLost {
			write(w, 503, map[string]any{"error": "合成响应丢失，请读取状态核对，不重复提交"})
			return true
		}
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			write(w, 200, map[string]any{"cancelled": accepted, "unchanged": merged})
		} else {
			write(w, 202, map[string]any{"checkId": checkID, "accepted": accepted, "merged": merged, "skipped": skipped})
		}
	default:
		write(w, 404, map[string]any{"error": "unknown synthetic quality route"})
	}
	return true
}

// A local animation exercises the same standalone HTML contract as a model
// response without calling a model or loading external resources.
const syntheticPelicanSVG = `<svg xmlns="http://www.w3.org/2000/svg" width="720" height="440" viewBox="0 0 720 440" role="img" aria-labelledby="title"><title id="title">合成鹈鹕骑自行车</title><rect width="720" height="440" rx="24" fill="#faf7ef"/><circle cx="584" cy="86" r="34" fill="#f5cc70"/><path d="M65 352H655" stroke="#d6d5cb" stroke-width="3"/><g fill="none" stroke-linecap="round" stroke-linejoin="round"><g stroke="#35494b" stroke-width="7"><circle cx="246" cy="298" r="62"/><circle cx="484" cy="298" r="62"/></g><g stroke="#27857d" stroke-width="8"><path d="M246 298L314 211L365 298L246 298M314 211L438 211L365 298M438 211L484 298M430 193L450 231"/></g><path d="M299 198H337M426 186H454L469 199" stroke="#35494b" stroke-width="7"/><circle cx="365" cy="298" r="12" stroke="#35494b" stroke-width="6"/><path d="M365 298L387 284" stroke="#35494b" stroke-width="6"/></g><path d="M312 180C263 158 266 113 309 103C339 96 365 114 373 142L402 166C382 195 344 205 312 180Z" fill="#fff" stroke="#35494b" stroke-width="4"/><path d="M318 143C328 173 355 181 373 165C351 164 342 151 337 135" fill="#d8e7de"/><path d="M348 127C345 95 359 70 383 74C399 77 403 98 393 114L387 157" fill="#fff" stroke="#35494b" stroke-width="4"/><path d="M392 102L454 111L392 123Z" fill="#df9e43" stroke="#35494b" stroke-width="3"/><path d="M394 118C415 127 422 139 401 144L387 137" fill="#efc07c" stroke="#35494b" stroke-width="3"/><circle cx="385" cy="95" r="3.5" fill="#35494b"/><path d="M331 193L346 244L365 286M354 193L391 232L410 222" fill="none" stroke="#cc8e3d" stroke-width="8" stroke-linecap="round"/><path d="M354 287H376M402 225L419 220" stroke="#cc8e3d" stroke-width="7" stroke-linecap="round"/></svg>`

const syntheticPelicanHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>合成鹈鹕骑自行车动画</title><style>
html,body{margin:0;width:100%;height:100%;overflow:hidden;background:#faf7ef}body{display:grid;place-items:center}svg{width:100%;height:100%;object-fit:contain}
svg>g,svg>path:not(:first-of-type){animation:ride .7s ease-in-out infinite alternate;transform-box:fill-box;transform-origin:center}
svg>path:first-of-type{stroke-dasharray:32 18;animation:road 1s linear infinite}
@keyframes ride{to{transform:translateY(-3px) rotate(-.4deg)}}@keyframes road{to{stroke-dashoffset:-50}}
</style></head><body>` + syntheticPelicanSVG + `</body></html>`

// This opt-in output checks the browser boundary using only the synthetic
// fixture page and a fixed, secret-free key. A blocked fetch is confirmed by
// the CSP event as well as the fixture server's independent request counter.
func syntheticSandboxPreview(canaryURL string) string {
	encodedURL, _ := json.Marshal(canaryURL)
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Synthetic preview isolation check</title><style>
body{margin:0;padding:32px;font:24px/1.7 system-ui;background:#f4f8f3;color:#173b22}h1{font-size:30px}li{overflow-wrap:anywhere}
</style></head><body><h1>Synthetic preview isolation check</h1><ul>
<li id="script-status">script: pending</li><li id="parent-status">parent.document: pending</li>
<li id="storage-status">localStorage: pending</li><li id="fetch-status">fetch: pending</li>
</ul><p>This page uses only synthetic data.</p><script>
(function () {
  const show = (id, value) => { document.getElementById(id).textContent = value; };
  show("script-status", "script: executed");
  try { void parent.document.getElementById("synthetic-preview-canary"); show("parent-status", "parent.document: ALLOWED (unexpected)"); }
  catch (error) { show("parent-status", "parent.document: " + (error.name === "SecurityError" ? "blocked (SecurityError)" : "unconfirmed (" + error.name + ")")); }
  try { void localStorage.getItem("synthetic-preview-canary"); show("storage-status", "localStorage: ALLOWED (unexpected)"); }
  catch (error) { show("storage-status", "localStorage: " + (error.name === "SecurityError" ? "blocked (SecurityError)" : "unconfirmed (" + error.name + ")")); }
  let policyBlocked = false, rejection = "";
  const update = () => {
    if (rejection) show("fetch-status", policyBlocked ? "fetch: blocked (connect-src)" : "fetch: unconfirmed (" + rejection + ")");
  };
  document.addEventListener("securitypolicyviolation", event => {
    if (event.effectiveDirective === "connect-src") { policyBlocked = true; update(); }
  });
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 2000);
  fetch(` + string(encodedURL) + `, {mode:"no-cors", credentials:"omit", cache:"no-store", signal:controller.signal})
    .then(() => show("fetch-status", "fetch: ALLOWED (unexpected)"))
    .catch(error => { rejection = error.name; update(); })
    .finally(() => clearTimeout(timeout));
})();
</script></body></html>`
}

func (q *qualityFixture) tickManual() {
	if q.manualHold {
		return
	}
	for id, started := range q.manualRuns {
		if time.Since(started) < 1200*time.Millisecond {
			continue
		}
		delete(q.manualRuns, id)
		record := q.manual[id]
		now := time.Now().UTC()
		record["finishedAt"], record["nextAllowedAt"] = now.Format(time.RFC3339Nano), now.Add(2*time.Second).Format(time.RFC3339Nano)
		record["durationMs"] = now.Sub(started).Milliseconds()
		if q.manualOutcome == "forbidden" {
			record["state"], record["errorCode"], record["httpStatus"], record["message"] = "failed", "forbidden", 403, "合成上游拒绝了本次绘图请求，请核对账号权限和所选模型。"
		} else if q.manualOutcome == "failed" {
			record["state"], record["errorCode"], record["message"] = "failed", "timeout", "合成手动题目超时；不影响糖果题结果"
		} else if q.manualOutcome == "sandbox" {
			record["state"], record["html"] = "success", syntheticSandboxPreview(q.previewCanaryURL)
		} else {
			record["state"], record["html"] = "success", syntheticPelicanHTML
		}
		q.revision++
	}
}

func (q *qualityFixture) handleManual(w http.ResponseWriter, r *http.Request, write func(http.ResponseWriter, int, any)) {
	if r.Method == http.MethodGet && r.URL.Path == "/api/account-quality/manual" {
		id := r.URL.Query().Get("id")
		if record := q.manual[id]; record != nil {
			write(w, 200, record)
		} else {
			write(w, 200, map[string]any{"id": id, "task": "pelican-svg", "state": "idle"})
		}
		return
	}
	if r.Method != http.MethodPost {
		write(w, 405, map[string]any{"error": "unsupported synthetic manual method"})
		return
	}
	var request struct {
		ID        string          `json:"id"`
		RequestID string          `json:"requestId"`
		Task      string          `json:"task"`
		Options   json.RawMessage `json:"options"`
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil || q.snapshots[request.ID] == nil {
		write(w, 400, map[string]any{"error": "invalid synthetic manual account"})
		return
	}
	selectedOptions, ok := q.selectedOptions(request.Options)
	if !ok {
		write(w, 400, map[string]any{"error": "invalid synthetic manual options"})
		return
	}
	q.requests = append(q.requests, map[string]any{"path": r.URL.Path, "id": request.ID, "requestId": request.RequestID, "task": request.Task, "options": selectedOptions})
	record := q.manual[request.ID]
	if strings.HasSuffix(r.URL.Path, "/cancel") {
		if record == nil {
			write(w, 200, map[string]any{"id": request.ID, "task": "pelican-svg", "state": "idle"})
			return
		}
		if request.RequestID != "" && request.RequestID != record["requestId"] {
			write(w, 409, map[string]any{"error": "合成手动题目已改变，请重新核对"})
			return
		}
		if record["state"] == "running" {
			delete(q.manualRuns, request.ID)
			record["state"], record["finishedAt"], record["message"] = "cancelled", time.Now().UTC().Format(time.RFC3339Nano), "合成手动题目已取消"
			q.revision++
		}
		write(w, 200, record)
		return
	}
	if request.Task != "pelican-svg" || strings.TrimSpace(request.RequestID) == "" {
		write(w, 400, map[string]any{"error": "invalid synthetic manual task"})
		return
	}
	if record != nil && record["requestId"] == request.RequestID {
		if record["model"] != selectedOptions["model"] || record["reasoningEffort"] != selectedOptions["reasoningEffort"] || record["hasOptions"] != (len(request.Options) > 0) {
			write(w, 409, map[string]any{"error": "合成请求标识已被不同选项使用"})
			return
		}
		write(w, map[bool]int{true: 202, false: 200}[record["state"] == "running"], record)
		return
	}
	if q.settings["enabled"] != true || q.snapshots[request.ID]["canCheck"] != true || len(q.manualRuns) > 0 || record != nil && record["state"] == "running" {
		write(w, 409, map[string]any{"error": "合成手动题目暂不可开始"})
		return
	}
	if record != nil {
		if next, err := time.Parse(time.RFC3339Nano, fmt.Sprint(record["nextAllowedAt"])); err == nil && time.Now().Before(next) {
			write(w, 409, map[string]any{"error": "合成手动题目仍在冷却"})
			return
		}
	}
	now := time.Now().UTC()
	record = map[string]any{"id": request.ID, "requestId": request.RequestID, "task": request.Task, "state": "running", "model": selectedOptions["model"], "reasoningEffort": selectedOptions["reasoningEffort"], "hasOptions": len(request.Options) > 0, "startedAt": now.Format(time.RFC3339Nano)}
	q.manual[request.ID], q.manualRuns[request.ID] = record, now
	q.revision++
	if q.responseLost {
		write(w, 503, map[string]any{"error": "合成响应丢失，请读取手动题目状态核对"})
		return
	}
	write(w, 202, record)
}

func (q *qualityFixture) selectedOptions(raw json.RawMessage) (map[string]string, bool) {
	if len(raw) == 0 {
		return map[string]string{"model": fmt.Sprint(q.settings["model"]), "reasoningEffort": fmt.Sprint(q.settings["reasoningEffort"])}, true
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	values := map[string]string{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || (key != "model" && key != "reasoningEffort") {
			return nil, false
		}
		if _, exists := values[key]; exists {
			return nil, false
		}
		var value string
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		values[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, false
	}
	model, effort := values["model"], values["reasoningEffort"]
	if len(values) != 2 || model == "" || len(model) > 128 || strings.TrimSpace(model) != model || strings.ContainsAny(model, "\r\n\t") {
		return nil, false
	}
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
	default:
		return nil, false
	}
	return values, true
}
