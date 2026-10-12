// Package jshost executes authentication JavaScript inside a pure Go engine.
// All HTTP and cookie operations are delegated to the owning Go session over
// the runtime protocol. This package never creates an HTTP client.
package jshost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

const maxMessageBytes = 32 << 20

const workerMemoryExitStatus = 73

type workerResourceLimits struct {
	deadline    time.Duration
	gcBudget    int64
	heapGuard   uint64
	guardPeriod time.Duration
	maxStack    int
}

// These are worker-process limits, not script-controlled options. The Go memory
// limit is a GC target. The watchdog samples HeapAlloc, so it is not an OS RSS
// ceiling and can overshoot between samples or during an uninterruptible builtin.
// The parent must retain its process deadline and cancellation path.
var workerLimits = workerResourceLimits{
	deadline: 150 * time.Second, gcBudget: 256 << 20, heapGuard: 384 << 20,
	guardPeriod: 25 * time.Millisecond, maxStack: 8 << 20,
}

// Error carries a closed diagnostic code, never script source, a request URL,
// cookie contents or a remote JavaScript exception message.
type Error struct{ Code string }

func (e *Error) Error() string { return "GO_JS_RUNTIME: " + diagnosticCode(e.Code) }

// DiagnosticCode validates the exact host failure-code set. Callers receiving a
// worker message can reject an unknown code without exposing its original text.
func DiagnosticCode(code string) (safe string, known bool) {
	safe = diagnosticCode(code)
	return safe, safe == code
}

// IsDiagnosticCode reports whether code belongs to the host's closed set.
func IsDiagnosticCode(code string) bool {
	_, known := DiagnosticCode(code)
	return known
}

// WorkerExitDiagnostic classifies only exit statuses deliberately reserved by
// this host. Other exits can come from signals, the OS or an unrelated program.
func WorkerExitDiagnostic(status int) string {
	if status == workerMemoryExitStatus {
		return "JS_RESOURCE_LIMIT_MEMORY"
	}
	return ""
}

func diagnosticCode(code string) string {
	switch code {
	case "WORKER_FAILED", "WORKER_PANIC", "WORKER_ARGUMENTS_INVALID", "INPUT_UNAVAILABLE",
		"EXECUTION_DEADLINE", "EXECUTION_CANCELED", "INPUT_INVALID", "KIND_INVALID",
		"OUTPUT_CLOSED", "INPUT_CLOSED", "PROTOCOL_INVALID", "CALL_INVALID",
		"MESSAGE_SOURCE_INVALID", "MESSAGE_TARGET_INVALID", "REPLY_UNKNOWN",
		"RESPONSE_BINARY_INVALID", "HOST_EVENT_UNAVAILABLE", "HOST_INITIALIZATION_FAILED",
		"HOST_MESSAGE_UNAVAILABLE", "SDK_INITIALIZATION_TIMEOUT", "SCRIPT_REJECTION",
		"SCRIPT_EXCEPTION", "SCRIPT_TYPEERROR", "SCRIPT_SYNTAXERROR", "SCRIPT_RANGEERROR",
		"SCRIPT_REFERENCEERROR", "SCRIPT_UNHANDLED_REJECTION", "WORKER_SCRIPT_EXCEPTION",
		"WORKER_SOURCE_HTTP", "WORKER_SOURCE_INVALID", "WORKER_SOURCE_UNAVAILABLE",
		"WORKER_INPUT_CLOSED", "WORKER_PROTOCOL_INVALID", "JS_RESOURCE_LIMIT_MEMORY":
		return code
	}
	for _, known := range hostErrorCodes {
		if code == known {
			return code
		}
	}
	return "WORKER_FAILED"
}

func workerFailure(output io.Writer, code string, status int) (handled bool, exitStatus int) {
	// A failed diagnostic sink must not replace a safe exit with a panic stack.
	defer func() { _ = recover() }()
	handled, exitStatus = true, status
	_ = json.NewEncoder(output).Encode(map[string]any{"type": "failure", "code": diagnosticCode(code)})
	return
}

// WorkerMain handles only the private worker command. The same production
// executable is used, so no external interpreter or native JS library is loaded.
func WorkerMain(args []string, input io.Reader, output io.Writer) (handled bool, status int) {
	if len(args) == 0 || args[0] != "--auth-js-worker" {
		return false, 0
	}
	defer func() {
		if recover() != nil {
			handled, status = workerFailure(output, "WORKER_PANIC", 1)
		}
	}()
	if len(args) != 3 {
		return workerFailure(output, "WORKER_ARGUMENTS_INVALID", 2)
	}
	switch args[1] {
	case "sentinel", "parent", "child":
	default:
		return workerFailure(output, "KIND_INVALID", 2)
	}
	f, err := os.Open(args[2])
	if err != nil {
		return workerFailure(output, "INPUT_UNAVAILABLE", 2)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMessageBytes+1))
	_ = f.Close()
	if err != nil || len(data) > maxMessageBytes || !json.Valid(data) {
		return workerFailure(output, "INPUT_INVALID", 2)
	}
	// The worker owns the heap and can be terminated without corrupting task
	// state. The GC target and sampled heap watchdog mitigate sustained growth;
	// neither is an OS-enforced memory ceiling. The parent owns a hard deadline.
	limits := workerLimits
	debug.SetMemoryLimit(limits.gcBudget)
	debug.SetMaxStack(limits.maxStack)
	ctx, cancel := context.WithTimeout(context.Background(), limits.deadline)
	defer cancel()
	go func() {
		ticker := time.NewTicker(limits.guardPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				if stats.HeapAlloc > limits.heapGuard {
					// Avoid a potentially blocking or interleaved protocol write from
					// this goroutine. The parent identifies this fixed exit status.
					os.Exit(workerMemoryExitStatus)
				}
			}
		}
	}()
	err = Run(ctx, args[1], data, input, output)
	if err != nil {
		code := "WORKER_FAILED"
		var scriptError *Error
		if errors.As(err, &scriptError) {
			code = scriptError.Code
		}
		if errors.Is(err, context.DeadlineExceeded) {
			code = "EXECUTION_DEADLINE"
		}
		if errors.Is(err, context.Canceled) {
			code = "EXECUTION_CANCELED"
		}
		return workerFailure(output, code, 1)
	}
	return true, 0
}

func parseInput(data json.RawMessage) (map[string]any, error) {
	var input map[string]any
	if len(data) > maxMessageBytes || json.Unmarshal(data, &input) != nil || input == nil {
		return nil, &Error{Code: "INPUT_INVALID"}
	}
	return input, nil
}

func stringField(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func codeError(code string, cause error) error {
	diagnostic := &Error{Code: diagnosticCode(code)}
	if cause == nil {
		return diagnostic
	}
	return &diagnosticCause{diagnostic: diagnostic, cause: cause}
}

type diagnosticCause struct {
	diagnostic *Error
	cause      error
}

func (e *diagnosticCause) Error() string   { return e.diagnostic.Error() }
func (e *diagnosticCause) Unwrap() []error { return []error{e.diagnostic, e.cause} }
