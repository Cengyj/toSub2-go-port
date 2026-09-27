package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestCLISuboperationExitReflectsOperationNotOAuthAvailability(t *testing.T) {
	for _, test := range []struct {
		name, operation      string
		existingOutput, fail bool
		cli                  config.CLIConfig
	}{
		{"totp-success-without-oauth", "setup-totp", false, false, config.CLIConfig{SetupTOTP: true}},
		{"password-success-without-oauth", "add-password", false, false, config.CLIConfig{AddPassword: true}},
		{"totp-failure-with-oauth", "setup-totp", true, true, config.CLIConfig{SetupTOTP: true}},
		{"password-failure-with-oauth", "add-password", true, true, config.CLIConfig{AddPassword: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager := jobs.NewManager(context.Background(), 1, st, nil)
			manager.SetRunner(func(ctx context.Context, job *jobs.Job, _ func(jobs.JobEvent)) error {
				if test.existingOutput {
					manager.UpdateResult(job.ID, true, "")
				}
				if test.fail {
					return errors.New("fixture operation failed")
				}
				return nil
			})
			job, err := manager.Submit(jobs.Spec{Email: "fixture@example.test", Operation: test.operation})
			if err != nil {
				t.Fatal(err)
			}
			manager.Start()
			defer manager.Shutdown(context.Background())
			deadline := time.After(2 * time.Second)
			for {
				current, _ := manager.Get(job.ID)
				if current.FinishedAt != nil {
					err := cliTaskError(test.cli, current)
					if (err != nil) != test.fail {
						t.Fatalf("status=%s error=%q CLI error=%v; expectedFailure=%v", current.Status, current.Error, err, test.fail)
					}
					return
				}
				select {
				case <-deadline:
					t.Fatal("operation did not finish")
				case <-time.After(time.Millisecond):
				}
			}
		})
	}
}

func TestCLIOperationPrivateDefaultsAndExplicitPaths(t *testing.T) {
	if cliOperationResultPath(config.CLIConfig{}, true) != filepath.ToSlash("tmp/chatgpt-totp-setup.json") || cliOperationResultPath(config.CLIConfig{}, false) != "tmp/chatgpt-password-add-result.json" {
		t.Fatal("legacy private output defaults changed")
	}
	cfg := config.CLIConfig{TOTPResult: "private/totp.json", PasswordAddResult: "private/password.json"}
	if cliOperationResultPath(cfg, true) != cfg.TOTPResult || cliOperationResultPath(cfg, false) != cfg.PasswordAddResult {
		t.Fatal("explicit private output ignored")
	}
}

func TestCLICredentialDeliveryCannotDiscardGeneratedSecrets(t *testing.T) {
	for _, c := range []config.CLIConfig{{SetupTOTP: true}, {AddPassword: true}, {AddPassword: true, PasswordAddResult: "private.json"}, {SetupTOTP: true, TOTPResult: "private.json"}} {
		if err := validateCLICredentialDelivery(c, "linux", ""); err == nil {
			t.Fatal("CLI would lose generated credential or write plaintext")
		}
		for _, platform := range []string{"darwin", "windows"} {
			if err := validateCLICredentialDelivery(c, platform, ""); err != nil {
				t.Fatalf("secure platform %s: %v", platform, err)
			}
		}
	}
	if err := validateCLICredentialDelivery(config.CLIConfig{AddPassword: true}, "linux", "Known-Strong-Password9!"); err != nil {
		t.Fatal(err)
	}
	if err := validateCLICredentialDelivery(config.CLIConfig{}, "linux", ""); err != nil {
		t.Fatal(err)
	}
}
