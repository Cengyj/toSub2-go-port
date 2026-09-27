package console

import "context"

func (s *Server) syncOutputs(ctx context.Context, force bool) {
	if err := s.jobs.SyncOutputs(ctx, force); err != nil && ctx.Err() == nil && s.logger != nil {
		// File paths or operating-system credential messages can contain secrets.
		s.logger.Warn("new job output discovery could not be completed")
	}
}
