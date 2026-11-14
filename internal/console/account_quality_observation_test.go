package console

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/protocol"
)

func TestAccountQualityAPIProjectsHTTPObservationAndNewSuccess(t *testing.T) {
	for _, status := range []int{401, 402, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var recovered atomic.Bool
			s, calls := qualityAPIFixture(t, func(_ context.Context, o modelprobe.Options) (modelprobe.Result, error) {
				if recovered.Load() {
					return modelprobe.Result{Model: o.Model, RuleVersion: modelprobe.RuleVersion, Verdict: "normal", Score: modelprobe.TotalQuestions, Total: modelprobe.TotalQuestions, CheckedAt: time.Now().UTC()}, nil
				}
				code := "http_error"
				switch status {
				case 401:
					code = "unauthorized"
				case 403:
					code = "forbidden"
				case 429:
					code = "rate_limited"
				}
				return modelprobe.Result{}, &modelprobe.Error{Code: code, HTTPStatus: status}
			})
			j, err := s.jobs.Submit(jobs.Spec{ID: "normal-10", Email: "synthetic-observation@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			// Only a regular saved-file marker is needed for this read-only API
			// fixture. The fake provider never reads or sends an authorization.
			if err := protocol.WritePrivateJSON(filepath.Join(s.cfg.OutputRoot, j.ID, "sub2api-import-oauth.json"), map[string]bool{"synthetic": true}); err != nil {
				t.Fatal(err)
			}
			s.jobs.UpdateResult(j.ID, true, "")
			settings := enableQualityAPI(t, s)
			settings.CooldownSeconds = 0
			if _, err := s.quality.Service.Update(t.Context(), settings); err != nil {
				t.Fatal(err)
			}
			refresh := func() {
				w := accountInsightAPIRequest(s, http.MethodPost, "/api/account-quality/refresh", `{"ids":["normal-10"]}`)
				if w.Code != http.StatusAccepted {
					t.Fatalf("refresh failed: %d", w.Code)
				}
				waitAccountInsightAPI(t, func() bool { st := s.quality.Status(); return st.Active == 0 && st.Queued == 0 })
			}
			readObservation := func() *accountquality.HTTPObservation {
				w := accountInsightAPIRequest(s, http.MethodGet, "/api/account-quality?ids=normal-10", "")
				var body struct {
					Snapshots map[string]accountquality.Snapshot `json:"snapshots"`
				}
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
					t.Fatal("snapshot read failed")
				}
				return body.Snapshots[j.ID].HTTPObservation
			}
			refresh()
			if o := readObservation(); o == nil || o.Status != "failed" || o.HTTPStatus != status || o.Source != "candy" || o.FinishedAt.IsZero() {
				t.Fatalf("HTTP failure absent from batch API: %+v", o)
			}
			if calls.Load() != 1 {
				t.Fatal("snapshot read initiated another probe")
			}
			recovered.Store(true)
			refresh()
			if o := readObservation(); o == nil || o.Status != "success" || o.HTTPStatus != 200 || o.ErrorCode != "" {
				t.Fatalf("success failed to clear old HTTP error: %+v", o)
			}
			if calls.Load() != 2 {
				t.Fatal("status reads replayed inference")
			}
		})
	}
}
