//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/windows/svc"
	"io"
	"log/slog"
	"testing"
	"time"
	"tosub2-go-port/internal/config"
)

func TestWindowsServiceControlCancelsAndWaitsForCleanup(t *testing.T) {
	for _, command := range []svc.Cmd{svc.Stop, svc.Shutdown, svc.PreShutdown} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			requests := make(chan svc.ChangeRequest, 2)
			statuses := make(chan svc.Status, 32)
			cleanupStarted, finishCleanup := make(chan struct{}), make(chan struct{})
			result := make(chan uint32, 1)
			h := windowsServiceHandler{cfg: config.Config{ShutdownTimeoutMS: 1000}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), run: func(ctx context.Context, _ config.Config, _ *slog.Logger, ready func()) error {
				ready()
				<-ctx.Done()
				close(cleanupStarted)
				<-finishCleanup
				return nil
			}}
			go func() {
				specific, code := h.Execute(nil, requests, statuses)
				if specific && code == 0 {
					code = 999
				}
				result <- code
			}()
			readState := func(want svc.State) {
				t.Helper()
				select {
				case status := <-statuses:
					if status.State != want {
						t.Fatalf("service state=%v want=%v", status.State, want)
					}
					if want == svc.Running && status.Accepts&(svc.AcceptStop|svc.AcceptShutdown|svc.AcceptPreShutdown) != (svc.AcceptStop|svc.AcceptShutdown|svc.AcceptPreShutdown) {
						t.Fatal("stop controls not accepted")
					}
				case <-time.After(time.Second):
					t.Fatal("missing service state")
				}
			}
			readState(svc.StartPending)
			readState(svc.Running)
			requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
			readState(svc.Running)
			requests <- svc.ChangeRequest{Cmd: command}
			readState(svc.StopPending)
			select {
			case <-cleanupStarted:
			case <-time.After(time.Second):
				t.Fatal("stop did not cancel console")
			}
			select {
			case <-result:
				t.Fatal("service exited before cleanup")
			default:
			}
			close(finishCleanup)
			select {
			case code := <-result:
				if code != 0 {
					t.Fatalf("stop code=%d", code)
				}
			case <-time.After(time.Second):
				t.Fatal("service failed to finish")
			}
		})
	}
}

func TestWindowsServiceStartupFailureIsReported(t *testing.T) {
	statuses := make(chan svc.Status, 16)
	h := windowsServiceHandler{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), run: func(context.Context, config.Config, *slog.Logger, func()) error {
		return errors.New("fixture startup failure")
	}}
	specific, code := h.Execute(nil, make(chan svc.ChangeRequest), statuses)
	if !specific || code != 1 {
		t.Fatalf("SCM failure code=%v/%d", specific, code)
	}
	close(statuses)
	for status := range statuses {
		if status.State == svc.Running {
			t.Fatal("failed service announced Running")
		}
	}
}
