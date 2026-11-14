package accountquality

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/modelprobe"
)

func projectedHTTP(t *testing.T, s *Service) *HTTPObservation {
	t.Helper()
	rows, _ := s.Snapshots([]string{"a"})
	return rows["a"].HTTPObservation
}

func refreshHTTP(t *testing.T, s *Service, f *fixture, probe func(context.Context, modelprobe.Options) (modelprobe.Result, error)) {
	t.Helper()
	f.mu.Lock()
	f.probe = probe
	f.mu.Unlock()
	receipt, err := s.Refresh(t.Context(), []string{"a"}, false)
	if err != nil || len(receipt.Accepted) != 1 {
		t.Fatalf("probe was not admitted: %+v %v", receipt, err)
	}
	idle(t, s)
}

func TestHTTPObservationRetainsExplicitFailureUntilCompleteValidAnswer(t *testing.T) {
	s, f, root := newFixture(t)
	_, err := s.CheckForUpload(t.Context(), "a", upload("grant-a", func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
		return modelprobe.Result{}, fmt.Errorf("synthetic-private upstream: %w", &modelprobe.Error{Code: "rate_limited", HTTPStatus: 429})
	}))
	if err == nil {
		t.Fatal("failed probe returned success")
	}
	failure := projectedHTTP(t, s)
	if failure == nil || failure.Status != "failed" || failure.HTTPStatus != 429 || failure.ErrorCode != "rate_limited" || failure.Source != "candy" || failure.Model != "synthetic-model" || failure.FinishedAt.IsZero() || strings.Contains(failure.Message, "synthetic-private") {
		t.Fatalf("explicit failure was lost or exposed private text: %+v", failure)
	}
	assertRetained := func(stage string) {
		t.Helper()
		if got := projectedHTTP(t, s); !reflect.DeepEqual(got, failure) {
			t.Fatalf("%s changed prior explicit observation: got %+v; want %+v", stage, got, failure)
		}
	}

	started, release := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.probe = func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
		close(started)
		<-release
		return modelprobe.Result{}, errors.New("synthetic-private missing response")
	}
	f.mu.Unlock()
	s.runGate <- struct{}{}
	receipt, err := s.Refresh(t.Context(), []string{"a"}, false)
	if err != nil || len(receipt.Accepted) != 1 {
		<-s.runGate
		close(release)
		t.Fatal("queued probe not admitted", err)
	}
	assertRetained("queued")
	<-s.runGate
	waitFor(t, func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})
	assertRetained("running")
	close(release)
	idle(t, s)
	assertRetained("unknown response")

	for _, tc := range []struct {
		name   string
		result modelprobe.Result
		err    error
	}{
		{name: "invalid result without error"},
		{name: "invalid HTTP 200", err: &modelprobe.Error{Code: "invalid_response", HTTPStatus: 200}},
		{name: "incomplete HTTP 200", err: &modelprobe.Error{Code: "incomplete", HTTPStatus: 200}},
		{name: "missing HTTP response", err: &modelprobe.Error{Code: "network_error"}},
		{name: "semantic failure without observed HTTP", err: &modelprobe.Error{Code: "unauthorized"}},
		{name: "timeout", err: context.DeadlineExceeded},
		{name: "cancelled", err: context.Canceled},
	} {
		refreshHTTP(t, s, f, func(context.Context, modelprobe.Options) (modelprobe.Result, error) { return tc.result, tc.err })
		assertRetained(tc.name)
	}

	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored, err := New(root, fixtureProvider(f))
	if err != nil {
		t.Fatal(err)
	}
	if got := projectedHTTP(t, restored); !reflect.DeepEqual(got, failure) {
		t.Fatalf("restart lost prior explicit observation: %+v", got)
	}
	restored.Start(context.Background())
	t.Cleanup(func() { _ = restored.Close(context.Background()) })
	refreshHTTP(t, restored, f, func(_ context.Context, opts modelprobe.Options) (modelprobe.Result, error) {
		return resultFor(opts, 0), nil
	})
	success := projectedHTTP(t, restored)
	if success == nil || success.Status != "success" || success.HTTPStatus != 200 || success.ErrorCode != "" || !success.FinishedAt.After(failure.FinishedAt) || success.Model != "synthetic-model" {
		t.Fatalf("valid wrong answer did not replace transport failure with HTTP success: %+v", success)
	}
	rows, _ := restored.Snapshots([]string{"a"})
	if rows["a"].LastResult == nil || rows["a"].LastResult.Score != 0 || rows["a"].LastResult.Verdict != "degraded" {
		t.Fatal("transport success changed the independent candy verdict")
	}
	success.Model = "mutated-return-value"
	if projectedHTTP(t, restored).Model != "synthetic-model" {
		t.Fatal("snapshot exposed the stored observation pointer")
	}
}

