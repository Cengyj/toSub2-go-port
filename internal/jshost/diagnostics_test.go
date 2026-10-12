package jshost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type diagnosticWorkerResult struct {
	stdout, stderr string
	status         int
	duration       time.Duration
}

func diagnosticInput(t *testing.T, source string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"url":          "https://example.test/" + privateDiagnosticFixture,
		"html":         "<html><head></head><body></body></html>",
		"loaderUrl":    "https://example.test/" + privateDiagnosticFixture + ".js",
		"loaderSource": source,
		"privateInput": privateDiagnosticFixture,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private-input.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runDiagnosticWorker(t *testing.T, args []string, fixture, commands string, keepInputOpen bool, timeout time.Duration) diagnosticWorkerResult {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.WaitDelay = 500 * time.Millisecond
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, boundaryFixtureEnv+"=") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	if fixture != "" {
		cmd.Env = append(cmd.Env, boundaryFixtureEnv+"="+fixture)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	start := time.Now()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if commands != "" {
		_, _ = io.WriteString(input, commands)
	}
	if !keepInputOpen {
		_ = input.Close()
	}
	err = cmd.Wait()
	result := diagnosticWorkerResult{stdout: stdout.String(), stderr: stderr.String(), duration: time.Since(start)}
	if err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			t.Fatal(err)
		}
		result.status = exited.ExitCode()
	}
	if strings.Contains(result.stdout, privateDiagnosticFixture) || strings.Contains(result.stderr, privateDiagnosticFixture) {
		t.Fatal("worker diagnostic exposed private input or exception text")
	}
	if result.stderr != "" {
		t.Fatalf("worker wrote unexpected stderr (%d bytes)", len(result.stderr))
	}
	return result
}

func expectDiagnosticFailure(t *testing.T, result diagnosticWorkerResult, status int, code string) {
	t.Helper()
	if result.status != status {
		t.Fatalf("worker exit status=%d, expected %d", result.status, status)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &event); err != nil {
		t.Fatalf("expected one failure event: %v", err)
	}
	if len(event) != 2 || event["type"] != "failure" || event["code"] != code {
		t.Fatalf("unexpected diagnostic event: %v", event)
	}
}

func TestDiagnosticCodesStayClosedAndCausesStayPrivate(t *testing.T) {
	privateCause := errors.New(privateDiagnosticFixture)
	err := codeError("SCRIPT_EXCEPTION", privateCause)
	if err.Error() != "GO_JS_RUNTIME: SCRIPT_EXCEPTION" || !errors.Is(err, privateCause) {
		t.Fatal("diagnostic must preserve its cause without printing it")
	}
	var diagnostic *Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != "SCRIPT_EXCEPTION" {
		t.Fatal("diagnostic type lost through the cause wrapper")
	}
	for _, unknown := range []string{"", privateDiagnosticFixture, "SCRIPT_" + privateDiagnosticFixture, "SCRIPT_EXCEPTION\n" + privateDiagnosticFixture} {
		if safe, known := DiagnosticCode(unknown); safe != "WORKER_FAILED" || known || IsDiagnosticCode(unknown) {
			t.Fatal("unknown code passed the shared closed set validator")
		}
		if got := (&Error{Code: unknown}).Error(); got != "GO_JS_RUNTIME: WORKER_FAILED" {
			t.Fatal("unknown diagnostic code was printed")
		}
		var output bytes.Buffer
		_, _ = workerFailure(&output, unknown, 1)
		expectDiagnosticFailure(t, diagnosticWorkerResult{stdout: output.String(), status: 1}, 1, "WORKER_FAILED")
	}
	for _, code := range hostErrorCodes {
		if safe, known := DiagnosticCode(code); safe != code || !known || !IsDiagnosticCode(code) {
			t.Fatalf("closed host code was lost: %s", code)
		}
	}
	if safe, known := DiagnosticCode("WORKER_FAILED"); safe != "WORKER_FAILED" || !known {
		t.Fatal("the generic failure code is part of the shared closed set")
	}
	if WorkerExitDiagnostic(workerMemoryExitStatus) != "JS_RESOURCE_LIMIT_MEMORY" || WorkerExitDiagnostic(0) != "" || WorkerExitDiagnostic(1) != "" || WorkerExitDiagnostic(-1) != "" {
		t.Fatal("worker exit classification treated an unknown exit as the heap guard")
	}
	if handled, status := workerFailure(persistentPanicOutput{}, privateDiagnosticFixture, 1); !handled || status != 1 {
		t.Fatal("a panicking failure sink changed the safe worker exit")
	}
}

