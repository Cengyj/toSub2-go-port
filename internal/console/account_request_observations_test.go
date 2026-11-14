package console

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
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

func insightObservationManager(t *testing.T) (*jobs.Manager, string) {
	t.Helper()
	m := jobs.NewManager(t.Context(), 1, nil, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	j, err := m.Submit(jobs.Spec{ID: "synthetic-request", Email: "request@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return m, j.ID
}

func TestInsightRequestObservationsKeepAllErrorsAndOnlyClearAttemptedSuccess(t *testing.T) {
	m, id := insightObservationManager(t)
	for _, source := range []string{"usage", "bps", "models", "resetCredits"} {
		if err := m.BeginAccountRequest(id, source).Complete(jobs.AccountRequestObservation{Status: "failed", HTTPStatus: 429, ErrorCode: "rate_limited"}); err != nil {
			t.Fatal(err)
		}
	}
	r := beginInsightRequestObservations(m, id)
	result := protocol.InsightResult{AttemptedComponents: []string{"usage", "subscription", "bps"}, Usage: &protocol.InsightUsage{}, Errors: []protocol.InsightError{
		{Component: "subscription", Code: "permission_denied", HTTPStatus: 403},
		{Component: "subscription", Code: "upstream_error", HTTPStatus: 503},
		{Component: "bps", Code: "canceled"},
	}}
	completeInsightRequestObservations(t.Context(), func() error { return nil }, r, result)
	j, _ := m.Get(id)
	if len(j.AccountRequestObservations) != 6 {
		t.Fatal("source batch was lost", j.AccountRequestObservations)
	}
	for _, o := range j.AccountRequestObservations {
		switch o.Source {
		case "usage":
			if o.Status != "success" {
				t.Fatal("verified success did not clear its source", o)
			}
		case "subscription":
			if o.HTTPStatus != 403 && o.HTTPStatus != 503 {
				t.Fatal("query error altered", o)
			}
		default:
			if o.HTTPStatus != 429 {
				t.Fatal("skipped/canceled/unrelated source cleared", o)
			}
		}
	}
}

func TestInsightRequestObservationsDoNotClearOnCancellationReplacementOrMissingData(t *testing.T) {
	for _, kind := range []string{"cancel", "replaced", "empty", "not-attempted"} {
		t.Run(kind, func(t *testing.T) {
			m, id := insightObservationManager(t)
			_ = m.BeginAccountRequest(id, "usage").Complete(jobs.AccountRequestObservation{Status: "failed", HTTPStatus: 401})
			before, _ := m.Get(id)
			r := beginInsightRequestObservations(m, id)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			validate := func() error { return nil }
			result := protocol.InsightResult{AttemptedComponents: []string{"usage"}, Usage: &protocol.InsightUsage{}}
			switch kind {
			case "cancel":
				cancel()
			case "replaced":
				validate = func() error { return errors.New("synthetic superseded grant") }
			case "empty":
				result.Usage = nil
			case "not-attempted":
				result.AttemptedComponents = nil
			}
			completeInsightRequestObservations(ctx, validate, r, result)
			after, _ := m.Get(id)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("unconfirmed query changed observations")
			}
		})
	}
}
