package jobs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
)

func TestAccountQueryRechecksRecoveryPublishedDuringOSAcquisition(t *testing.T) {
	for _, evidenceID := range []string{"query-recovery-owner", "query-recovery-history"} {
		t.Run(evidenceID, func(t *testing.T) {
			m, disk := accountQueryManager(t)
			registerCompletionFixture(t, disk.Root)
			job := accountQueryJob(t, m, "query-recovery-owner", "recovery-window@example.test")
			accountQueryJob(t, m, "query-recovery-history", job.Email)
			// Admission may read the protected operation journal, but must not
			// decrypt the entire credential store under the scheduler mutex.
			m.SetCredentialStore(&queryForbiddenVault{t: t})
			beforeJobs := m.List()
			beforeMeta, err := os.ReadFile(disk.Path("job-meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(disk.Root, evidenceID, "totp-setup-result.json")
			const origin = "https://query-recovery-window.synthetic.invalid"
			acquisitions, releases := 0, 0
			var evidence []byte
			m.SetAccountOperationLease(func(email string) (func() error, error) {
				acquisitions++
				if !m.mu.TryLock() {
					return nil, errors.New("OS acquisition ran under the manager mutex")
				}
				m.mu.Unlock()
				release, err := accountlock.Acquire(origin, email)
				if err != nil {
					return nil, err
				}
				// Deterministically model a peer's last write after local
				// eligibility was inspected and before ownership is returned.
				journal := map[string]any{
					"version": 1, "operation_scope": "current_session", "email": email,
					"operation_id": "external-synthetic-reset", "account_id": "fixture-account",
					"reset_totp_requested": true, "old_totp_factor_id": "old-factor",
					"reset_totp_phase": "disable_pending", "reset_totp_status": "unknown",
					"mutation_schema": 1, "disable_dispatch": "may_have_been_sent",
					"enroll_dispatch": "not_sent", "activation_dispatch": "not_sent",
				}
				if err := privatestate.WriteJSON(t.Context(), path, "totp-reset", email, "external-synthetic-reset", journal); err != nil {
					_ = release()
					return nil, err
				}
				evidence, err = os.ReadFile(path)
				if err != nil {
					_ = release()
					return nil, err
				}
				return func() error {
					releases++
					if !m.mu.TryLock() {
						t.Error("OS cleanup ran under the manager mutex")
					} else {
						if m.accountQueries[emailKey(email)] == nil {
							t.Error("local ownership disappeared before OS cleanup")
						}
						m.mu.Unlock()
					}
					return release()
				}, nil
			})
			lease, err := m.ReserveAccountQuery(t.Context(), job.ID)
			if lease != nil {
				lease.Release()
			}
			if lease != nil || !errors.Is(err, ErrTOTPRecoveryPending) {
				t.Fatal("query admitted recovery published while acquiring OS ownership", err)
			}
			decoded, readErr := protocol.ReadTOTPResetJournal(path)
			retained, fileErr := os.ReadFile(path)
			if readErr != nil || fileErr != nil || decoded["operation_id"] != "external-synthetic-reset" || decoded["disable_dispatch"] != "may_have_been_sent" || !bytes.Equal(evidence, retained) || bytes.Contains(retained, []byte(job.Email)) {
				t.Fatal("query changed, exposed or discarded the protected unknown operation evidence", readErr, fileErr)
			}
			if acquisitions != 1 || releases != 1 || m.ActivitySnapshot().Queries != 0 {
				t.Fatal("denied query leaked or repeated account ownership", acquisitions, releases)
			}
			peerRelease, err := accountlock.Acquire(origin, job.Email)
			if err != nil {
				t.Fatal("denied query leaked the host-wide OS lock", err)
			}
			if err := peerRelease(); err != nil {
				t.Fatal(err)
			}
			if lease, err := m.ReserveAccountQuery(t.Context(), job.ID); lease != nil || !errors.Is(err, ErrTOTPRecoveryPending) || acquisitions != 1 {
				if lease != nil {
					lease.Release()
				}
				t.Fatal("next admission ignored the retained unknown journal", err)
			}
			afterMeta, err := os.ReadFile(disk.Path("job-meta.json"))
			if err != nil || !bytes.Equal(beforeMeta, afterMeta) || !reflect.DeepEqual(beforeJobs, m.List()) {
				t.Fatal("failed query rewrote account state or operation history", err)
			}
		})
	}
}
