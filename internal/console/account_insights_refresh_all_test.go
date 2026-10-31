package console

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/protocol"
)

func TestAccountInsightsAPIRefreshAllUsesFullInventoryAndManualSemantics(t *testing.T) {
	f := newAccountInsightAPIFixture(t, func(ctx context.Context) protocol.InsightResult {
		<-ctx.Done()
		return protocol.InsightResult{}
	})
	settings := accountInsightAPISettings(t, f.server, true)
	settings.ExcludedAccountIDs = []string{"synthetic-second"}
	w := accountInsightAPIRequest(f.server, http.MethodPut, "/api/account-insights/settings", accountInsightJSON(t, settings))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	// Browser pagination, filtering and selection must not scope the inventory.
	w = accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh?page=2&email=not-present&ids=synthetic-first", `{"all":true}`)
	var receipt accountinsight.Receipt
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
		t.Fatal("all-account refresh was not accepted", w.Code, w.Body.String())
	}
	if receipt.QueryID == "" || !reflect.DeepEqual(receipt.Accepted, []string{"synthetic-first", "synthetic-second"}) || !reflect.DeepEqual(receipt.Skipped, []accountinsight.Skip{{ID: "synthetic-busy", Reason: "账号操作进行中，查询暂缓"}}) {
		t.Fatalf("all-account receipt lost inventory or manual eligibility: %+v", receipt)
	}
	w = accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", `{"all":true}`)
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || len(receipt.Accepted) != 0 || !reflect.DeepEqual(receipt.Merged, []string{"synthetic-first", "synthetic-second"}) {
		t.Fatal("repeat all-account request did not merge existing queries", w.Code, w.Body.String())
	}
}

func TestAccountInsightsAPIRefreshAllRejectsAmbiguousOrInvalidSelection(t *testing.T) {
	f := newAccountInsightAPIFixture(t, nil)
	accountInsightAPISettings(t, f.server, true)
	for _, body := range []string{
		`{"all":true,"ids":["synthetic-first"]}`,
		`{"all":true,"ids":[]}`,
		`{"all":true,"ids":null}`,
		`{"all":"true"}`,
		`{"all":false}`,
		`{"ids":[]}`,
		`{"ids":"synthetic-first"}`,
		`{}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", body)
			if w.Code != http.StatusBadRequest {
				t.Fatal("ambiguous or invalid selection admitted work", w.Code, w.Body.String())
			}
		})
	}
	status := f.server.insights.Status()
	if f.calls.Load() != 0 || status.Active != 0 || status.Queued != 0 {
		t.Fatal("invalid all-account request queried upstream")
	}
	w := accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", `{"all":false,"ids":["synthetic-first"]}`)
	var receipt accountinsight.Receipt
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || !reflect.DeepEqual(receipt.Accepted, []string{"synthetic-first"}) {
		t.Fatal("explicit false broke the existing selected-account API", w.Code, w.Body.String())
	}
}

func TestAccountInsightsAPIRefreshAllPreservesDisabledAndClosedGates(t *testing.T) {
	f := newAccountInsightAPIFixture(t, nil)
	w := accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", `{"all":true}`)
	if w.Code != http.StatusConflict {
		t.Fatal("disabled service admitted all-account refresh", w.Code)
	}
	accountInsightAPISettings(t, f.server, true)
	if err := f.server.insights.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	w = accountInsightAPIRequest(f.server, http.MethodPost, "/api/account-insights/refresh", `{"all":true}`)
	if w.Code != http.StatusConflict || f.calls.Load() != 0 {
		t.Fatal("closed service admitted all-account refresh", w.Code)
	}
}
