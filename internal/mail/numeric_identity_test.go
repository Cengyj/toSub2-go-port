package mail

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNumericMessageIdentityPreservesLargeIntegersAndLegacyKeys(t *testing.T) {
	for _, ids := range [][2]string{
		{"9007199254740992", "9007199254740993"},
		{"18446744073709551616", "18446744073709551617"},
	} {
		first := ExtractCandidates(`{"id":` + ids[0] + `,"code":654321,"received_at":1790330460000}`)
		second := ExtractCandidates(`{"id":` + ids[1] + `,"code":654321,"received_at":1790330460000}`)
		if len(first) != 1 || len(second) != 1 {
			t.Fatal("numeric code or timestamp was lost")
		}
		if first[0].Key == second[0].Key || first[0].LegacyKey != second[0].LegacyKey || first[0].LegacyKey == "" {
			t.Fatalf("message identities were not separated with a legacy alias: %#v %#v", first, second)
		}
		if first[0].ReceivedAt != 1790330460000 || second[0].ReceivedAt != 1790330460000 {
			t.Fatal("numeric timestamps changed")
		}
		data, err := json.Marshal(first[0])
		if err != nil || strings.Contains(string(data), first[0].LegacyKey) {
			t.Fatal("compatibility alias escaped into candidate JSON", err)
		}
	}
}

func TestNumericMessageIdentityKeepsExistingRepresentations(t *testing.T) {
	for _, tc := range []struct{ number, legacy string }{
		{"12345", "12345"},
		{"1234567890", "1.23456789e+09"},
		{"9007199254740991", "9.007199254740991e+15"},
		{"12345.5", "12345.5"},
		{"1e3", "1000"},
	} {
		got := ExtractCandidates(`{"id":` + tc.number + `,"code":654321}`)
		want := ExtractCandidates(`{"id":"` + tc.legacy + `","code":"654321"}`)
		if len(got) != 1 || len(want) != 1 || got[0].Key != want[0].Key || got[0].LegacyKey != "" {
			t.Fatalf("existing candidate identity changed for %s: %#v %#v", tc.number, got, want)
		}
	}
}
