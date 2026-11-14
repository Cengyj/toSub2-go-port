package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func TestManualFailureRecordRetainsLocalizedSafeHTTPDiagnostic(t *testing.T) {
	for _, method := range []string{jobs.AuthorizationMethodChatGPT, jobs.AuthorizationMethodBPS} {
		for _, tc := range []struct {
			name, code string
			status     int
			network    bool
		}{
			{"unauthorized", "unauthorized", 401, false}, {"permission", "forbidden", 403, false}, {"limited", "rate_limited", 429, false}, {"upstream", "http_error", 503, false}, {"svg-format", "invalid_response", 200, false}, {"network", "network_error", 0, true},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				f, c := newQualityControllerFixture(t, method)
				c.StartManualProbes(t.Context())
				enableQualityFixture(t, c, nil)
				var calls atomic.Int32
				c.transportFactory = func(string) (protocol.Doer, func(), error) {
					return qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
						calls.Add(1)
						if tc.network {
							return nil, errors.New("synthetic-private Authorization: Bearer secret")
						}
						if tc.status == 200 {
							return manualResponse(r, `<html><body><script>synthetic-private`), nil
						}
						return &transport.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"error":"synthetic-private upstream diagnostic"}`)}, nil
					}), func() {}, nil
				}
				if _, err := c.StartManualProbe(t.Context(), f.job.ID, "manual-error-0001", modelprobe.PelicanTask); err != nil {
					t.Fatal(err)
				}
				record := waitManual(t, c, f.job.ID)
				if record.State != "failed" || record.ErrorCode != tc.code || record.HTTPStatus != tc.status || record.Message == "" || record.SVG != "" {
					t.Fatalf("manual diagnostic lost: %+v", record)
				}
				encoded, _ := json.Marshal(record)
				for _, private := range []string{"synthetic-private", "Authorization", "model probe"} {
					if strings.Contains(string(encoded), private) {
						t.Fatal("manual failure leaked raw text or remained untranslated")
					}
				}
				duplicate, err := c.StartManualProbe(t.Context(), f.job.ID, "manual-error-0001", modelprobe.PelicanTask)
				if err != nil || duplicate.HTTPStatus != tc.status || duplicate.ErrorCode != tc.code || calls.Load() != 1 {
					t.Fatal("failure readback lost diagnostics or repeated inference")
				}
				rows, _ := c.Service.Snapshots([]string{f.job.ID})
				if rows[f.job.ID].LastAttempt.Status != "never" {
					t.Fatal("manual failure changed automatic probe record")
				}
			})
		}
	}
}
