# Pure-Go worker diagnostics and resource boundaries

> Historical verification boundary: the dated test commands and resource measurements below describe the Go host at the recorded revision. They do not establish the current deployment status; see ../local-deployment.md for the current cleanup and verification.

Authentication JavaScript runs in a subprocess of the same Go executable.
`WorkerMain` accepts only the private `--auth-js-worker` command with a supported
kind and bounded JSON input file. File paths, invalid input, remote exception
messages, cookie/password/OTP fixture values and arbitrary script error names
are never copied into failure diagnostics. Failure events contain only `type`
and a code from the host's closed set. Wrapped Go causes remain available to
`errors.Is/As` without being printed in the diagnostic string.

The bridge can use `DiagnosticCode` / `IsDiagnosticCode` to validate the same
closed set. An arbitrary all-uppercase string does not become safe merely by
matching a code-shaped regular expression. `WorkerExitDiagnostic` classifies
only reserved exit status 73 as `JS_RESOURCE_LIMIT_MEMORY`; status 0, generic
failure status 1 and termination signals have no implicit heap classification.

The worker uses these production defaults:

| Boundary | Default | Meaning |
| --- | --- | --- |
| Input file / protocol message | 32 MiB | Input files are read with a limit; command scanner has a bounded token size. |
| Worker execution | 150 seconds | Context cancellation interrupts JavaScript; parent separately owns its process deadline and cancellation. |
| Go GC target | 256 MiB | `debug.SetMemoryLimit` asks the runtime to collect toward this target. It is not an allocation or RSS ceiling. |
| Sampled heap guard | 384 MiB HeapAlloc, every 25 ms | Exits the worker with status 73 after observing sustained Go heap growth beyond the guard. |
| Go goroutine stack | 8 MiB | `debug.SetMaxStack` bounds individual stack growth. It does not make a fatal Go stack-overflow recoverable. |

The host also bounds pending timer tasks and generated request promises at
4,096 each, realms at 32, retained Blob URL entries at 1,024, active Workers at
32 and lifetime Worker instances at 128. Worker message channels use bounded
queues and report `JS_RESOURCE_LIMIT_WORKER_MESSAGES` on saturation. Storage
has a separate 5 MiB UTF-16 byte quota per origin and local/session area. These
limits report fixed resource codes; this document does not claim each threshold
was reached in an end-to-end production account flow.

The memory guard is **not an OS hard memory limit**. Allocations can overshoot
between samples; Go runtime/native allocations and process RSS are different
measurements. A native builtin can delay JavaScript context interruption. The
parent must preserve its subprocess termination path. The heap watchdog exits
without writing protocol output from its goroutine, avoiding interleaved output
or a blocking diagnostic sink. The parent can identify fixed exit status 73 as
the heap guard; other process exits must not be presented as proof of this guard.

`diagnostics_test.go` launches `os.Executable()` and dispatches the real
`WorkerMain` through a test `TestMain`. It verifies:

- Missing arguments, invalid kind, missing/private file path, invalid JSON and
  a sparse input file beyond 32 MiB produce only the expected fixed code.
- Error, TypeError, RangeError, syntax failures, custom private exception names,
  unhandled rejection and SDK rejection do not print the fixture secret in
  stdout or stderr.
- Idle stdin EOF stops cleanly. Failed output and a recovered output panic
  cause a safe failure without printing the writer's private exception.
- A test-only 180 ms deadline stops a JavaScript busy loop and an endless
  Promise chain. An independent 180 ms parent deadline terminates a worker
  running under the unchanged production deadline.
- A test-only 48 MiB heap guard uses the same 25 ms production watchdog path.
  The script retains 1 MiB Uint8Array blocks every 5 ms, with an absolute 80 MiB
  script allocation bound. The watchdog exits with status 73 and emits no
  concurrent protocol message. A fresh worker then returns a healthy result.

The smaller deadline/heap guard are assigned only by `_test.go` before calling
WorkerMain in a child process. Production has no environment or script option
that can select these test limits. This bounded probe verifies the watchdog's
behavior and isolation without exhausting host memory; it does not certify an
OS-enforced 384 MiB ceiling. Production defaults are asserted independently.

```sh
go test ./internal/jshost -run '^Test(Diagnostic|WorkerStartup|WorkerScriptExceptions|WorkerEOF|WorkerMalformed|WorkerOwn|WorkerSampled)' -count=1
go test -race ./internal/jshost -run '^Test(Diagnostic|WorkerStartup|WorkerScriptExceptions|WorkerEOF|WorkerMalformed|WorkerOwn|WorkerSampled)' -count=1
CGO_ENABLED=0 go test ./internal/jshost -run '^Test(Diagnostic|WorkerStartup|WorkerScriptExceptions|WorkerEOF|WorkerMalformed|WorkerOwn|WorkerSampled)' -count=1
```

On 2026-10-12 these three checks passed on macOS arm64 with Go 1.27.1, in
1.226 s, 6.205 s and 1.372 s respectively. They execute real child processes;
the successful race run is not a skipped memory/deadline test. Earlier full
package runs found a separate loader currentScript microtask regression; these
diagnostic results do not claim a final full-package acceptance after every
host edit. That acceptance belongs to the final combined verification.
