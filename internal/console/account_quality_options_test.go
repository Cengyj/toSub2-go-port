package console

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/modelprobe"
)

func TestQualityRefreshAPIUsesSingleRunModelWithoutSavingGlobalSettings(t *testing.T) {
	observed := make(chan modelprobe.Options, 1)
	server, calls := qualityAPIFixture(t, func(_ context.Context, opts modelprobe.Options) (modelprobe.Result, error) {
		observed <- opts
		return modelprobe.Result{Model: opts.Model, RuleVersion: modelprobe.RuleVersion, Verdict: "normal", Score: 1, Total: 1, CheckedAt: time.Now().UTC()}, nil
	})
	before := enableQualityAPI(t, server)
	response := accountInsightAPIRequest(server, http.MethodPost, "/api/account-quality/refresh", `{"ids":["normal-10"],"options":{"model":"synthetic-selected","reasoningEffort":"low"}}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("single-run submission rejected: %d %s", response.Code, response.Body.String())
	}
	select {
	case opts := <-observed:
		if opts.Model != "synthetic-selected" || opts.ReasoningEffort != "low" || opts.TimeoutSeconds != before.TimeoutSeconds || opts.MinimumScore != 1 {
			t.Fatal("API did not pass selected options with original execution limits")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("selected probe did not execute")
	}
	waitAccountInsightAPI(t, func() bool { status := server.quality.Status(); return status.Active == 0 && status.Queued == 0 })
	after, err := server.quality.Service.Settings()
	if err != nil || after != before || calls.Load() != 1 {
		t.Fatal("single observation changed global settings/revision or dispatched twice")
	}
	rows, _ := server.quality.Service.Snapshots([]string{"normal-10"})
	if rows["normal-10"].LastResult == nil || rows["normal-10"].LastResult.Model != "synthetic-selected" || !rows["normal-10"].Stale {
		t.Fatal("selected-model observation was not retained separately from global policy")
	}
}

func TestQualityProbeAPIsRejectAmbiguousOptionsBeforeDispatch(t *testing.T) {
	server, calls := qualityAPIFixture(t, nil)
	before := enableQualityAPI(t, server)
	for _, path := range []string{"/api/account-quality/refresh", "/api/account-quality/manual"} {
		for _, options := range []string{
			`null`, `{}`, `{"model":"synthetic"}`, `{"model":"synthetic","reasoningEffort":""}`,
			`{"model":"synthetic","reasoningEffort":"high","unknown":1}`,
			`{"model":"one","model":"two","reasoningEffort":"high"}`,
			`{"model":"synthetic","reasoningEffort":"high","\u0072easoningEffort":"low"}`,
			`{"model":"synthetic\nheader","reasoningEffort":"high"}`,
		} {
			base := `"ids":["normal-10"]`
			if strings.HasSuffix(path, "/manual") {
				base = `"id":"normal-10","requestId":"synthetic-options-0001","task":"pelican-svg"`
			}
			body := `{` + base + `,"options":` + options + `}`
			response := accountInsightAPIRequest(server, http.MethodPost, path, body)
			if response.Code != 400 {
				t.Fatalf("ambiguous option submitted on %s: status %d", path, response.Code)
			}
		}
	}
	for _, body := range []string{
		`{"ids":["normal-10"],"options":{"model":"one","reasoningEffort":"high"},"options":{"model":"two","reasoningEffort":"low"}}`,
		`{"ids":["normal-10"],"ids":["second-20"]}`,
		`{"ids":["normal-10"],"unknown":true}`,
	} {
		if response := accountInsightAPIRequest(server, http.MethodPost, "/api/account-quality/refresh", body); response.Code != 400 {
			t.Fatal("duplicate or unknown action field accepted")
		}
	}
	after, err := server.quality.Service.Settings()
	if err != nil || after != before || calls.Load() != 0 {
		t.Fatal("invalid selection changed policy or contacted a model")
	}
}
