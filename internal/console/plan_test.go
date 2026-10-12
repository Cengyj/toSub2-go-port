package console

import (
	"context"
	"embed"
	"log/slog"
	"testing"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
)

func TestPublicJobExposesOnlyRecognizedPlanEnum(t *testing.T) {
	manager := jobs.NewManager(context.Background(), 1, nil, nil)
	server := NewServer(config.Config{StartupToken: "fixture-console-token"}, manager, embed.FS{}, slog.Default())
	for _, tc := range []struct{ reported, want string }{{"business", "team"}, {"pro", "pro"}, {"", "unknown"}, {"private-token", "unknown"}} {
		public := server.publicJob(&jobs.Job{ID: "fixture", Email: "fixture@example.test", PlanType: tc.reported})
		if public["planType"] != tc.want {
			t.Fatalf("public plan=%v, want %q", public["planType"], tc.want)
		}
	}
}
