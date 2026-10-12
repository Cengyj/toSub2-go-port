package console

import "tosub2-go-port/internal/transport"

// Keep manual uploads and monitor repair retries in the service log. These
// requests do not own the account's current operation stage or prompt.
func (s *Server) sub2APIRetryLogger(jobID string) func(transport.ReadRetryEvent) {
	logger := s.logger
	if logger == nil {
		return nil
	}
	return func(event transport.ReadRetryEvent) {
		if jobID == "" {
			logger.Info("Sub2API network retry", "message", event.Message())
			return
		}
		logger.Info("Sub2API network retry", "jobId", jobID, "message", event.Message())
	}
}
