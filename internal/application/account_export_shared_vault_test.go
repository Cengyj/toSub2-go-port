package application

import (
	"errors"
	"reflect"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

func TestUploadSessionRejectsSharedVaultSetupWithQualityOnOrOff(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "quality-off"
		if enabled {
			name = "quality-on"
		}
		t.Run(name, func(t *testing.T) {
			for _, scenario := range []string{"setup-unverified", "vault-unavailable"} {
				t.Run(scenario, func(t *testing.T) {
					f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
					if enabled {
						enableQualityFixture(t, c, nil)
					}
					want := jobs.ErrTOTPSetupVerificationPending
					before, err := f.vault.Load(f.job.Email)
					if err != nil {
						t.Fatal(err)
					}
					if scenario == "setup-unverified" {
						before.PendingTOTPSecret = "JBSWY3DPEHPK3PXP"
						before.PendingTOTPFactorID = "shared-unverified-factor"
						if err := f.vault.Save(f.job.Email, before); err != nil {
							t.Fatal(err)
						}
					} else {
						f.manager.SetCredentialStore(nil)
						want = jobs.ErrTOTPExportUnavailable
					}
					jobsBefore := f.manager.List()
					c.transportFactory = func(string) (protocol.Doer, func(), error) {
						t.Error("unsafe upload entered a model transport")
						return nil, nil, errors.New("synthetic forbidden transport")
					}
					ctx, check, cleanup := c.NewUploadSession(t.Context(), []string{f.job.ID})
					defer cleanup()
					if err := check(ctx, f.account); !errors.Is(err, want) {
						t.Error("upload quality policy bypassed shared credential safety", err)
					}
					// A failed preflight still belongs to the upload session until
					// its caller has finished; it must not release another account.
					if f.manager.ActivitySnapshot().Queries != 1 {
						t.Error("failed upload preflight prematurely released ownership")
					}
					cleanup()
					if f.manager.ActivitySnapshot().Queries != 0 {
						t.Error("upload cleanup retained ownership")
					}
					after, err := f.vault.Load(f.job.Email)
					if err != nil || !credentials.EqualReadback(before, after) || !reflect.DeepEqual(jobsBefore, f.manager.List()) {
						t.Fatal("rejected upload changed security evidence or account history", err)
					}
				})
			}
		})
	}
}
