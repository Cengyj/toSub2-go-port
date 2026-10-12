package console

import (
	"math"
	"reflect"
	"testing"
)

func TestParseTOTPSub2APIConfigMatchesConsoleSettings(t *testing.T) {
	cfg, opts, err := ParseTOTPSub2APIConfig(map[string]any{
		"baseUrl": " https://sub2.example.test/prefix/ ", "adminApiKey": "private-admin-key",
		"groupIds": []any{"110001", float64(7), "110001"}, "proxyId": "",
		"concurrency": "", "loadFactor": "", "priority": "0",
		"modelWhitelist": "model-one, model-two\nmodel-one", "codexFingerprintMode": " SESSION ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://sub2.example.test/prefix" || cfg.APIKey != "private-admin-key" || !reflect.DeepEqual(opts.GroupIDs, []int{110001, 7}) || opts.Priority == nil || *opts.Priority != 0 || opts.Concurrency != nil || opts.LoadFactor != nil || opts.ProxyID != 0 || opts.CodexFingerprintMode != "session" || !reflect.DeepEqual(opts.ModelWhitelist, []string{"model-one", "model-two"}) {
		t.Fatal("console settings changed during validation", cfg, opts)
	}
}

func TestParseTOTPSub2APIConfigRejectsInvalidValues(t *testing.T) {
	for name, changed := range map[string]map[string]any{
		"url-scheme":   {"baseUrl": "ftp://sub2.example.test"},
		"url-userinfo": {"baseUrl": "https://user:pass@sub2.example.test"},
		"url-query":    {"baseUrl": "https://sub2.example.test?apiKey=value"},
		"url-fragment": {"baseUrl": "https://sub2.example.test/#value"},
		"empty-key":    {"adminApiKey": " "}, "key-newline": {"adminApiKey": "value\r\nheader:value"},
		"negative-priority": {"priority": "-1"}, "oversize-concurrency": {"concurrency": "10001"},
		"fractional-proxy": {"proxyId": 1.2}, "nan": {"loadFactor": math.NaN()},
		"number-boolean": {"loadFactor": true}, "group-blank": {"groupIds": []string{""}},
		"group-zero": {"groupIds": []any{float64(0)}}, "group-object": {"groupIds": map[string]any{}},
		"model-nontext": {"modelWhitelist": []any{"one", 2}}, "fingerprint": {"codexFingerprintMode": "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{"baseUrl": "https://sub2.example.test", "adminApiKey": "private-admin-key"}
			for key, value := range changed {
				input[key] = value
			}
			if _, _, err := ParseTOTPSub2APIConfig(input); err == nil {
				t.Fatal("invalid upload setting accepted")
			}
		})
	}
}
