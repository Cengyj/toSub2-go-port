package console

import (
	"encoding/json"
	"net/http"
	"testing"

	"tosub2-go-port/internal/application"
)

func TestManualQualityAPIReadsAreIdleAndExplicitActionsAreValidated(t *testing.T) {
	s, calls := qualityAPIFixture(t, nil)
	w := accountInsightAPIRequest(s, http.MethodGet, "/api/account-quality/manual?id=normal-10", "")
	var idle application.ManualProbeRecord
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &idle) != nil || idle.State != "idle" || idle.Task != "pelican-svg" || calls.Load() != 0 {
		t.Fatal("manual read initiated a probe or lost the idle contract")
	}
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/api/account-quality/manual", `{"id":"normal-10","requestId":"manual-api-01","task":"pelican-svg"}`, 409},
		{"/api/account-quality/manual", `{"id":"normal-10","requestId":"manual-api-01","task":"pelican-svg","prompt":"arbitrary"}`, 400},
		{"/api/account-quality/manual", `{"id":"normal-10","requestId":"manual-api-01","task":"arbitrary"}`, 400},
		{"/api/account-quality/manual", `{"id":"../escape","requestId":"manual-api-01","task":"pelican-svg"}`, 400},
		{"/api/account-quality/manual/cancel", `{"id":"normal-10","requestId":"not-running"}`, 409},
	} {
		w = accountInsightAPIRequest(s, http.MethodPost, tc.path, tc.body)
		if w.Code != tc.status {
			t.Fatalf("invalid manual action status=%d want=%d", w.Code, tc.status)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("manual routes invoked automatic quality runner")
	}
}
