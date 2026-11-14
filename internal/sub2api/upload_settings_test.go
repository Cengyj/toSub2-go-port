package sub2api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestUploadSettingsDoNotMutateInputsOrAliasCallerSlices(t *testing.T) {
	input := map[string]any{"baseUrl": "https://sub2.example.test/", "apiKey": "synthetic-key", "groupIds": []int{7, 3, 7}, "modelWhitelist": []string{"model-one", "model-two"}}
	before, _ := json.Marshal(input)
	cfg, opts, err := ParseUploadSettings(input)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIRequestTimezone != DefaultOpenAIRequestTimezone || !reflect.DeepEqual(opts.GroupIDs, []int{7, 3}) || cfg.APIKey != "synthetic-key" {
		t.Fatal("shared settings changed a supported contract")
	}
	opts.GroupIDs[0] = 99
	opts.ModelWhitelist[0] = "changed"
	after, _ := json.Marshal(input)
	if string(before) != string(after) {
		t.Fatal("parsing aliased or mutated caller-owned settings")
	}
}

func TestUploadSettingsAcceptBrowserAndExactJSONNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, values string
		populated    bool
	}{
		{"browser-defaults", `"groupIds":[],"proxyId":"","concurrency":"","loadFactor":"","priority":""`, false},
		{"browser-text", `"groupIds":["7","7"],"proxyId":"4","concurrency":"2","loadFactor":"0","priority":"3"`, true},
		{"exact-json", `"groupIds":[7.0,7e0],"proxyId":4e0,"concurrency":2.0,"loadFactor":0,"priority":3e0`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var input map[string]any
			decoder := json.NewDecoder(strings.NewReader(`{"baseUrl":"https://sub2.example.test/prefix/","adminApiKey":"synthetic-key",` + tc.values + `}`))
			decoder.UseNumber()
			if err := decoder.Decode(&input); err != nil {
				t.Fatal(err)
			}
			cfg, opts, err := ParseUploadSettings(input)
			if err != nil || cfg.BaseURL != "https://sub2.example.test/prefix" {
				t.Fatal("supported upload settings rejected", err)
			}
			if !tc.populated {
				if len(opts.GroupIDs) != 0 || opts.ProxyID != 0 || opts.Concurrency != nil || opts.LoadFactor != nil || opts.Priority != nil {
					t.Fatal("blank browser defaults became numeric overrides")
				}
				return
			}
			if !reflect.DeepEqual(opts.GroupIDs, []int{7}) || opts.ProxyID != 4 || opts.Concurrency == nil || *opts.Concurrency != 2 || opts.LoadFactor == nil || *opts.LoadFactor != 0 || opts.Priority == nil || *opts.Priority != 3 {
				t.Fatal("supported numeric options lost their exact values")
			}
		})
	}
}

func TestUploadSettingsRejectFractionalOverflowAndInvalidNumericOptions(t *testing.T) {
	for name, value := range map[string]any{
		"negative": json.Number("-1"), "fractional": json.Number("1.2"),
		"fraction-rounded-by-float64": json.Number("1.0000000000000000001"),
		"fraction-underflow":          json.Number("1e-9999"),
		"unsafe-json-integer":         json.Number("9007199254740992"),
		"oversize":                    json.Number("10001"), "text-fraction": "1.2",
		"text-overflow": "9223372036854775808", "boolean": true,
	} {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{"baseUrl": "https://sub2.example.test", "adminApiKey": "synthetic-key", "concurrency": value}
			if _, _, err := ParseUploadSettings(input); err == nil {
				t.Fatal("invalid numeric upload option accepted")
			}
		})
	}
	for _, value := range []any{json.Number("0"), json.Number("7.0000000000000000001"), "9007199254740992"} {
		input := map[string]any{"baseUrl": "https://sub2.example.test", "adminApiKey": "synthetic-key", "groupIds": []any{value}}
		if _, _, err := ParseUploadSettings(input); err == nil {
			t.Fatal("invalid group identity accepted")
		}
	}
}

func TestUploadSettingsRejectBareQueryDestination(t *testing.T) {
	if _, _, err := ParseUploadSettings(map[string]any{"baseUrl": "https://sub2.example.test?", "adminApiKey": "synthetic-key"}); err == nil {
		t.Fatal("bare query destination accepted")
	}
}
