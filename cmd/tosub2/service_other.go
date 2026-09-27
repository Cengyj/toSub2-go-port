//go:build !windows

package main

import (
	"errors"
	"log/slog"
	"tosub2-go-port/internal/config"
)

func runPlatformService(cfg config.Config, _ *slog.Logger) (bool, error) {
	if cfg.WindowsService {
		return true, errors.New("--windows-service requires Windows Service Control Manager")
	}
	return false, nil
}
