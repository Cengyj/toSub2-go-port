package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestQualityFixtureUploadBlockedIsOptInAndPreservesWholeBatch(t *testing.T) {
	q := newQualityFixture()
	const body = `{"ids":["fixture-1","fixture-6"],"config":{"baseUrl":"https://synthetic.invalid"}}`
	write := func(w http.ResponseWriter, code int, data any) {
		w.WriteHeader(code)
		if err := json.NewEncoder(w).Encode(data); err != nil {
			t.Fatal(err)
		}
	}
	for _, enabled := range []bool{false, true, false} {
		q.configure(url.Values{"qualityBlocked": {map[bool]string{true: "true", false: "false"}[enabled]}})
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/sub2api/upload", strings.NewReader(body))
		handled := q.handle(response, request, write)
		if handled != enabled {
			t.Fatal("upload fault changed the default fixture route", enabled, handled)
		}
		if !enabled {
			continue
		}
		var result struct {
			OK             bool           `json:"ok"`
			Outcome        string         `json:"outcome"`
			QualityBlocked bool           `json:"qualityBlocked"`
			Uncertain      bool           `json:"uncertain"`
			Selected       int            `json:"selected"`
			Uploaded       int            `json:"uploaded"`
			Skipped        int            `json:"skipped"`
			Result         map[string]int `json:"result"`
			Error          string         `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusConflict || result.OK || result.Outcome != "blocked" || !result.QualityBlocked || result.Uncertain || result.Selected != 2 || result.Uploaded != 0 || result.Skipped != 2 {
			t.Fatalf("invalid whole-batch blocked receipt: %#v", result)
		}
		if len(result.Result) != 3 || result.Result["success"] != 0 || result.Result["created"] != 0 || result.Result["updated"] != 0 || !strings.Contains(result.Error, "合成模型检测") {
			t.Fatal("blocked receipt claimed remote writes or lost its synthetic reason")
		}
	}
	if len(q.requests) != 1 || q.requests[0]["qualityBlocked"] != true || q.state()["qualityBlocked"] != false {
		t.Fatal("fixture fault was not isolated, recorded or resettable")
	}
	if len(q.runs) != 0 {
		t.Fatal("upload rejection unexpectedly scheduled a probe")
	}
}

func TestQualityFixtureSeparatesCurrentCandyAndLegacySixTaskHistory(t *testing.T) {
	q := newQualityFixture()
	if q.settings["minimumScore"] != 1 {
		t.Fatal("fixture still asks users for a six-task threshold")
	}
	for id, want := range map[string]int{"fixture-1": 1, "fixture-3": 0} {
		result := q.snapshots[id]["lastResult"].(map[string]any)
		if result["ruleVersion"] != "candy-v1" || result["total"] != 1 || result["score"] != want {
			t.Fatal("current fixture is not a candy observation", id)
		}
	}
	history := q.snapshots["fixture-4"]
	result := history["lastResult"].(map[string]any)
	if result["ruleVersion"] != "reasoning-six-v1" || result["total"] != 6 || history["stale"] != true {
		t.Fatal("historical fixture lost its original scale or stale identity")
	}
}

func TestQualityFixtureHistoryIsOptionalStableAndDoesNotRunProbes(t *testing.T) {
	q := newQualityFixture()
	before, _ := json.Marshal(q.snapshots)
	if _, exists := q.snapshots["fixture-1"]["probeHistory"]; exists {
		t.Fatal("default fixture unexpectedly enabled list history")
	}
	q.configure(url.Values{"qualityHistory": {"true"}})
	response := httptest.NewRecorder()
	if !q.handle(response, httptest.NewRequest(http.MethodGet, "/api/account-quality?ids=fixture-1,fixture-2", nil), func(w http.ResponseWriter, code int, value any) {
		w.WriteHeader(code)
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Fatal(err)
		}
	}) {
		t.Fatal("history snapshot route was not handled")
	}
	var body struct {
		Snapshots map[string]struct {
			History []struct {
				ID, Kind, Status, Verdict, Model, FinishedAt, ErrorCode string
				Score, Total                                            *int
				HTTPStatus                                              int
			} `json:"probeHistory"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	history := body.Snapshots["fixture-1"].History
	if response.Code != http.StatusOK || len(history) != 5 || len(body.Snapshots["fixture-2"].History) != 0 {
		t.Fatal("optional history did not preserve the five-cell and empty-account states")
	}
	ids := map[string]bool{}
	previous := time.Time{}
	for _, entry := range history {
		at, err := time.Parse(time.RFC3339Nano, entry.FinishedAt)
		if err != nil || !at.After(previous) || ids[entry.ID] || entry.ID == "" || entry.Model == "" {
			t.Fatal("history lost completed chronology, identity or model")
		}
		ids[entry.ID], previous = true, at
	}
	for index, want := range map[int]int{0: 1, 1: 0, 4: 1} {
		entry := history[index]
		if entry.Kind != "candy" || entry.Status != "success" || entry.Score == nil || *entry.Score != want || entry.Total == nil || *entry.Total != 1 || (entry.Verdict == "normal") != (want == 1) {
			t.Fatal("candy history lost its actual single-question outcome", index)
		}
	}
	if entry := history[2]; entry.Kind != "pelican" || entry.Status != "success" || entry.Score != nil || entry.Total != nil {
		t.Fatal("drawing completion acquired a candy score")
	}
	if entry := history[3]; entry.Status != "failed" || entry.HTTPStatus != 429 || entry.ErrorCode != "rate_limited" || entry.Score != nil || entry.Total != nil {
		t.Fatal("rate-limited request acquired a measured score or lost its HTTP evidence")
	}
	enabled, _ := json.Marshal(q.snapshots)
	q.configure(url.Values{"qualityHistory": {"true"}})
	again, _ := json.Marshal(q.snapshots)
	if string(enabled) != string(again) {
		t.Fatal("re-enabling history duplicated or changed the fixed observations")
	}
	q.configure(url.Values{"qualityHistory": {"false"}})
	after, _ := json.Marshal(q.snapshots)
	if string(before) != string(after) || len(q.requests)+len(q.runs)+len(q.manualRuns)+len(q.manual) != 0 || q.revision != 1 {
		t.Fatal("history presentation changed normal snapshots or executed a probe")
	}
}

func TestManualFixtureCompletionIsSeparateFromCandyAndDeduplicatesRequests(t *testing.T) {
	q := newQualityFixture()
	before, _ := json.Marshal(q.snapshots)
	write := func(w http.ResponseWriter, code int, data any) {
		w.WriteHeader(code)
		if err := json.NewEncoder(w).Encode(data); err != nil {
			t.Fatal(err)
		}
	}
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		recorder := httptest.NewRecorder()
		if !q.handle(recorder, httptest.NewRequest(method, path, strings.NewReader(body)), write) {
			t.Fatal("manual fixture route was not handled")
		}
		var result map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return recorder.Code, result
	}
	if code, result := call(http.MethodGet, "/api/account-quality/manual?id=fixture-1", ""); code != 200 || result["state"] != "idle" {
		t.Fatal("unstarted manual fixture did not read as idle")
	}
	q.manualHold = true
	const request = `{"id":"fixture-1","requestId":"synthetic-manual-request","task":"pelican-svg"}`
	if code, result := call(http.MethodPost, "/api/account-quality/manual", request); code != 202 || result["state"] != "running" || result["html"] != nil || result["svg"] != nil {
		t.Fatal("manual receipt prematurely claimed a completed drawing")
	}
	started := q.manualRuns["fixture-1"]
	if code, _ := call(http.MethodPost, "/api/account-quality/manual", request); code != 202 || q.manualRuns["fixture-1"] != started {
		t.Fatal("duplicate manual request scheduled another drawing")
	}
	q.manualRuns["fixture-1"] = time.Now().Add(-2 * time.Second)
	if _, result := call(http.MethodGet, "/api/account-quality/manual?id=fixture-1", ""); result["state"] != "running" {
		t.Fatal("hold scene did not retain the running task")
	}
	q.manualHold = false
	if code, result := call(http.MethodGet, "/api/account-quality/manual?id=fixture-1", ""); code != 200 || result["state"] != "success" || result["html"] != syntheticPelicanHTML || result["svg"] != nil || result["finishedAt"] == nil || result["nextAllowedAt"] == nil {
		t.Fatal("completed manual fixture lost its HTML animation or lifecycle times")
	}
	if code, result := call(http.MethodPost, "/api/account-quality/manual", request); code != 200 || result["state"] != "success" || len(q.manualRuns) != 0 {
		t.Fatal("completed request replay scheduled another drawing")
	}
	after, _ := json.Marshal(q.snapshots)
	if string(before) != string(after) || len(q.runs) != 0 {
		t.Fatal("manual illustration changed candy scores or scheduled automatic probes")
	}
}

func TestManualFixtureCancellationCannotCancelAnotherRequest(t *testing.T) {
	q := newQualityFixture()
	q.manualHold = true
	write := func(w http.ResponseWriter, code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(data)
	}
	start := httptest.NewRecorder()
	q.handle(start, httptest.NewRequest(http.MethodPost, "/api/account-quality/manual", strings.NewReader(`{"id":"fixture-3","requestId":"synthetic-bps-manual","task":"pelican-svg"}`)), write)
	if start.Code != 202 {
		t.Fatal("eligible synthetic BPS account could not start a manual illustration")
	}
	for _, requestID := range []string{"stale-request", "synthetic-bps-manual"} {
		response := httptest.NewRecorder()
		q.handle(response, httptest.NewRequest(http.MethodPost, "/api/account-quality/manual/cancel", strings.NewReader(`{"id":"fixture-3","requestId":"`+requestID+`"}`)), write)
		if requestID == "stale-request" {
			if response.Code != 409 || q.manual["fixture-3"]["state"] != "running" {
				t.Fatal("stale cancellation changed another manual request")
			}
		} else if response.Code != 200 || q.manual["fixture-3"]["state"] != "cancelled" || len(q.manualRuns) != 0 {
			t.Fatal("matching cancellation did not finish the manual task")
		}
	}
}

func TestQualityFixtureModelCatalogHasEmptyAndFailureStatesWithoutGeneratingContent(t *testing.T) {
	q := newQualityFixture()
	call := func(path string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		q.handle(w, httptest.NewRequest(http.MethodGet, path, nil), func(w http.ResponseWriter, status int, value any) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(value)
		})
		var value map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return w.Code, value
	}
	for id, source := range map[string]string{"fixture-1": "codex-models", "fixture-3": "bps-access"} {
		status, value := call("/api/account-quality/models?id=" + id)
		models, ok := value["models"].([]any)
		if status != 200 || value["source"] != source || value["fetchedAt"] == nil || !ok || len(models) != 3 {
			t.Fatal("synthetic catalog missing real response contract")
		}
		first := models[0].(map[string]any)
		if first["id"] != q.settings["model"] || first["displayName"] == nil || len(first["reasoningEfforts"].([]any)) == 0 {
			t.Fatal("catalog entry has no selectable model/effort")
		}
	}
	q.modelsEmpty = true
	if status, value := call("/api/account-quality/models?id=fixture-1"); status != 200 || len(value["models"].([]any)) != 0 || value["message"] == nil {
		t.Fatal("empty catalog fabricated default model options")
	}
	q.modelsError = true
	if status, value := call("/api/account-quality/models?id=fixture-1"); status != 503 || value["code"] != "models_unavailable" {
		t.Fatal("failed catalog claimed a successful default")
	}
	if len(q.requests) != 0 || len(q.runs) != 0 || len(q.manualRuns) != 0 {
		t.Fatal("catalog lookup generated model content")
	}
}

