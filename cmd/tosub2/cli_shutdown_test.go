package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

type cliShutdownWriter func([]byte) (int, error)

func (write cliShutdownWriter) Write(data []byte) (int, error) { return write(data) }

// A completed account operation and its exported result do not imply that the
// final task metadata commit succeeded. Exercise the actual CLI return path.
func TestCLIReportsShutdownPersistenceFailure(t *testing.T) {
	for _, outcome := range []string{"completed", "failed", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			root := t.TempDir()
			outputPath := filepath.Join(root, "result.json")
			cfg, err := config.Parse([]string{"--email=cli-shutdown@example.test", "--output-root=" + root, "--sub2api-out=" + outputPath})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var metadataPath string
			blockMetadata := func() {
				if err := os.Remove(metadataPath); err != nil {
					t.Error(err)
					return
				}
				if err := os.Mkdir(metadataPath, 0700); err != nil {
					t.Error(err)
				}
			}
			var output io.Writer = io.Discard
			if outcome == "completed" {
				output = cliShutdownWriter(func(data []byte) (int, error) {
					// The output is already saved; only the later shutdown fails.
					blockMetadata()
					return len(data), nil
				})
			}
			handled, err := runCLIWithRunnerFactory(ctx, cfg, nil, strings.NewReader(""), output, func(cfg config.Config, _ *jobs.Manager, _ *slog.Logger) jobs.Runner {
				return func(ctx context.Context, job *jobs.Job, _ func(jobs.JobEvent)) error {
					metadataPath = filepath.Join(cfg.OutputRoot, job.ID, "job-meta.json")
					switch outcome {
					case "failed":
						blockMetadata()
						return errors.New("synthetic operation failure")
					case "canceled":
						blockMetadata()
						cancel()
						<-ctx.Done()
						return ctx.Err()
					default:
						return protocol.WritePrivateJSON(filepath.Join(cfg.OutputRoot, job.ID, "sub2api-import-oauth.json"), map[string]any{"fixture": true, "email": job.Email})
					}
				}
			})
			if !handled || err == nil || !strings.Contains(err.Error(), "CLI job manager shutdown") {
				t.Fatalf("CLI discarded final persistence failure: handled=%v err=%v", handled, err)
			}
			if outcome == "failed" && !strings.Contains(err.Error(), "synthetic operation failure") {
				t.Fatal("shutdown error replaced the operation failure", err)
			}
			if outcome == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("shutdown error replaced cancellation", err)
			}
			if outcome == "completed" {
				if data, err := os.ReadFile(outputPath); err != nil || !strings.Contains(string(data), `"fixture": true`) {
					t.Fatal("shutdown failure discarded a completed output", err)
				}
			}
		})
	}
}

// Model a deadline's terminal error with a manually released Done channel, so
// the event ordering below does not depend on a wall-clock timeout winning.
type cliTerminalContext struct {
	context.Context
	done        <-chan struct{}
	terminalErr error
}

func (ctx cliTerminalContext) Done() <-chan struct{} { return ctx.done }

func (ctx cliTerminalContext) Err() error {
	select {
	case <-ctx.done:
		return ctx.terminalErr
	default:
		return nil
	}
}

func TestCLITerminalPublicationPreservesCancellationAndCompletedOutput(t *testing.T) {
	for _, outcome := range []string{"canceled", "deadline", "completed"} {
		t.Run(outcome, func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			root := t.TempDir()
			outputPath := filepath.Join(root, "result.json")
			cfg, err := config.Parse([]string{"--email=cli-terminal@example.test", "--output-root=" + root, "--sub2api-out=" + outputPath, "--verbose"})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			cancel := sync.OnceFunc(func() { close(done) })
			defer cancel()
			wantErr := context.Canceled
			if outcome == "deadline" {
				wantErr = context.DeadlineExceeded
			}
			ctx := cliTerminalContext{Context: context.Background(), done: done, terminalErr: wantErr}
			entered := make(chan struct{})
			var manager *jobs.Manager
			observedTerminal := false
			output := cliShutdownWriter(func(data []byte) (int, error) {
				if !strings.Contains(string(data), "cli_terminal_barrier") {
					return len(data), nil
				}
				close(entered)
				deadline := time.Now().Add(5 * time.Second)
				for {
					list := manager.List()
					if len(list) == 1 && list[0].FinishedAt != nil {
						observedTerminal = true
						// A completed output remains successful when cancellation
						// arrives after the runner has published its final result.
						cancel()
						return len(data), nil
					}
					if time.Now().After(deadline) {
						t.Error("runner did not publish its terminal state at the CLI barrier")
						cancel()
						return 0, io.ErrUnexpectedEOF
					}
					time.Sleep(time.Millisecond)
				}
			})
			handled, err := runCLIWithRunnerFactory(ctx, cfg, nil, strings.NewReader(""), output, func(_ config.Config, m *jobs.Manager, _ *slog.Logger) jobs.Runner {
				manager = m
				return func(runCtx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) error {
					emit(jobs.JobEvent{Stage: "cli_terminal_barrier", Status: jobs.StatusRunning})
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						return errors.New("runner event did not reach the CLI output barrier")
					}
					if outcome == "completed" {
						return protocol.WritePrivateJSON(filepath.Join(cfg.OutputRoot, job.ID, "sub2api-import-oauth.json"), map[string]any{"fixture": true})
					}
					cancel()
					<-runCtx.Done()
					return runCtx.Err()
				}
			})
			if !handled || !observedTerminal {
				t.Fatal("fixture did not force terminal publication before the CLI checks cancellation")
			}
			if outcome != "completed" {
				if !errors.Is(err, wantErr) {
					t.Fatalf("terminal publication discarded cancellation: want=%v err=%v", wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal("late cancellation replaced a completed operation", err)
			}
			data, readErr := os.ReadFile(outputPath)
			if readErr != nil || !strings.Contains(string(data), `"fixture": true`) {
				t.Fatal("late cancellation discarded the completed output", readErr)
			}
		})
	}
}
