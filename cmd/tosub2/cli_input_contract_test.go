package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

func TestCLIInputPreservesPasswordAndWaitsForManualPrompt(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	dir := t.TempDir()
	output := filepath.Join(dir, "session.json")
	cfg, err := config.Parse([]string{"--email=cli-input@example.test", "--output-root=" + filepath.Join(dir, "jobs"), "--output-mode=session", "--out=" + output})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const password = " \tSynthetic-Password9! "
	_, err = runCLIWithRunnerFactory(ctx, cfg, nil, strings.NewReader(password+"\n \t123456 \n"), io.Discard, func(_ config.Config, m *jobs.Manager, _ *slog.Logger) jobs.Runner {
		return func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) error {
			emit(jobs.JobEvent{JobID: job.ID, Stage: "email_otp", Status: jobs.StatusWaitingInput, Prompt: "Automatic mailbox", Data: map[string]any{"mailStatus": "polling"}})
			// Leave the CLI event loop time to process the automatic event. No
			// stdin line belongs to it; both lines belong to later prompts.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			if _, ok := m.ConsumeInput(job.ID, "email_otp"); ok {
				t.Error("automatic polling consumed a later manual credential")
			}
			for _, step := range []struct{ stage, want string }{{"password", password}, {"email_otp", "123456"}} {
				emit(jobs.JobEvent{JobID: job.ID, Stage: step.stage, Status: jobs.StatusWaitingInput, Prompt: "Manual fixture input"})
				for {
					if value, ok := m.ConsumeInput(job.ID, step.stage); ok {
						if value != step.want {
							t.Errorf("input changed for stage %s", step.stage)
						}
						break
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(time.Millisecond):
					}
				}
			}
			return protocol.WritePrivateJSON(output, map[string]any{"email": job.Email, "fixture": true})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}
