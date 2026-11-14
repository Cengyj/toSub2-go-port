package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAccountRequestIndependentSourcesAndSuccessTombstone(t *testing.T) {
	m, _ := accountQueryManager(t)
	j := accountQueryJob(t, m, "request-sources", "synthetic@example.test")
	before, _ := m.Get(j.ID)
	lease := accountQueryLease(t, m, t.Context(), j.ID)
	for _, source := range []string{"subscription", "models"} {
		if err := m.BeginAccountRequest(j.ID, source).Complete(AccountRequestObservation{Status: "failed", HTTPStatus: 403, ErrorCode: "forbidden", Message: "PRIVATE-CANARY"}, AccountRequestObservation{Status: "failed", HTTPStatus: 503, ErrorCode: "upstream_error"}, AccountRequestObservation{Status: "success", HTTPStatus: 200}); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := m.Get(j.ID)
	if len(after.AccountRequestObservations) != 4 || !lease.Valid() {
		t.Fatal("source errors missing or query lease invalidated", after.AccountRequestObservations)
	}
	copy := cloneJob(after)
	copy.AccountRequestObservations = nil
	if !reflect.DeepEqual(before, copy) {
		t.Fatal("diagnostics changed business history")
	}
	if err := m.BeginAccountRequest(j.ID, "subscription").Complete(AccountRequestObservation{Status: "success"}); err != nil {
		t.Fatal(err)
	}
	after, _ = m.Get(j.ID)
	if len(after.AccountRequestObservations) != 3 {
		t.Fatal("success erased another source", after.AccountRequestObservations)
	}
	for _, o := range after.AccountRequestObservations {
		if o.Source == "subscription" && (o.Status != "success" || o.HTTPStatus != 0) {
			t.Fatal("success tombstone missing", o)
		}
	}
	after.AccountRequestObservations[0].Message = "mutated"
	again, _ := m.Get(j.ID)
	if again.AccountRequestObservations[0].Message == "mutated" {
		t.Fatal("public slice aliases manager")
	}
}

func TestAccountRequestRejectsStaleAndRecreatedAccount(t *testing.T) {
	for _, change := range []string{"newer-source", "new-operation", "new-attempt", "new-method", "recreated", "deleted"} {
		t.Run(change, func(t *testing.T) {
			m, _ := accountQueryManager(t)
			j := accountQueryJob(t, m, "request-order", "synthetic@example.test")
			r := m.BeginAccountRequest(j.ID, "models")
			if change == "newer-source" {
				n := m.BeginAccountRequest(j.ID, "models")
				if !n.startedAt.After(r.startedAt) {
					t.Fatal("request starts lack a strict order")
				}
				if err := n.Complete(AccountRequestObservation{Status: "success"}); err != nil {
					t.Fatal(err)
				}
			} else {
				m.mu.Lock()
				current := m.jobs[j.ID]
				switch change {
				case "new-operation":
					current.LastOperationAt = time.Now()
				case "new-attempt":
					current.Attempts++
				case "new-method":
					current.AuthorizationMethod = "bps"
				case "recreated":
					m.jobs[j.ID] = cloneJob(current)
				case "deleted":
					delete(m.jobs, j.ID)
				}
				m.mu.Unlock()
			}
			if err := r.Complete(AccountRequestObservation{Status: "failed", HTTPStatus: 401}); !errors.Is(err, ErrAccountRequestSuperseded) {
				t.Fatal("stale diagnostic accepted", err)
			}
		})
	}
}

func TestAccountRequestPersistsSafeBoundedDiagnostics(t *testing.T) {
	m, st := accountQueryManager(t)
	j := accountQueryJob(t, m, "request-persist", "synthetic@example.test")
	batch := []AccountRequestObservation{{Status: "success", HTTPStatus: 200}}
	for i := 0; i < 40; i++ {
		batch = append(batch, AccountRequestObservation{Status: "failed", HTTPStatus: 400 + i, ErrorCode: "PRIVATE-CODE-CANARY", Message: "PRIVATE-BODY-CANARY"})
	}
	if err := m.BeginAccountRequest(j.ID, "models").Complete(batch...); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := st.ReadJSON(t.Context(), "job-meta.json", &data); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(data)
	if strings.Contains(string(b), "CANARY") {
		t.Fatal("private error escaped to disk")
	}
	n := NewManager(t.Context(), 1, st, nil)
	t.Cleanup(func() { _ = n.Shutdown(context.Background()) })
	if err := n.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored, ok := n.Get(j.ID)
	if !ok || len(restored.AccountRequestObservations) != 8 {
		t.Fatal("diagnostics did not restore bounded batch", restored)
	}
	for _, o := range restored.AccountRequestObservations {
		if o.Status != "failed" || o.ErrorCode != "request_failed" {
			t.Fatal("failed batch swallowed or code unsafe", o)
		}
	}
}

func TestAccountRequestPreservesSafeProbeResponseClassifications(t *testing.T) {
	m, st := accountQueryManager(t)
	job := accountQueryJob(t, m, "probe-response-diagnostics", "synthetic@example.test")
	codes := []string{"unexpected_content_type", "invalid_stream", "invalid_answer_format"}
	batch := make([]AccountRequestObservation, 0, len(codes))
	for _, code := range codes {
		batch = append(batch, AccountRequestObservation{Status: "failed", HTTPStatus: 200, ErrorCode: code, Message: "PRIVATE-RESPONSE-CANARY"})
	}
	if err := m.BeginAccountRequest(job.ID, "candy").Complete(batch...); err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	if err := st.ReadJSON(t.Context(), "job-meta.json", &persisted); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(persisted)
	if strings.Contains(string(data), "PRIVATE-RESPONSE-CANARY") {
		t.Fatal("response classification persisted provider content")
	}
	restored := NewManager(t.Context(), 1, st, nil)
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _ := restored.Get(job.ID)
	if len(current.AccountRequestObservations) != len(codes) {
		t.Fatal("response diagnostics disappeared during persistence")
	}
	for index, observation := range current.AccountRequestObservations {
		if observation.ErrorCode != codes[index] || observation.HTTPStatus != 200 || observation.Status != "failed" || observation.Message != "账号响应未通过完整性校验" {
			t.Fatal("response failure lost its safe diagnosis or became a successful request")
		}
	}
}

func TestAccountRequestMalformedMetadataDoesNotDiscardJob(t *testing.T) {
	for _, diagnostic := range []string{`"bad"`, `[{"httpStatus":"bad"}]`, `[{"source":"PRIVATE-SOURCE","status":"failed","finishedAt":"2026-11-06T00:00:00Z"}]`} {
		job, ok := decodeLegacyJob([]byte(`{"id":"safe-job","email":"synthetic@example.test","account_request_observations":`+diagnostic+`}`), "safe-job")
		if !ok || len(job.AccountRequestObservations) != 0 {
			t.Fatal("bad optional diagnostic discarded job or leaked data")
		}
	}
}

func TestAccountRequestPersistenceFailurePreservesBusinessResult(t *testing.T) {
	m, _ := accountQueryManager(t)
	j := accountQueryJob(t, m, "request-write-fail", "synthetic@example.test")
	sentinel := errors.New("synthetic disk full")
	m.metadataWriter = func(context.Context, string, any) error { return sentinel }
	r := m.BeginAccountRequest(j.ID, "models")
	if err := r.Complete(AccountRequestObservation{Status: "failed", HTTPStatus: 200, ErrorCode: "invalid_response"}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	got, _ := m.Get(j.ID)
	if len(got.AccountRequestObservations) != 1 || got.AccountRequestObservations[0].HTTPStatus != 200 || got.Status != j.Status || !got.UpdatedAt.Equal(j.UpdatedAt) {
		t.Fatal("diagnostic write changed business state", got)
	}
	m.metadataWriter = nil
	if err := r.Complete(AccountRequestObservation{Status: "success"}); !errors.Is(err, ErrAccountRequestSuperseded) {
		t.Fatal("same completion overwrote result", err)
	}
}

func TestAccountRequestRejectsChangedQueryLeaseAtCommit(t *testing.T) {
	m, _ := accountQueryManager(t)
	j := accountQueryJob(t, m, "request-lease", "synthetic@example.test")
	lease := accountQueryLease(t, m, t.Context(), j.ID)
	r := m.BeginAccountRequest(j.ID, "models")
	if !lease.Valid() {
		t.Fatal("fixture lease invalid")
	}
	if _, err := m.Update(j.ID, map[string]any{"proxyUrl": "http://proxy.example.test:8080"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Complete(AccountRequestObservation{Status: "failed", HTTPStatus: 403}); !errors.Is(err, ErrAccountRequestSuperseded) {
		t.Fatal("changed grant admitted diagnostic", err)
	}
}

func TestAccountRequestRejectsMalformedSuccessAndInvalidSources(t *testing.T) {
	m, _ := accountQueryManager(t)
	j := accountQueryJob(t, m, "request-invalid", "synthetic@example.test")
	_ = m.BeginAccountRequest(j.ID, "models").Complete(AccountRequestObservation{Status: "failed", HTTPStatus: 403})
	before, _ := m.Get(j.ID)
	for _, status := range []int{100, 302, 403, 503, 999} {
		_ = m.BeginAccountRequest(j.ID, "models").Complete(AccountRequestObservation{Status: "success", HTTPStatus: status})
	}
	if r := m.BeginAccountRequest(j.ID, "https://private.example/token"); r != nil {
		t.Fatal("unrecognized source accepted")
	}
	var none *AccountRequestRecorder
	if err := none.Complete(AccountRequestObservation{Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	after, _ := m.Get(j.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("invalid success cleared failure")
	}
}

func TestAccountRequestRestoredClockAndHistoricalScope(t *testing.T) {
	m, st := accountQueryManager(t)
	j := accountQueryJob(t, m, "request-restored-clock", "synthetic@example.test")
	future := time.Now().UTC().Add(time.Hour)
	m.mu.Lock()
	m.jobs[j.ID].AccountRequestObservations = []AccountRequestObservation{{Source: "models", Status: "failed", HTTPStatus: 403, ErrorCode: "forbidden", StartedAt: future, FinishedAt: future}}
	m.persistLocked()
	m.mu.Unlock()
	n := NewManager(t.Context(), 1, st, nil)
	t.Cleanup(func() { _ = n.Shutdown(context.Background()) })
	if err := n.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := n.BeginAccountRequest(j.ID, "models")
	if !r.startedAt.After(future) {
		t.Fatal("restored timestamp blocks later requests")
	}
	if err := r.Complete(AccountRequestObservation{Status: "success"}); err != nil {
		t.Fatal(err)
	}
	got, _ := n.Get(j.ID)
	if len(got.AccountRequestObservations) != 1 || got.AccountRequestObservations[0].Status != "success" {
		t.Fatal("restored error could not be replaced")
	}
	n.mu.Lock()
	n.jobs[j.ID].LastOperationAt = r.startedAt.Add(time.Second)
	n.mu.Unlock()
	got, _ = n.Get(j.ID)
	got = SanitizeJobForPublic(got)
	if !got.AccountRequestObservations[0].Historical {
		t.Fatal("previous authorization observation presented as current")
	}
}

func TestAccountRequestBatchKeepsActualOrderAndLastDuplicate(t *testing.T) {
	for _, scenario := range []struct {
		name           string
		statuses, want []int
	}{
		{"newer-low-code", []int{503, 403}, []int{503, 403}},
		{"repeated-low-code", []int{403, 503, 403}, []int{503, 403}},
		{"repeated-high-code", []int{503, 403, 503}, []int{403, 503}},
		{"bounded-newest", []int{403, 500, 501, 502, 503, 504, 505, 506, 507, 508, 509, 403}, []int{503, 504, 505, 506, 507, 508, 509, 403}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			m, st := accountQueryManager(t)
			j := accountQueryJob(t, m, "request-observed-order", "synthetic@example.test")
			var batch []AccountRequestObservation
			for _, status := range scenario.statuses {
				batch = append(batch, AccountRequestObservation{Status: "failed", HTTPStatus: status, ErrorCode: "http_error"})
			}
			if err := m.BeginAccountRequest(j.ID, "subscription").Complete(batch...); err != nil {
				t.Fatal(err)
			}
			assertOrder := func(job *Job) {
				t.Helper()
				var statuses []int
				for _, observation := range job.AccountRequestObservations {
					statuses = append(statuses, observation.HTTPStatus)
				}
				if !reflect.DeepEqual(statuses, scenario.want) {
					t.Fatalf("response order changed: got %v want %v", statuses, scenario.want)
				}
			}
			got, _ := m.Get(j.ID)
			assertOrder(got)
			assertOrder(SanitizeJobForPublic(got))
			n := NewManager(t.Context(), 1, st, nil)
			t.Cleanup(func() { _ = n.Shutdown(context.Background()) })
			if err := n.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			restored, _ := n.Get(j.ID)
			assertOrder(restored)
		})
	}
}

func TestAccountRequestSanitizerPreservesInterleavedSourceOrder(t *testing.T) {
	now := time.Now().UTC()
	observations := []AccountRequestObservation{
		{Source: "models", Status: "failed", HTTPStatus: 503, ErrorCode: "upstream_error", StartedAt: now, FinishedAt: now},
		{Source: "subscription", Status: "failed", HTTPStatus: 403, ErrorCode: "forbidden", StartedAt: now, FinishedAt: now},
		{Source: "models", Status: "failed", HTTPStatus: 401, ErrorCode: "unauthorized", StartedAt: now, FinishedAt: now},
	}
	got := sanitizeAccountRequestObservations(observations)
	if len(got) != 3 || got[0].HTTPStatus != 503 || got[1].HTTPStatus != 403 || got[2].HTTPStatus != 401 {
		t.Fatalf("equal timestamps were reordered by source or HTTP number: %+v", got)
	}
	if again := sanitizeAccountRequestObservations(got); !reflect.DeepEqual(got, again) {
		t.Fatal("public sanitization changed stored observation order")
	}
}
