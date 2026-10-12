package legacyruntime

import (
	"os"
	"testing"

	"tosub2-go-port/internal/jshost"
)

func TestMain(m *testing.M) {
	if handled, code := bridgeWorkerFixture(os.Args[1:], os.Stdin, os.Stdout); handled {
		os.Exit(code)
	}
	if handled, code := jshost.WorkerMain(os.Args[1:], os.Stdin, os.Stdout); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
