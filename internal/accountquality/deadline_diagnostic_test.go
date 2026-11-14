package accountquality

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/modelprobe"
)

func TestCandyDeadlineRetainsObservedHTTPWithoutScoringOrRetry(t *testing.T) {
	s, fixture, root := newFixture(t)
	enable(t, s, func(settings *Settings) { settings.TimeoutSeconds = 1 })
	if decision, err := s.CheckForUpload(t.Context(), "a", upload("grant-a", good)); err != nil || !decision.Allowed {
		t.Fatal("synthetic prior measurement failed", err)
	}
	before, _ := s.Snapshots([]string{"a"})
	var calls atomic.Int32
	fixture.mu.Lock()
	fixture.probe = func(ctx context.Context, _ modelprobe.Options) (modelprobe.Result, error) {
		calls.Add(1)
		<-ctx.Done()
		// The model client received HTTP 200 before the stream timed out.
		return modelprobe.Result{}, &modelprobe.Error{Code: "timeout", HTTPStatus: 200}
	}
	fixture.mu.Unlock()
	receipt, err := s.Refresh(t.Context(), []string{"a"}, false)
	if err != nil || len(receipt.Accepted) != 1 {
		t.Fatal("synthetic timeout observation not admitted", err)
	}
	s.mu.Lock()
	flight := s.flights["email-a"]
	s.mu.Unlock()
	if flight == nil {
		t.Fatal("missing admitted timeout flight")
	}
	<-flight.done
	if !errors.Is(flight.err, context.DeadlineExceeded) {
		t.Fatal("preserving response evidence lost the context deadline")
	}
	after, _ := s.Snapshots([]string{"a"})
	snapshot := after["a"]
	if calls.Load() != 1 || snapshot.LastAttempt.Status != "failed" || snapshot.LastAttempt.ErrorCode != "timeout" || snapshot.LastAttempt.HTTPStatus != 200 || !snapshot.Persisted {
		t.Fatalf("deadline erased its observed response or retried: %+v", snapshot.LastAttempt)
	}
	if !reflect.DeepEqual(snapshot.LastResult, before["a"].LastResult) || !reflect.DeepEqual(snapshot.HTTPObservation, before["a"].HTTPObservation) {
		t.Fatal("partial timed-out response replaced an existing valid measurement")
	}
	history := snapshot.ProbeHistory
	if len(history) != 2 || history[1].Status != "failed" || history[1].HTTPStatus != 200 || history[1].ErrorCode != "timeout" || history[1].Score != nil || history[1].Verdict != "" {
		t.Fatal("timeout history lost evidence or fabricated a score")
	}
	restored, err := New(root, fixtureProvider(fixture))
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _ := restored.Snapshots([]string{"a"})
	if !reflect.DeepEqual(reloaded["a"].LastAttempt, snapshot.LastAttempt) || !reflect.DeepEqual(reloaded["a"].ProbeHistory, history) || calls.Load() != 1 {
		t.Fatal("local readback lost timeout evidence or replayed the request")
	}
}

func TestCancellationAndDeadlineDiagnosticsKeepOnlyObservedStatus(t *testing.T) {
	for _, tc := range []struct {
		cause error
		code  string
	}{
		{context.Canceled, "cancelled"}, {context.DeadlineExceeded, "timeout"},
	} {
		for _, status := range []int{0, 200, 503, 999} {
			err := errors.Join(tc.cause, errors.New("synthetic-private-provider-text"), &modelprobe.Error{Code: "network_error", HTTPStatus: status})
			code, message, observed := failureDetails(err)
			expected := status
			if status == 999 {
				expected = 0
			}
			if code != tc.code || observed != expected || strings.Contains(message, "synthetic-private") {
				t.Fatal("context outcome lost observed status or exposed raw provider text")
			}
		}
	}
}
