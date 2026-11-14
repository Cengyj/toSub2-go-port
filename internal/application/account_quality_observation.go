package application

import (
	"net/http"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/modelprobe"
)

type manualHTTPObservation struct {
	accountRevision string
	value           accountquality.HTTPObservation
}

// observeHTTP runs under the manual-state mutex only when an attempt finishes.
// Unknown outcomes and cancellations retain the last explicit observation.
// The drawing record and automatic score remain independent of this projection.
func (m *manualProbeState) observeHTTP(attempt *manualProbeAttempt) {
	r := attempt.record
	if r.FinishedAt == nil || r.FinishedAt.IsZero() || attempt.accountRevision == "" {
		return
	}
	observation := accountquality.HTTPObservation{Source: "pelican", FinishedAt: *r.FinishedAt, Model: r.Model}
	switch {
	case r.State == "success":
		observation.Status, observation.HTTPStatus = "success", http.StatusOK
	case r.State == "failed" && r.HTTPStatus >= 300 && r.HTTPStatus <= 599:
		diagnostic := modelprobe.DiagnoseFailure(&modelprobe.Error{Code: r.ErrorCode, HTTPStatus: r.HTTPStatus})
		observation.Status, observation.HTTPStatus = "failed", r.HTTPStatus
		observation.ErrorCode, observation.Message = diagnostic.Code, diagnostic.Message
	default:
		return
	}
	m.observations[r.ID] = manualHTTPObservation{accountRevision: attempt.accountRevision, value: observation}
	m.observationRevision++
}

// Snapshots batches existing automatic snapshots with the latest explicit
// manual HTTP outcome. Reads never acquire a grant, query upstream, persist a
// row, or copy a drawing into the scored automatic result.
func (c *AccountQualityController) Snapshots(ids []string) (map[string]accountquality.Snapshot, uint64) {
	rows, revision := c.Service.Snapshots(ids)
	accounts := map[string]accountquality.Account{}
	for _, account := range c.accounts() {
		accounts[account.ID] = account
	}
	m := c.manual
	m.mu.Lock()
	defer m.mu.Unlock()
	revision += m.observationRevision
	for id, snapshot := range rows {
		account, active := accounts[id]
		if !active || !account.CanCheck {
			snapshot.HTTPObservation = nil
		} else if manual, exists := m.observations[id]; exists && manual.accountRevision == account.Revision {
			candidate := manual.value
			current := snapshot.HTTPObservation
			if current == nil || candidate.FinishedAt.After(current.FinishedAt) || candidate.FinishedAt.Equal(current.FinishedAt) && candidate.Status == "success" && current.Status != "success" {
				snapshot.HTTPObservation = &candidate
			}
		}
		snapshot.Revision = revision
		rows[id] = snapshot
	}
	return rows, revision
}