func TestQualityFixtureCapturesPerRunOptionsWithoutChangingGlobalSettings(t *testing.T) {
	q := newQualityFixture()
	q.manualHold = true
	before, _ := json.Marshal(q.settings)
	call := func(path, body string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		q.handle(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)), func(w http.ResponseWriter, status int, value any) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(value)
		})
		var value map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return w.Code, value
	}
	const manual = `{"id":"fixture-3","requestId":"synthetic-options-manual","task":"pelican-svg","options":{"model":"synthetic-visual-fixture-only","reasoningEffort":"medium"}}`
	if status, value := call("/api/account-quality/manual", manual); status != 202 || value["model"] != "synthetic-visual-fixture-only" || value["reasoningEffort"] != "medium" {
		t.Fatal("manual task ignored explicit model or effort")
	}
	if status, _ := call("/api/account-quality/manual", strings.Replace(manual, `"medium"`, `"high"`, 1)); status != 409 {
		t.Fatal("different manual options reused request identity")
	}
	if status, _ := call("/api/account-quality/refresh", `{"ids":["fixture-1"],"options":{"model":"synthetic-fast-fixture-only","reasoningEffort":"low"}}`); status != 202 {
		t.Fatal("one-off candy model rejected")
	}
	q.runs["fixture-1"] = time.Now().Add(-7 * time.Second)
	q.tick()
	if q.snapshots["fixture-1"]["lastResult"].(map[string]any)["model"] != "synthetic-fast-fixture-only" {
		t.Fatal("automatic task read global model after accepting override")
	}
	for _, options := range []string{`null`, `{}`, `{"model":"m"}`, `{"model":"m","reasoningEffort":""}`, `{"model":"m","reasoningEffort":"high","extra":true}`, `{"model":"m","model":"other","reasoningEffort":"high"}`} {
		if status, _ := call("/api/account-quality/refresh", `{"ids":["fixture-6"],"options":`+options+`}`); status != 400 {
			t.Fatal("malformed override silently fell back to settings")
		}
	}
	after, _ := json.Marshal(q.settings)
	if string(before) != string(after) {
		t.Fatal("one-off options modified saved global policy")
	}
}

func TestCandyFixtureOneOffResultIsIndependentRatherThanExpired(t *testing.T) {
	q := newQualityFixture()
	q.runs["fixture-1"] = time.Now().Add(-7 * time.Second)
	q.runOptions["fixture-1"] = map[string]string{"model": "synthetic-selected-model", "reasoningEffort": "low"}
	q.tick()
	if q.snapshots["fixture-1"]["stale"] != true || q.snapshots["fixture-1"]["staleReason"] != "options_changed" {
		t.Fatal("fresh independent observation was described as expired or a default-policy pass")
	}
	q.runs["fixture-1"] = time.Now().Add(-7 * time.Second)
	q.runOptions["fixture-1"] = map[string]string{"model": q.settings["model"].(string), "reasoningEffort": q.settings["reasoningEffort"].(string)}
	q.tick()
	if q.snapshots["fixture-1"]["stale"] != false || q.snapshots["fixture-1"]["staleReason"] != nil {
		t.Fatal("default-parameter observation retained the independent-result reason")
	}
}
