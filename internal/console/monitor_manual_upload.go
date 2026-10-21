package console

import (
	"errors"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

// Explicit uploads can finish a paused repair. Only a completely successful
// upload to its original backend, confirming every pending ID, acknowledges
// the saved intent. Stale snapshots never clear a newer operation's state.
func (s *Server) finishManualRepairTransfers(selected []*jobs.Job, result any) error {
	ids, _ := sub2api.PublicUploadResult(result)["account_ids"].([]int64)
	confirmed := make(map[int64]bool, len(ids))
	for _, id := range ids {
		confirmed[id] = true
	}
	for _, job := range selected {
		complete := len(job.AutoRepairPendingAccountIDs) > 0
		for _, id := range job.AutoRepairPendingAccountIDs {
			complete = complete && confirmed[int64(id)]
		}
		if !complete {
			continue
		}
		if _, err := s.jobs.CommitAutoRepair(job, nil, "", true); err != nil && !errors.Is(err, jobs.ErrAutoRepairSuperseded) {
			return err
		}
	}
	return nil
}
