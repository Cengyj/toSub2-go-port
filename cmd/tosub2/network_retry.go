package main

import (
	"context"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/transport"
)

func networkRetryReporter(ctx context.Context, jobID, prefix string, emit func(jobs.JobEvent)) func(transport.ReadRetryEvent) {
	return func(retry transport.ReadRetryEvent) {
		if ctx.Err() != nil || emit == nil {
			return
		}
		// Polling can finish after manual input has advanced the flow. Emit only
		// a log entry, never a snapshot of an earlier stage or input prompt.
		emit(jobs.JobEvent{JobID: jobID, Data: map[string]any{"networkRetry": prefix + retry.Message()}})
	}
}