type persistentPanicOutput struct{}

func (persistentPanicOutput) Write([]byte) (int, error) { panic(privateDiagnosticFixture) }

func TestWorkerStartupFailuresUsePrivateDiagnostics(t *testing.T) {
	invalid := filepath.Join(t.TempDir(), privateDiagnosticFixture+".json")
	if err := os.WriteFile(invalid, []byte("{"+privateDiagnosticFixture), 0600); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(t.TempDir(), "oversized.json")
	f, err := os.OpenFile(oversized, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(maxMessageBytes + 1)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name, code string
		args       []string
	}{
		{"arguments", "WORKER_ARGUMENTS_INVALID", []string{"--auth-js-worker"}},
		{"kind", "KIND_INVALID", []string{"--auth-js-worker", privateDiagnosticFixture, invalid}},
		{"missing-input", "INPUT_UNAVAILABLE", []string{"--auth-js-worker", "sentinel", invalid + "-missing"}},
		{"invalid-input", "INPUT_INVALID", []string{"--auth-js-worker", "sentinel", invalid}},
		{"oversized-input", "INPUT_INVALID", []string{"--auth-js-worker", "sentinel", oversized}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			result := runDiagnosticWorker(t, fixture.args, "", "", false, 3*time.Second)
			expectDiagnosticFailure(t, result, 2, fixture.code)
		})
	}
}

func TestWorkerScriptExceptionsAndRejectionsNeverPrintSecrets(t *testing.T) {
	for _, fixture := range []struct {
		name, source, commands, code string
	}{
		{"exception", `throw new Error(__config.privateInput)`, "", "SCRIPT_EXCEPTION"},
		{"type-error", `throw new TypeError(__config.privateInput)`, "", "SCRIPT_TYPEERROR"},
		{"range-error", `throw new RangeError(__config.privateInput)`, "", "SCRIPT_RANGEERROR"},
		{"custom-name", `const e=new Error(__config.privateInput);e.name=__config.privateInput;throw e;`, "", "SCRIPT_EXCEPTION"},
		{"syntax-error", `function `, "", "SCRIPT_SYNTAXERROR"},
		{"unhandled-rejection", `Promise.reject(new Error(__config.privateInput));`, "", "SCRIPT_UNHANDLED_REJECTION"},
		{"sdk-rejection", `globalThis.SentinelSDK={token:()=>Promise.reject(new Error(__config.privateInput))};`, "{\"type\":\"call\",\"method\":\"token\",\"flow\":\"private-flow\"}\n", "SCRIPT_REJECTION"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := diagnosticInput(t, fixture.source)
			result := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "", fixture.commands, false, 3*time.Second)
			expectDiagnosticFailure(t, result, 1, fixture.code)
		})
	}
}

func TestWorkerEOFOutputFailureAndPanicRecovery(t *testing.T) {
	path := diagnosticInput(t, `globalThis.SentinelSDK={token:()=>"healthy"};`)
	result := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "", "", false, 3*time.Second)
	if result.status != 0 || result.stdout != "" {
		t.Fatal("stdin EOF did not stop an idle worker cleanly")
	}
	commands := "{\"type\":\"call\",\"method\":\"token\",\"flow\":\"fixture\"}\n"
	closedOutput := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "output-error", commands, false, 3*time.Second)
	if closedOutput.status != 1 || closedOutput.stdout != "" {
		t.Fatal("worker ignored a failed result output")
	}
	panicOutput := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "panic-output", commands, false, 3*time.Second)
	expectDiagnosticFailure(t, panicOutput, 1, "WORKER_PANIC")
}