func TestHTTPObservationAuthorizationIsIndependentFromNewAdmission(t *testing.T) {
	s, f, root := newFixture(t)
	refreshHTTP(t, s, f, func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
		return modelprobe.Result{}, &modelprobe.Error{Code: "forbidden", HTTPStatus: 403}
	})
	want := projectedHTTP(t, s)
	if want == nil {
		t.Fatal("missing initial observation")
	}
	setAccount := func(accounts []Account) {
		f.mu.Lock()
		f.accounts = accounts
		f.mu.Unlock()
	}
	original := Account{ID: "a", Key: "email-a", Revision: "r1", CanCheck: true}
	setAccount(nil)
	if projectedHTTP(t, s) != nil {
		t.Fatal("recycled account exposed a transport observation")
	}
	setAccount([]Account{original})
	if !reflect.DeepEqual(projectedHTTP(t, s), want) {
		t.Fatal("restore with the same authorization lost observation")
	}
	unavailable := original
	unavailable.CanCheck = false
	setAccount([]Account{unavailable})
	if projectedHTTP(t, s) != nil {
		t.Fatal("unavailable authorization exposed observation")
	}
	changed := original
	changed.Revision = "r2"
	setAccount([]Account{changed})
	if projectedHTTP(t, s) != nil {
		t.Fatal("new authorization inherited old observation")
	}
	refreshHTTP(t, s, f, func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
		return modelprobe.Result{}, &modelprobe.Error{Code: "network_error"}
	})
	s.mu.Lock()
	row := s.rows["a"]
	s.mu.Unlock()
	if row.AccountRevision != "r2" || row.HTTPObservationRevision != "r1" || !reflect.DeepEqual(row.HTTPObservation, want) || projectedHTTP(t, s) != nil {
		t.Fatal("new failed admission rebound the older transport observation")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored, err := New(root, fixtureProvider(f))
	if err != nil {
		t.Fatal(err)
	}
	if projectedHTTP(t, restored) != nil {
		t.Fatal("restart rebound old observation to latest admission revision")
	}
	setAccount([]Account{original})
	if !reflect.DeepEqual(projectedHTTP(t, restored), want) {
		t.Fatal("retained observation was invalidated rather than hidden")
	}
}

func TestLegacyHTTPObservationBackfillIsBoundedAndReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name, status, code, revision string
		httpStatus                   int
		finish                       string
		wantStatus                   string
	}{
		{name: "success", status: "success", wantStatus: "success"},
		{name: "unauthorized", status: "failed", code: "unauthorized", httpStatus: 401, wantStatus: "failed"},
		{name: "real unknown code", status: "failed", code: "synthetic-private", httpStatus: 503, wantStatus: "failed"},
		{name: "legacy unauthorized", status: "failed", code: "unauthorized", wantStatus: "failed"},
		{name: "legacy forbidden", status: "failed", code: "forbidden", wantStatus: "failed"},
		{name: "legacy rate limit", status: "failed", code: "rate_limited", wantStatus: "failed"},
		{name: "unknown", status: "failed", code: "network_error"},
		{name: "invalid HTTP 200", status: "failed", code: "invalid_response", httpStatus: 200},
		{name: "incomplete HTTP 200", status: "failed", code: "incomplete", httpStatus: 200},
		{name: "cancelled", status: "cancelled", code: "forbidden", httpStatus: 403},
		{name: "running", status: "running", code: "unauthorized", httpStatus: 401},
		{name: "queued", status: "queued", code: "unauthorized", httpStatus: 401},
		{name: "missing finish", status: "success", finish: "missing"},
		{name: "zero finish", status: "success", finish: "zero"},
		{name: "future finish", status: "success", finish: "future"},
		{name: "changed authorization", status: "success", revision: "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, root := newFixture(t)
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			finished := time.Now().Add(-time.Second).UTC()
			attempt := Attempt{Status: tc.status, ErrorCode: tc.code, HTTPStatus: tc.httpStatus, Message: "synthetic-private persisted message", FinishedAt: &finished}
			switch tc.finish {
			case "missing":
				attempt.FinishedAt = nil
			case "zero":
				attempt.FinishedAt = &time.Time{}
			case "future":
				future := time.Now().Add(time.Hour)
				attempt.FinishedAt = &future
			}
			revision := tc.revision
			if revision == "" {
				revision = "r1"
			}
			row := storedRow{SchemaVersion: 1, AccountRevision: revision, Snapshot: Snapshot{ID: "a", LastAttempt: attempt}}
			if err := s.disk.WriteJSON(t.Context(), snapshotFile("a"), row); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, directoryName, snapshotFile("a"))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := New(root, fixtureProvider(f))
			if err != nil {
				t.Fatal(err)
			}
			got := projectedHTTP(t, restored)
			if tc.wantStatus == "" {
				if got != nil {
					t.Fatalf("non-evidence was backfilled: %+v", got)
				}
			} else {
				status := tc.httpStatus
				if tc.wantStatus == "success" {
					status = 200
				}
				if got == nil || got.Status != tc.wantStatus || got.HTTPStatus != status || got.Source != "candy" || !got.FinishedAt.Equal(finished) || got.Model != "" || strings.Contains(got.Message, "synthetic-private") || strings.Contains(got.ErrorCode, "synthetic-private") {
					t.Fatalf("terminal evidence was lost, invented, or unsafe: %+v", got)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("startup or snapshot query wrote a migration", err)
			}
		})
	}
}
