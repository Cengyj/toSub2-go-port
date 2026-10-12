package jshost

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

const boundaryFixtureEnv = "TOSUB2_JSHOST_TEST_BOUNDARY"
const privateDiagnosticFixture = "PRIVATE_COOKIE_PASSWORD_OTP_EXCEPTION_123"

// Dispatch the exact production WorkerMain from this same test executable.
// Smaller limits exist only in this _test.go entry point; production cannot
// select them through an environment variable or a remote script input.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--auth-js-worker" {
		var output io.Writer = os.Stdout
		switch os.Getenv(boundaryFixtureEnv) {
		case "deadline":
			workerLimits.deadline = 180 * time.Millisecond
		case "memory":
			workerLimits.gcBudget = 24 << 20
			workerLimits.heapGuard = 48 << 20
		case "output-error":
			output = privateBrokenOutput{}
		case "panic-output":
			output = &privatePanicOutput{}
		}
		if handled, code := WorkerMain(os.Args[1:], os.Stdin, output); handled {
			os.Exit(code)
		}
	}
	os.Exit(m.Run())
}

type privateBrokenOutput struct{}

func (privateBrokenOutput) Write([]byte) (int, error) {
	return 0, errors.New(privateDiagnosticFixture)
}

type privatePanicOutput struct{ panicked bool }

func (w *privatePanicOutput) Write(data []byte) (int, error) {
	if !w.panicked {
		w.panicked = true
		panic(privateDiagnosticFixture)
	}
	return os.Stdout.Write(data)
}