func TestWorkerMalformedCommandsStayWithinClosedDiagnostics(t *testing.T) {
	path := diagnosticInput(t, `globalThis.SentinelSDK={token:()=>"healthy"};`)
	for _, fixture := range []struct{ name, command, code string }{
		{"malformed", "{" + privateDiagnosticFixture + "\n", "PROTOCOL_INVALID"},
		{"invalid-call", "{\"type\":\"call\",\"method\":\"" + privateDiagnosticFixture + "\"}\n", "CALL_INVALID"},
		{"unknown-reply", "{\"type\":\"" + privateDiagnosticFixture + "\",\"id\":99}\n", "REPLY_UNKNOWN"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			result := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "", fixture.command, false, 3*time.Second)
			expectDiagnosticFailure(t, result, 1, fixture.code)
		})
	}
}

func TestWorkerOwnDeadlineAndParentCancellationRecover(t *testing.T) {
	for _, source := range []string{`while(true){}`, `Promise.resolve().then(function spin(){return Promise.resolve().then(spin);});`} {
		path := diagnosticInput(t, source)
		result := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "deadline", "", true, 3*time.Second)
		expectDiagnosticFailure(t, result, 1, "EXECUTION_DEADLINE")
		if result.duration > 2*time.Second {
			t.Fatal("worker deadline did not interrupt execution promptly")
		}
	}
	path := diagnosticInput(t, `while(true){}`)
	parentCanceled := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "", "", true, 180*time.Millisecond)
	if parentCanceled.status == 0 || parentCanceled.duration > 2*time.Second {
		t.Fatal("parent cancellation did not terminate its worker")
	}
	assertHealthyWorkerAfterFailure(t)
}

func TestWorkerSampledHeapGuardExitsOnlyItsOwnProcess(t *testing.T) {
	if workerLimits.deadline != 150*time.Second || workerLimits.gcBudget != 256<<20 || workerLimits.heapGuard != 384<<20 || workerLimits.guardPeriod != 25*time.Millisecond || workerLimits.maxStack != 8<<20 {
		t.Fatal("production worker resource defaults changed")
	}
	// A test-only 48 MiB guard exercises the same WorkerMain watchdog with 1 MiB
	// retained chunks every 5 ms and an absolute 80 MiB script allocation bound.
	// It tests sampled HeapAlloc termination, not an OS/RSS ceiling or 384 MiB
	// physical-memory conformance. Parent cancellation bounds the entire probe.
	path := diagnosticInput(t, `
var retained=[],allocated=0;
var interval=setInterval(()=>{retained.push(new Uint8Array(1<<20));if(++allocated===80){clearInterval(interval);throw new Error("heap guard did not stop growth");}},5);
`)
	result := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "memory", "", true, 5*time.Second)
	if result.status != workerMemoryExitStatus {
		t.Fatalf("heap guard exit status=%d, expected %d", result.status, workerMemoryExitStatus)
	}
	if result.stdout != "" {
		t.Fatal("heap watchdog interleaved a protocol message")
	}
	assertHealthyWorkerAfterFailure(t)
}

func assertHealthyWorkerAfterFailure(t *testing.T) {
	t.Helper()
	path := diagnosticInput(t, `globalThis.SentinelSDK={token:()=>"healthy"};`)
	result := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "", "{\"type\":\"call\",\"method\":\"token\",\"flow\":\"fixture\"}\n", false, 3*time.Second)
	if result.status != 0 {
		t.Fatal("fresh worker did not recover after a bounded failure")
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &event); err != nil || event["type"] != "result" || event["value"] != "healthy" {
		t.Fatal("fresh worker did not return its SDK result")
	}
}
