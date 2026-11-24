package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

type cliProgressReader struct{ reads atomic.Int32 }

func (r *cliProgressReader) Read([]byte) (int, error) {
	r.reads.Add(1)
	return 0, io.EOF
}

func TestCLIProfileProgressIsVisibleWithoutConsumingInput(t *testing.T) {
	for _, terminal := range []string{"profile_completed", "profile_unknown", "profile_rejected", "profile_not_sent"} {
		t.Run(terminal, func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			root := t.TempDir()
			cfg, err := config.Parse([]string{"--email=profile-cli@example.test", "--output-root=" + root, "--sub2api-out=" + filepath.Join(root, "result.json")})
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			input := &cliProgressReader{}
			calls := 0
			failure := errors.New("synthetic profile completion failed")
			handled, err := runCLIWithRunnerFactory(t.Context(), cfg, nil, input, &output, func(_ config.Config, _ *jobs.Manager, _ *slog.Logger) jobs.Runner {
				return func(_ context.Context, job *jobs.Job, emit func(jobs.JobEvent)) error {
					calls++
					for _, stage := range []string{"profile_required", "profile_submitting", terminal} {
						emit(cliProfileFixtureEvent(stage))
					}
					if terminal != "profile_completed" {
						return failure
					}
					return protocol.WritePrivateJSON(filepath.Join(root, job.ID, "sub2api-import-oauth.json"), map[string]any{"fixture": true})
				}
			})
			if !handled || calls != 1 || input.reads.Load() != 0 {
				t.Fatal("profile progress changed operation or consumed stdin", handled, calls, input.reads.Load())
			}
			if (err == nil) != (terminal == "profile_completed") || err != nil && !strings.Contains(err.Error(), failure.Error()) {
				t.Fatal("profile display changed operation result", err)
			}
			previous := -1
			for _, stage := range []string{"profile_required", "profile_submitting", terminal} {
				message := cliProfileFixtureEvent(stage).Prompt
				index := strings.Index(output.String(), message)
				if index <= previous || strings.Count(output.String(), message) != 1 {
					t.Fatal("CLI omitted or reordered profile progress", stage, output.String())
				}
				previous = index
			}
			if strings.Contains(output.String(), "private-generated-profile") {
				t.Fatal("CLI exposed generated profile material")
			}
		})
	}
}

func TestCLIDrainsCommittedProgressBeforeCompletedOutput(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	root := t.TempDir()
	outputPath := filepath.Join(root, "result.json")
	cfg, err := config.Parse([]string{"--email=cli-progress@example.test", "--output-root=" + root, "--sub2api-out=" + outputPath, "--verbose"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{})
	var manager *jobs.Manager
	var output bytes.Buffer
	writer := cliShutdownWriter(func(data []byte) (int, error) {
		if strings.Contains(string(data), "cli_progress_barrier") {
			close(entered)
			for {
				list := manager.List()
				if len(list) == 1 && list[0].FinishedAt != nil {
					break
				}
				select {
				case <-ctx.Done():
					return 0, ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
		}
		return output.Write(data)
	})
	_, err = runCLIWithRunnerFactory(ctx, cfg, nil, &cliProgressReader{}, writer, func(_ config.Config, m *jobs.Manager, _ *slog.Logger) jobs.Runner {
		manager = m
		return func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) error {
			emit(jobs.JobEvent{Stage: "cli_progress_barrier", Status: jobs.StatusRunning})
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-entered:
			}
			for _, stage := range []string{"profile_required", "profile_submitting", "profile_completed"} {
				emit(cliProfileFixtureEvent(stage))
			}
			return protocol.WritePrivateJSON(filepath.Join(root, job.ID, "sub2api-import-oauth.json"), map[string]any{"fixture": true})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	complete, saved := strings.Index(output.String(), "profile fixture completed"), strings.Index(output.String(), "OAuth import saved.")
	if complete < 0 || saved <= complete {
		t.Fatal("terminal publication skipped queued progress", output.String())
	}
}

func TestCLIProgressOutputFailureDoesNotRepeatOrDiscardCompletedOperation(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	root := t.TempDir()
	outputPath := filepath.Join(root, "result.json")
	cfg, err := config.Parse([]string{"--email=cli-output@example.test", "--output-root=" + root, "--sub2api-out=" + outputPath, "--verbose"})
	if err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("synthetic output failure")
	failed := false
	writer := cliShutdownWriter(func(data []byte) (int, error) {
		if !failed {
			failed = true
			return 0, writeErr
		}
		return len(data), nil
	})
	calls := 0
	_, err = runCLIWithRunnerFactory(t.Context(), cfg, nil, &cliProgressReader{}, writer, func(_ config.Config, _ *jobs.Manager, _ *slog.Logger) jobs.Runner {
		return func(_ context.Context, job *jobs.Job, emit func(jobs.JobEvent)) error {
			calls++
			emit(cliProfileFixtureEvent("profile_completed"))
			return protocol.WritePrivateJSON(filepath.Join(root, job.ID, "sub2api-import-oauth.json"), map[string]any{"fixture": true})
		}
	})
	if !errors.Is(err, writeErr) || calls != 1 {
		t.Fatal("CLI output failure was lost or repeated the operation", err, calls)
	}
	if data, err := os.ReadFile(outputPath); err != nil || !strings.Contains(string(data), `"fixture": true`) {
		t.Fatal("progress output failure discarded the completed private result", err)
	}
}

func TestCLICancellationRetainsUncertainProfileOutcome(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	root := t.TempDir()
	outputPath := filepath.Join(root, "result.json")
	cfg, err := config.Parse([]string{"--email=cli-cancel-profile@example.test", "--output-root=" + root, "--sub2api-out=" + outputPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var output bytes.Buffer
	input := &cliProgressReader{}
	calls := 0
	_, err = runCLIWithRunnerFactory(ctx, cfg, nil, input, &output, func(_ config.Config, _ *jobs.Manager, _ *slog.Logger) jobs.Runner {
		return func(runCtx context.Context, _ *jobs.Job, emit func(jobs.JobEvent)) error {
			calls++
			emit(cliProfileFixtureEvent("profile_required"))
			emit(cliProfileFixtureEvent("profile_submitting"))
			cancel()
			<-runCtx.Done()
			// Cancellation after dispatch cannot prove whether the mutation
			// happened. This final observation does not resume the operation.
			emit(cliProfileFixtureEvent("profile_unknown"))
			return runCtx.Err()
		}
	})
	if !errors.Is(err, context.Canceled) || calls != 1 || input.reads.Load() != 0 {
		t.Fatal("outcome display changed cancellation or repeated input/work", err, calls, input.reads.Load())
	}
	if strings.Count(output.String(), cliProfileFixtureEvent("profile_unknown").Prompt) != 1 || strings.Contains(output.String(), "OAuth import saved.") {
		t.Fatal("cancellation hid the uncertain profile result or claimed authorization success", output.String())
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uncertain profile result produced an authorization export", err)
	}
}

func cliProfileFixtureEvent(stage string) jobs.JobEvent {
	outcome := map[string]string{"profile_required": "required", "profile_submitting": "submitting", "profile_completed": "succeeded", "profile_unknown": "unknown", "profile_rejected": "rejected", "profile_not_sent": "not_sent"}[stage]
	return jobs.JobEvent{Stage: stage, Status: jobs.StatusRunning, Prompt: "profile fixture " + strings.TrimPrefix(stage, "profile_"), Data: map[string]any{
		"profileCompletion": map[string]any{"trigger": "about_you", "source": "generated_adult", "outcome": outcome, "name": "private-generated-profile"},
	}}
}
