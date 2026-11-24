package main

import (
	"fmt"
	"io"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/resetlog"
)

// Output failure is independent of the account operation. Keep the first
// failure for the CLI exit while allowing the runner to protect its result.
// Reporting a progress failure must not replay or abandon a remote change.
type cliProgressOutput struct {
	writer  io.Writer
	options config.CLIConfig
	err     error
}

func (p *cliProgressOutput) recordError(err error) {
	if p.err == nil && err != nil {
		p.err = fmt.Errorf("CLI progress output could not be written: %w", err)
	}
}

// event reports non-interactive progress and returns true when it handled a
// dedicated diagnostic. It never reads stdin or changes operation state.
func (p *cliProgressOutput) event(event jobs.JobEvent) bool {
	if d, ok := event.Data[resetlog.DataKey].(*resetlog.Detail); ok && d != nil {
		if d.Terminal || p.options.Verbose || p.options.DebugAuth {
			_, err := fmt.Fprintf(p.writer, "[%s #%d] %s\n", d.OpRef, d.Seq, d.Message())
			p.recordError(err)
		}
		return true
	}
	// The manager admits only the public trigger/source/outcome fields. The
	// operator needs these notices even when general stage tracing is disabled.
	if _, ok := event.Data["profileCompletion"].(map[string]any); ok && event.Prompt != "" {
		_, err := fmt.Fprintf(p.writer, "[%s] %s\n", event.Stage, logging.RedactText(event.Prompt))
		p.recordError(err)
		return true
	}
	if p.options.Verbose || p.options.DebugAuth {
		_, err := fmt.Fprintf(p.writer, "[%s] %s\n", event.Stage, event.Status)
		p.recordError(err)
	}
	return false
}

func (p *cliProgressOutput) drain(events <-chan jobs.JobEvent, id string) {
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			if event.JobID == id {
				p.event(event)
			}
		default:
			return
		}
	}
}
