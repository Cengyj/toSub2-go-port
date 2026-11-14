package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/protocol"
)

func TestAccountInsightsAvailabilitySelectionIncludesUnknownAndDeduplicates(t *testing.T) {
	f := newAccountInsightAPIFixture(t, nil)
	accountInsightAPISettings(t, f.server, true)
	w := accountInsightAPIRequest(f.server, http.MethodGet, "/api/account-insights/status?ids=synthetic-first,missing,synthetic-first", "")
	var wire struct {
		Refresh   *struct{ Total, Eligible, Unavailable int } `json:"refresh"`
		Selection *struct{ Total, Eligible, Unavailable int } `json:"selection"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &wire) != nil || wire.Refresh == nil || wire.Selection == nil {
		t.Fatal("status did not include full and selected availability", w.Code)
	}
	if wire.Refresh.Total != 3 || wire.Refresh.Eligible != 2 || wire.Refresh.Unavailable != 1 || wire.Selection.Total != 2 || wire.Selection.Eligible != 1 || wire.Selection.Unavailable != 1 {
		t.Fatal("selection did not preserve its scope or unknown account")
	}
	if f.calls.Load() != 0 {
		t.Fatal("status read queried upstream")
	}
}

func TestAccountInsightsAvailabilityRejectsEmptySelection(t *testing.T) {
	f := newAccountInsightAPIFixture(t, nil)
	for _, suffix := range []string{"?ids=", "?ids=,", "?ids=synthetic-first,", "?ids=synthetic-first&ids=synthetic-second", "?ids=%", "?ids=synthetic%2Ffirst", "?ids=+", "?ids=.", "?ids=..", "?ids=" + strings.Repeat("a", 129), "?ids=synthetic-first;ids=synthetic-second", "?ids=" + strings.TrimSuffix(strings.Repeat("synthetic-first,", 1001), ",")} {
		w := accountInsightAPIRequest(f.server, http.MethodGet, "/api/account-insights/status"+suffix, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid selection returned HTTP %d", w.Code)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("invalid status selection queried upstream")
	}
}

func TestAccountInsightsAvailabilityUnscopedAndMaximumSelection(t *testing.T) {
	f := newAccountInsightAPIFixture(t, nil)
	accountInsightAPISettings(t, f.server, true)
	w := accountInsightAPIRequest(f.server, http.MethodGet, "/api/account-insights/status", "")
	var raw map[string]json.RawMessage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &raw) != nil || raw["refresh"] == nil || raw["selection"] != nil {
		t.Fatal("unscoped status response changed its selection meaning")
	}
	w = accountInsightAPIRequest(f.server, http.MethodGet, "/api/account-insights/status?ids="+strings.TrimSuffix(strings.Repeat("synthetic-first,", 1000), ","), "")
	var status accountinsight.Status
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Selection == nil || status.Selection.Total != 1 || status.Selection.Eligible != 1 {
		t.Fatal("maximum valid selection was not deduplicated", w.Code)
	}
	accountInsightAPISettings(t, f.server, false)
	w = accountInsightAPIRequest(f.server, http.MethodGet, "/api/account-insights/status?ids=synthetic-first,missing", "")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Refresh.Total != 3 || status.Refresh.Eligible != 0 || status.Refresh.Unavailable != 3 || status.Selection.Total != 2 || status.Selection.Unavailable != 2 {
		t.Fatal("disabled availability lost selected targets")
	}
	if f.calls.Load() != 0 {
		t.Fatal("availability reads queried upstream")
	}
}

func TestAccountInsightsAvailabilityBusyTargetDoesNotBlockIndependentSelection(t *testing.T) {
	f := newAccountInsightAPIFixture(t, func(ctx context.Context) protocol.InsightResult { <-ctx.Done(); return protocol.InsightResult{} })
	accountInsightAPISettings(t, f.server, true)
	w := accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", `{"ids":["synthetic-first"]}`)
	if w.Code != http.StatusAccepted {
		t.Fatal("fixture query was not admitted", w.Code)
	}
	w = accountInsightAPIRequest(f.server, http.MethodGet, "/api/account-insights/status?ids=synthetic-second", "")
	var status accountinsight.Status
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Refresh.Active+status.Refresh.Queued != 1 || status.Selection == nil || status.Selection.Eligible != 1 || status.Selection.Active+status.Selection.Queued != 0 {
		t.Fatal("other live work incorrectly disabled an independent selection")
	}
	w = accountInsightAPIRequest(f.server, http.MethodGet, "/api/account-insights/status?ids=synthetic-first", "")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Selection.Eligible != 0 || status.Selection.Active+status.Selection.Queued != 1 {
		t.Fatal("live target remained refreshable after receipt")
	}
	w = accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", `{"ids":["synthetic-first"]}`)
	var receipt accountinsight.Receipt
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || len(receipt.Accepted) != 0 || len(receipt.Merged) != 1 || len(receipt.Skipped) != 0 {
		t.Fatal("status projection changed the compatible duplicate receipt")
	}
}
