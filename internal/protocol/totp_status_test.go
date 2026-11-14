package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTOTPStateRecognizesExplicitDisabledResponsesWithoutInventingRemoval(t *testing.T) {
	for _, body := range []string{
		`{"mfa_enabled_v2":false}`,
		`{"mfa_enabled_v2":false,"factors":null}`,
		`{"mfa_enabled_v2":false,"factors":{}}`,
		`{"mfa_enabled_v2":false,"factors":{"totp":null}}`,
		`{"mfa_enabled_v2":false,"factors":{"totp":[]}}`,
		`{"factors":{"totp":[]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			var info map[string]any
			if err := json.Unmarshal([]byte(body), &info); err != nil {
				t.Fatal(err)
			}
			got := classifyTOTPState(info)
			if got.State != totpStateDisabled || len(got.Factors) != 0 {
				t.Fatalf("explicit disabled state not recognized: %+v", got)
			}
		})
	}
	for _, body := range []string{
		`{}`,
		`{"factors":{}}`,
		`{"factors":{"totp":null}}`,
		`{"mfa_enabled_v2":null,"factors":{"totp":[]}}`,
		`{"mfa_enabled_v2":"false","factors":{"totp":[]}}`,
		`{"mfa_enabled_v2":false,"factors":[]}`,
		`{"mfa_enabled_v2":false,"factors":"not a factor object"}`,
		`{"mfa_enabled_v2":false,"factors":{"totp":{}}}`,
		`{"mfa_enabled_v2":false,"factors":{"totp":false}}`,
		`{"mfa_enabled_v2":false,"factors":{"totp":"[]"}}`,
		`{"mfa_enabled_v2":true}`,
		`{"mfa_enabled_v2":true,"factors":{"totp":null}}`,
		`{"mfa_enabled_v2":true,"factors":{"totp":[]}}`,
		`{"mfa_enabled_v2":false,"factors":{"totp":[{"id":"old","factor_type":"totp","is_recovery":false}]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			var info map[string]any
			if err := json.Unmarshal([]byte(body), &info); err != nil {
				t.Fatal(err)
			}
			got := classifyTOTPState(info)
			if got.State != totpStateUnknown || len(got.Factors) != 0 {
				t.Fatalf("ambiguous response authorized a security transition: %+v", got)
			}
		})
	}
}

func TestTOTPStateDistinguishesStaleActiveFactorFromVerifiedRemoval(t *testing.T) {
	info := map[string]any{"mfa_enabled_v2": true, "factors": map[string]any{"totp": []any{
		map[string]any{"id": "previous-factor", "factor_type": "totp", "is_recovery": false},
	}}}
	got := classifyTOTPState(info)
	if got.State != totpStateEnabled || len(got.Factors) != 1 || got.Factors[0]["id"] != "previous-factor" {
		t.Fatalf("old factor response lost identity needed for a read-only recheck: %+v", got)
	}
	for _, factor := range []map[string]any{
		{"id": "recovery", "factor_type": "totp", "is_recovery": true},
		{"id": "missing-recovery-flag", "factor_type": "totp"},
		{"id": "other-kind", "factor_type": "sms", "is_recovery": false},
		{"id": " ", "factor_type": "totp", "is_recovery": false},
		{"id": 12, "factor_type": "totp", "is_recovery": false},
	} {
		info["factors"] = map[string]any{"totp": []any{factor}}
		if got := classifyTOTPState(info); got.State != totpStateUnknown || len(got.Factors) != 0 {
			t.Fatalf("invalid active factor accepted: %+v", got)
		}
	}
	info["factors"] = map[string]any{"totp": []any{
		map[string]any{"id": "one", "factor_type": "totp", "is_recovery": false},
		map[string]any{"id": "two", "factor_type": "totp", "is_recovery": false},
	}}
	if got := classifyTOTPState(info); got.State != totpStateUnknown || len(got.Factors) != 0 {
		t.Fatalf("multiple factors became an unambiguous active factor: %+v", got)
	}
}

func TestTOTPStateDiagnosticNeverIncludesResponseValues(t *testing.T) {
	const sensitive = "private-factor-token-seed-sentinel"
	for _, info := range []map[string]any{
		{"mfa_enabled_v2": sensitive, "factors": sensitive},
		{"mfa_enabled_v2": false, "factors": map[string]any{"totp": sensitive}},
		{"mfa_enabled_v2": true, "factors": map[string]any{"totp": []any{
			map[string]any{"id": sensitive, "factor_type": "totp", "is_recovery": false},
		}}, "error": sensitive, "access_token": sensitive, "secret": sensitive},
	} {
		got := classifyTOTPState(info)
		if strings.Contains(got.Summary, sensitive) || !strings.Contains(got.Summary, "mfa_enabled_v2=") || !strings.Contains(got.Summary, "count=") {
			t.Fatalf("unsafe or unhelpful status diagnostic: %s", got.Summary)
		}
	}
}
