package console

import (
	"context"
	"encoding/json"
	"testing"
	"time"
	"tosub2-go-port/internal/jobs"
)

func TestAccountRequestPublicJobCarriesSafeObservations(t *testing.T) {
	s, m := newMonitorTestServer(t)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	now := time.Now().UTC()
	j, err := m.Submit(jobs.Spec{ID: "synthetic-public-request", Email: "request@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	j.LastOperationAt = now
	j.AccountRequestObservations = []jobs.AccountRequestObservation{{Source: "models", Status: "failed", HTTPStatus: 403, ErrorCode: "forbidden", Message: "PRIVATE-ERROR-CANARY", StartedAt: now.Add(-time.Minute), FinishedAt: now.Add(-time.Second)}}
	for _, public := range []map[string]any{s.publicJob(j), s.publicSelectionJob(j)} {
		data, err := json.Marshal(public)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Observations []jobs.AccountRequestObservation `json:"accountRequestObservations"`
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if len(decoded.Observations) != 1 || decoded.Observations[0].HTTPStatus != 403 || !decoded.Observations[0].Historical || decoded.Observations[0].Message == "PRIVATE-ERROR-CANARY" {
			t.Fatal("public job dropped or exposed raw diagnostic", decoded)
		}
	}
}
