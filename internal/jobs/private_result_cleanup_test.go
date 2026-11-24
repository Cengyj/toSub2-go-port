package jobs

import (
	"bytes"
	"context"
	"os"
	"testing"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/protocol"
)

func TestRecoveryCleanupReacquiresAccountLeaseAndRechecksCommittedEvidence(t *testing.T) {
	for _, kind := range []string{"add-password", "setup-2fa"} {
		for _, conflict := range []string{"lease-held", "new-evidence", "new-credential"} {
			t.Run(kind+"/"+conflict, func(t *testing.T) {
				m, vault, id, path := privateRecoveryReadbackFixture(t, kind, StatusQueued, false)
				const origin, email = "https://cleanup-lease.synthetic.invalid", "private-result@example.test"
				acquisitions := 0
				m.SetAccountOperationLease(func(account string) (func() error, error) {
					acquisitions++
					return accountlock.Acquire(origin, account)
				})
				var releaseOther func() error
				defer func() {
					if releaseOther != nil {
						_ = releaseOther()
					}
				}()
				var expected []byte
				m.metadataWriter = func(ctx context.Context, name string, value any) error {
					if err := m.store.WriteJSON(ctx, name, value); err != nil {
						return err
					}
					var err error
					switch conflict {
					case "lease-held":
						releaseOther, err = accountlock.Acquire(origin, email)
					case "new-evidence":
						if kind == "add-password" {
							err = protocol.WritePrivateJSON(path, map[string]any{"version": 1, "email": email, "password": "Synthetic-later-password9!"})
						} else {
							err = protocol.WritePrivateJSON(path, map[string]any{"version": 1, "email": email, "secret": "SYNTHETICLATERSEED", "activation_succeeded": true})
						}
					case "new-credential":
						current, loadErr := vault.memoryVault.Load(email)
						if loadErr != nil {
							return loadErr
						}
						if kind == "add-password" {
							current.Password = "Synthetic-later-password9!"
						} else {
							current.TOTPSecret = "SYNTHETICLATERSEED"
						}
						err = vault.memoryVault.Save(email, current)
					}
					if err != nil {
						t.Fatal("failed to inject cross-process cleanup conflict", err)
					}
					expected, err = os.ReadFile(path)
					return err
				}
				if err := m.Restore(t.Context()); err == nil {
					t.Fatal("cleanup ignored a conflict after local metadata commit")
				}
				retained, err := os.ReadFile(path)
				job, ok := m.Get(id)
				if err != nil || !bytes.Equal(retained, expected) || acquisitions != 2 || !ok || job.OperationStatus != "succeeded" {
					t.Fatal("cleanup deleted newer evidence or reverted confirmed local completion", err)
				}
			})
		}
	}
}
