package accountquality

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"tosub2-go-port/internal/modelprobe"
)

func TestFailedAttemptRetainsSafeHTTPDiagnosticAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{
		{"unauthorized", 401}, {"forbidden", 403}, {"rate_limited", 429}, {"http_error", 503}, {"invalid_response", 200}, {"incomplete", 200}, {"network_error", 0},
	} {
		t.Run(tc.code, func(t *testing.T) {
			s, f, root := newFixture(t)
			calls := 0
			_, err := s.CheckForUpload(t.Context(), "a", upload("grant-a", func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
				calls++
				return modelprobe.Result{}, fmt.Errorf("synthetic-private upstream: %w", &modelprobe.Error{Code: tc.code, HTTPStatus: tc.status})
			}))
			if err == nil || calls != 1 {
				t.Fatal("failed probe returned success or retried")
			}
			rows, _ := s.Snapshots([]string{"a"})
			attempt := rows["a"].LastAttempt
			if attempt.Status != "failed" || attempt.ErrorCode != tc.code || attempt.HTTPStatus != tc.status || attempt.Message == "" {
				t.Fatalf("diagnostic lost: %+v", attempt)
			}
			encoded, _ := json.Marshal(rows["a"])
			if strings.Contains(string(encoded), "synthetic-private") || strings.Contains(attempt.Message, "model probe") {
				t.Fatal("failure diagnostic was unsafe or not localized")
			}
			restored, err := New(root, fixtureProvider(f))
			if err != nil {
				t.Fatal(err)
			}
			restoredRows, _ := restored.Snapshots([]string{"a"})
			if restoredRows["a"].LastAttempt.HTTPStatus != tc.status || restoredRows["a"].LastAttempt.Message != attempt.Message {
				t.Fatal("restart lost persisted HTTP diagnostic")
			}
			if _, err := s.CheckForUpload(t.Context(), "a", upload("grant-a", good)); err != nil {
				t.Fatal(err)
			}
			rows, _ = s.Snapshots([]string{"a"})
			if rows["a"].LastAttempt.HTTPStatus != 0 || rows["a"].LastAttempt.ErrorCode != "" {
				t.Fatal("successful attempt retained an old HTTP error")
			}
		})
	}
}
