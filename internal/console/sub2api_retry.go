package console

import (
	"context"
	"log/slog"

	"tosub2-go-port/internal/transport"
)

// Keep manual uploads and monitor repair retries in the service log. These
// requests do not own the account's current operation stage or prompt.
func (s *Server) sub2APIRetryLogger(jobID string) func(transport.ReadRetryEvent) {
	logger := s.logger
	if logger == nil {
		return nil
	}
	return func(event transport.ReadRetryEvent) {
		level, message := slog.LevelInfo, "Sub2API read retry"
		if event.Exhausted {
			level, message = slog.LevelWarn, "Sub2API read retries exhausted"
		}
		attrs := []any{"detail", event.Message(), "retry", event.Retry, "maxRetries", event.MaxRetries}
		if jobID != "" {
			attrs = append(attrs, "jobId", jobID)
		}
		logger.Log(context.Background(), level, message, attrs...)
	}
}
