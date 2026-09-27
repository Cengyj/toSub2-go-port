//go:build windows

package main

import (
	"context"
	"errors"
	"golang.org/x/sys/windows/svc"
	"log/slog"
	"time"
	"tosub2-go-port/internal/config"
)

const windowsServiceName = "toSub2-Go"

type windowsServiceHandler struct {
	cfg    config.Config
	logger *slog.Logger
	run    func(context.Context, config.Config, *slog.Logger, func()) error
}

func runPlatformService(cfg config.Config, logger *slog.Logger) (bool, error) {
	if !cfg.WindowsService {
		return false, nil
	}
	isService, err := svc.IsWindowsService()
	if err != nil {
		return true, err
	}
	if !isService {
		return true, errors.New("--windows-service must be launched by Windows Service Control Manager")
	}
	return true, svc.Run(windowsServiceName, &windowsServiceHandler{cfg: cfg, logger: logger, run: runConsole})
}

func (h *windowsServiceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	statuses <- svc.Status{State: svc.StartPending, WaitHint: 30000, CheckPoint: 1}
	go func() {
		done <- h.run(ctx, h.cfg, h.logger, func() {
			select {
			case ready <- struct{}{}:
			default:
			}
		})
	}()
	current := svc.Status{State: svc.StartPending, WaitHint: 30000, CheckPoint: 1}
	ticks := time.NewTicker(time.Second)
	defer ticks.Stop()
	var stopDeadline <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	stopping := false
	for {
		select {
		case <-ready:
			if !stopping {
				current = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown}
				statuses <- current
			}
		case change, ok := <-requests:
			if !ok || change.Cmd == svc.Stop || change.Cmd == svc.Shutdown || change.Cmd == svc.PreShutdown {
				if !stopping {
					stopping = true
					cancel()
					timeout := time.Duration(h.cfg.ShutdownTimeoutMS) * time.Millisecond
					if timeout <= 0 {
						timeout = 10 * time.Second
					}
					current = svc.Status{State: svc.StopPending, WaitHint: uint32((timeout + 5*time.Second) / time.Millisecond), CheckPoint: 1}
					statuses <- current
					timer = time.NewTimer(timeout + 5*time.Second)
					stopDeadline = timer.C
				}
				if !ok {
					requests = nil
				}
			} else if change.Cmd == svc.Interrogate {
				statuses <- current
			}
		case <-ticks.C:
			if current.State == svc.StartPending || current.State == svc.StopPending {
				current.CheckPoint++
				statuses <- current
			}
		case err := <-done:
			if err != nil && !(stopping && errors.Is(err, context.Canceled)) {
				h.logger.Error("Windows console service stopped with error", "error", err)
				return true, 1
			}
			return false, 0
		case <-stopDeadline:
			h.logger.Error("Windows console service exceeded shutdown deadline")
			return true, 2
		}
	}
}
