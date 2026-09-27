package console

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/jobs"
)

func TestLubanNumberAliasIgnoresConflictingModernConfiguration(t *testing.T) {
	var acquisitions, conflictingRequests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("apikey") != "legacy-fixture-key" {
			t.Error("alias did not use top-level Luban API key")
		}
		switch r.URL.Path {
		case "/v2/api/getNumber":
			acquisitions.Add(1)
			if r.URL.Query().Get("service_id") != "121949" {
				t.Error("alias did not use top-level service ID")
			}
			fmt.Fprint(w, `{"code":0,"number":"+12025550123","request_id":"fixture-lease"}`)
		case "/v2/api/setStatus":
			fmt.Fprint(w, `{"code":0}`)
		default:
			t.Errorf("unexpected Luban request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)
	conflicting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conflictingRequests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(conflicting.Close)
	t.Setenv("LUBAN_SMS_API_BASE", backend.URL+"/v2/api/")
	srv, manager := newMonitorTestServer(t)
	for _, provider := range []string{"smsbower", "custom"} {
		job, err := manager.Submit(jobs.Spec{ID: "legacy-" + provider, Email: provider + "@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		manager.Emit(jobs.JobEvent{JobID: job.ID, Stage: "phone", Status: jobs.StatusWaitingInput})
		w := frontendRequest(srv, http.MethodPost, "/api/jobs/"+job.ID+"/luban-number", map[string]any{
			"apiKey": "legacy-fixture-key", "serviceId": "121949", "providerId": provider,
			"config": map[string]any{"apiKey": "conflicting-fixture-key", "serviceId": "999999", "baseUrl": conflicting.URL, "entries": "+12025550199----" + conflicting.URL},
		}, "test")
		if w.Code != http.StatusOK {
			t.Fatalf("alias with %s conflict returned status %d", provider, w.Code)
		}
		current, _ := manager.Get(job.ID)
		if current.SMSProviderID != "luban" || current.CurrentPhone != "+12025550123" {
			t.Fatal("legacy alias selected the wrong provider or number")
		}
	}
	job, err := manager.Submit(jobs.Spec{ID: "missing-legacy-key", Email: "missing@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	manager.Emit(jobs.JobEvent{JobID: job.ID, Stage: "phone", Status: jobs.StatusWaitingInput})
	w := frontendRequest(srv, http.MethodPost, "/api/jobs/"+job.ID+"/luban-number", map[string]any{
		"config": map[string]any{"apiKey": "legacy-fixture-key", "serviceId": "121949", "baseUrl": backend.URL + "/v2/api/"},
	}, "test")
	if w.Code != http.StatusBadRequest || acquisitions.Load() != 2 || conflictingRequests.Load() != 0 {
		t.Fatalf("alias fallback or conflicting endpoint used: status=%d acquisitions=%d conflicts=%d", w.Code, acquisitions.Load(), conflictingRequests.Load())
	}
}

func TestSMSProviderOptionsAcceptsOnlyPost(t *testing.T) {
	var requests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("action") {
		case "getPrices":
			fmt.Fprint(w, `{"1":{"dr":{"cost":0.1,"count":3}}}`)
		case "getCountries":
			fmt.Fprint(w, `{"1":{"id":1,"eng":"Fixture"}}`)
		default:
			t.Error("unexpected options request")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)
	srv, _ := newMonitorTestServer(t)
	body := map[string]any{"config": map[string]any{"apiKey": "fixture-options-key", "service": "dr", "baseUrl": backend.URL}}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		w := frontendRequest(srv, method, "/api/sms-providers/smsbower/options", body, "test")
		if w.Code != http.StatusNotFound || requests.Load() != 0 {
			t.Fatalf("%s returned status %d and made %d provider requests", method, w.Code, requests.Load())
		}
	}
	w := frontendRequest(srv, http.MethodPost, "/api/sms-providers/smsbower/options", body, "test")
	if w.Code != http.StatusOK || requests.Load() != 2 {
		t.Fatalf("POST options returned status %d with %d provider requests", w.Code, requests.Load())
	}
}
