package protocol

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestInsightBPSUnknownDeniedAndMissingCatalog(t *testing.T) {
	for _, tc := range []struct {
		body string
		want *bool
	}{
		{`{"allowed":null}`, nil},
		{`{"allowed":false}`, insightBool(false)},
		{`{"allowed":true,"model_catalog":{"models":[]}}`, insightBool(true)},
	} {
		got, err := parseInsightBPS([]byte(tc.body), insightTestCredentials(), time.Now())
		if err != nil || !reflect.DeepEqual(got.Allowed, tc.want) || len(got.Models) != 0 {
			t.Fatalf("unknown and denial semantics changed: got=%+v err=%v", got, err)
		}
	}
	for _, body := range []string{`{}`, `{"allowed":true}`, `{"allowed":true,"model_catalog":{"models":null}}`, `{"allowed":"true"}`, `{"allowed":false,"model_catalog":[]}`} {
		if _, err := parseInsightBPS([]byte(body), insightTestCredentials(), time.Now()); err == nil {
			t.Fatalf("invalid permission response accepted: %s", body)
		}
	}
}

func TestInsightBPSPreservesRestrictionsAndUnknownAvailability(t *testing.T) {
	body := `{"allowed":true,"model_catalog":{"restricted_models":["gpt-restricted"],"models":[
		{"id":"gpt-open","label":"Open","available":true,"efforts":[{"value":"low"},{"value":"xhigh"},{"value":"low"},{"value":"future"}]},
		{"id":"gpt-unknown"},
		{"id":"gpt-restricted","available":true},
		{"id":"gpt-policy","available":true,"policy":{"state":"disabled"}},
		{"id":"gpt-flags","available":true,"enabled":false,"model_picker_enabled":true},
		{"id":"not-an-upstream-model"}
	]}}`
	got, err := parseInsightBPS([]byte(body), insightTestCredentials(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 5 || got.Models[0].Available == nil || !*got.Models[0].Available || got.Models[1].Available != nil || got.Models[1].Restricted {
		t.Fatalf("catalog availability was inferred or discarded: %+v", got.Models)
	}
	if !reflect.DeepEqual(got.Models[0].ReasoningEfforts, []string{"low", "xhigh"}) {
		t.Fatalf("unsupported or duplicate effort escaped: %v", got.Models[0].ReasoningEfforts)
	}
	for _, model := range got.Models[2:] {
		if model.Available == nil || *model.Available || !model.Restricted {
			t.Fatalf("explicit restriction was lost: %+v", model)
		}
	}
	denied, err := parseInsightBPS([]byte(`{"allowed":false,"model_catalog":{"models":[{"id":"gpt-open","available":true}]}}`), insightTestCredentials(), time.Now())
	if err != nil || denied.Models[0].Available == nil || *denied.Models[0].Available {
		t.Fatalf("account denial was overridden by model availability: %+v %v", denied, err)
	}
}

func TestInsightBPSRejectsMalformedOrConflictingCatalog(t *testing.T) {
	for _, body := range []string{
		`{"allowed":true,"model_catalog":{"models":[{"id":"gpt-duplicate"},{"id":"gpt-duplicate"}]}}`,
		`{"allowed":true,"model_catalog":{"models":[{"id":"gpt-open","available":"yes"}]}}`,
		`{"allowed":true,"model_catalog":{"models":[{"id":"gpt-open","label":"bad\nlabel"}]}}`,
		`{"allowed":true,"model_catalog":{"models":[{"id":"gpt-open","efforts":["low"]}]}}`,
		`{"allowed":true,"model_catalog":{"restricted_models":true,"models":[]}}`,
	} {
		if _, err := parseInsightBPS([]byte(body), insightTestCredentials(), time.Now()); err == nil {
			t.Fatalf("malformed catalog accepted: %s", body)
		}
	}
	if _, err := parseInsightBPS([]byte(`{"account_id":"other","allowed":false}`), insightTestCredentials(), time.Now()); !errors.Is(err, errInsightIdentity) {
		t.Fatal("BPS result from another workspace accepted")
	}
}

func insightBool(v bool) *bool { return &v }
