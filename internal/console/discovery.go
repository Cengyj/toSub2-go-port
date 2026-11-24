package console

import "context"

func (s *Server) syncOutputs(ctx context.Context, force bool) {
	// Discovery can persist imported output metadata. Count it even when a
	// public list GET initiated it, so maintenance cannot report drained
	// between the admission check and the metadata commit.
	release, err := s.maintenance.Enter()
	if err != nil {
		return
	}
	defer release()
	if err := s.jobs.SyncOutputs(ctx, force); err != nil && ctx.Err() == nil && s.logger != nil {
		// File paths or operating-system credential messages can contain secrets.
		s.logger.Warn("new job output discovery could not be completed")
	}
}
