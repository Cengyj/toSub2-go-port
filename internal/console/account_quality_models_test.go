package console

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestAccountQualityModelsAPIRequiresOneExactReadScope(t *testing.T) {
	s, calls := qualityAPIFixture(t, nil)
	before, _ := s.quality.Service.Settings()
	for _, query := range []string{"", "?id=", "?id=normal-10&id=normal-10", "?ids=normal-10", "?id=normal-10&all=true", "?id=..%2Fescape", "?id=line%0Afeed", "?id=%20normal-10", "?id=normal-10;extra"} {
		w := accountInsightAPIRequest(s, http.MethodGet, "/api/account-quality/models"+query, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid catalog query accepted: %q => %d", query, w.Code)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		w := accountInsightAPIRequest(s, method, "/api/account-quality/models?id=normal-10", "")
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("catalog method %s accepted: %d", method, w.Code)
		}
	}
	after, _ := s.quality.Service.Settings()
	if calls.Load() != 0 || !reflect.DeepEqual(before, after) {
		t.Fatal("model catalog validation initiated inference or changed policy")
	}
}

func TestAccountQualityModelsAPIReportsMissingAccountWithoutFallback(t *testing.T) {
	s, calls := qualityAPIFixture(t, nil)
	w := accountInsightAPIRequest(s, http.MethodGet, "/api/account-quality/models?id=normal-10", "")
	var failure struct {
		Error  string `json:"error"`
		Code   string `json:"code"`
		Models []any  `json:"models"`
	}
	if w.Code != http.StatusNotFound || json.Unmarshal(w.Body.Bytes(), &failure) != nil || failure.Code != "not_found" || failure.Error == "" || failure.Models != nil || calls.Load() != 0 {
		t.Fatalf("missing saved account fabricated a catalog: %d %s", w.Code, w.Body.String())
	}
}
