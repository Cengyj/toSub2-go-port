package cpa

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestSchedulingSettingsValidateDefaultsAndExplicitZero(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"baseUrl": "https://cpa.example.test", "adminApiKey": "key"}
	}
	cfg, err := ParseUploadSettings(base())
	if err != nil || cfg.Priority != 0 || cfg.EffectiveWeight() != 1 {
		t.Fatal("legacy options changed routing defaults", err)
	}
	for _, weight := range []any{0, int64(0), float64(0), json.Number("0"), 1_000_000} {
		values := base()
		values["weight"], values["priority"] = weight, json.Number("-2147483648")
		cfg, err := ParseUploadSettings(values)
		if err != nil || cfg.Priority != math.MinInt32 {
			t.Fatal("valid scheduling setting rejected", err)
		}
		if weight != 1_000_000 && cfg.EffectiveWeight() != 0 {
			t.Fatal("explicit zero weight became default")
		}
	}
	for _, field := range []string{"priority", "weight"} {
		bad := []any{nil, true, "2", 1.5, math.NaN(), math.Inf(1), json.Number("1.5"), int64(math.MaxInt32) + 1}
		if field == "weight" {
			bad = append(bad, -1, 1_000_001)
		} else {
			bad = append(bad, int64(math.MinInt32)-1)
		}
		for _, value := range bad {
			values := base()
			values[field] = value
			_, err := ParseUploadSettings(values)
			want := ErrInvalidPriority
			if field == "weight" {
				want = ErrInvalidWeight
			}
			if !errors.Is(err, want) || SafeError(err) != want.Error() {
				t.Fatalf("invalid %s (%T) accepted or obscured: %v", field, value, err)
			}
		}
	}
}

func TestUploadAppliesFrozenSchedulingWithoutMutatingGrant(t *testing.T) {
	for _, weight := range []int{0, 3, 1_000_000} {
		t.Run(strconv.Itoa(weight), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var doc map[string]any
				if json.NewDecoder(r.Body).Decode(&doc) != nil || doc["priority"] != float64(-12) || doc["weight"] != float64(weight) {
					t.Error("scheduling settings not applied to outgoing authorization")
				}
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			}))
			defer server.Close()
			file, err := BuildAuthFile(testAccount())
			if err != nil {
				t.Fatal(err)
			}
			original := string(file.Data)
			setting := weight
			client := New(Config{BaseURL: server.URL, APIKey: "key", UploadOptions: UploadOptions{Priority: -12, Weight: &setting}})
			setting = 99
			if err := client.Upload(t.Context(), file); err != nil || calls.Load() != 1 || string(file.Data) != original {
				t.Fatal("upload lost frozen settings or mutated saved grant", err)
			}
			invalid := 1_000_001
			err = New(Config{BaseURL: server.URL, APIKey: "key", UploadOptions: UploadOptions{Weight: &invalid}}).Upload(t.Context(), file)
			if !errors.Is(err, ErrInvalidWeight) || IsMutationOutcomeUnknown(err) || calls.Load() != 1 {
				t.Fatal("invalid scheduling reached upstream", err)
			}
		})
	}
}
