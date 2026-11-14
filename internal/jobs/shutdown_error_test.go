package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestShutdownTimeoutRetainsMetadataFailureAndQueryOwnership(t *testing.T) {
	m, _ := accountQueryManager(t)
	job := accountQueryJob(t, m, "shutdown-persist", "shutdown@example.test")
	lease := accountQueryLease(t, m, t.Context(), job.ID)
	persistErr := errors.New("synthetic metadata write failure")
	m.metadataWriter = func(context.Context, string, any) error { return persistErr }
	t.Cleanup(func() { m.metadataWriter = nil })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := m.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, persistErr) {
		t.Fatalf("shutdown must preserve both incomplete cleanup and failed metadata: %v", err)
	}
	if lease.Context().Err() == nil {
		t.Fatal("shutdown failed to cancel the query")
	}
	if _, err := m.ReserveAccountQuery(t.Context(), job.ID); !errors.Is(err, ErrAccountQuerySuperseded) {
		t.Fatal("shutdown admitted another query", err)
	}
}
