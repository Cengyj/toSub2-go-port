//go:build !windows

package main

import (
	"io"
	"log/slog"
	"testing"
	"tosub2-go-port/internal/config"
)

func TestWindowsServiceFlagFailsExplicitlyOnOtherPlatforms(t *testing.T) {
	handled, err := runPlatformService(config.Config{WindowsService: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !handled || err == nil {
		t.Fatal("unsupported service mode silently accepted")
	}
	handled, err = runPlatformService(config.Config{}, nil)
	if handled || err != nil {
		t.Fatal("normal console was intercepted")
	}
}
