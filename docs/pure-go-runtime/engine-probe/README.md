# JavaScript engine execution probe

This nested Go module is independent of the production module. It executes
synthetic JavaScript with two Go engines. It does not invoke external script
interpreters, request accounts or download SDKs. An optional first argument points
to already downloaded public SDK files. The obsolete Node/CJS reference programs
and their parser probe have been removed; they remain in the project backup.

```sh
cd docs/pure-go-runtime/engine-probe
CGO_ENABLED=0 GOTOOLCHAIN=local go run . > results.json
# Also compile a previously downloaded public loader.js and sdk.js:
CGO_ENABLED=0 GOTOOLCHAIN=local go run . /absolute/path/to/sdk-directory > results.json
```

The probe intentionally reports unsupported language features as failed rows
without aborting. Compilation confirms syntax acceptance; it does not prove that
a browser SDK works without the DOM, network, crypto and timer host APIs.

## Decision, 2026-10-12

Use `github.com/grafana/sobek v0.0.0-20261005111419-f56574604191` for the host.
The compared Goja version is
`github.com/dop251/goja v0.0.0-20261011122007-9b0700c7e02e`.

Both engines passed the actual tests for Promise microtask order, async/await and
rejections, Promise combinators, host-resolved Promises, class inheritance/private
fields/static blocks, optional chaining/nullish coalescing, typed arrays/DataView/
BigInt, RegExp named captures/lookbehind/Unicode properties, generators, dynamic
eval/Function, interruption of busy JavaScript and endless Promise microtasks,
runtime reuse after interruption, and isolation of eight concurrent pairs of
independent runtimes. The earlier public Sentinel loader/SDK syntax check is
identified by `sdk-manifest.json`; current default results contain only synthetic
Go-engine probes. Compiling those external SDK files is optional.

Direct JS object transfer between runtimes fails in both engines:
`rtB.Set("foreign", objectFromA)` returns
`TypeError: Illegal runtime transition of an Object`. This was tested with an
object containing a method that reads its realm's global state. Parent/child
window and DOM references require host proxies; plain direct value sharing does
not provide browser frame identity or method behavior.

Both reject RegExp `d` and `v` flags and async generators. These are measured
limitations, including for newly supplied remote scripts. Neither engine provides
URL, TextEncoder, crypto, document, queueMicrotask or setTimeout by itself.

The timer probe reproduces an engine panic in both pinned versions when a timer
wrapper captures a callback containing direct `eval`, followed by a global
`let id = setTimeout(...); clearTimeout(id);`. The native timer only returns an
ID; executing the callback is not required to trigger the panic. Indirect
`(0, eval)(String(callback))` passes the same probe and evaluates string timers
in global scope, as browsers do. The production timer wrapper uses indirect
eval. Both outcomes are retained in `results.json`.

The execution results were generated on macOS arm64 using Go 1.27.1 with
`CGO_ENABLED=0`; the build settings and exact linked module versions are recorded
in `results.json`. `go list -deps -json .` found no packages with CgoFiles.
The pinned Sobek module declares Go **1.25.0**, while pinned Goja declares
**1.26.0**. The comparative probe itself ran on Go 1.27.1. The production Sobek
host subsequently passed the full repository suite with Go 1.25.0 and cgo disabled;
see [final-verification.md](../final-verification.md). Sobek gives the same measured syntax coverage with
a lower compiler requirement. The comparison module uses Go 1.26.0 because it
contains both engines.

Each runtime must stay on one owner goroutine. The probe's concurrency uses
independent instances. Promise resolve/reject functions must be called by the
owner event loop. `Interrupt` can be called concurrently, but it does not stop Go
host functions or native built-ins; their contexts and resource bounds belong to
the host. `ClearInterrupt` requires synchronization after the prior run ends.

## License verification

The fixed downloaded versions' actual LICENSE files were inspected:

| Linked dependency | License |
| --- | --- |
| Goja | MIT |
| Sobek | MIT |
| github.com/dlclark/regexp2/v2 | MIT |
| github.com/go-sourcemap/sourcemap | BSD-2-Clause |
| golang.org/x/text | BSD-3-Clause |
| github.com/google/pprof | Apache-2.0 |

Production distribution needs the required notices for the dependencies it
actually links. The probe does not vendor public SDK sources; it preserves only
their byte counts, URLs and SHA-256 digests in the manifest. No temporary engine
binary or Python dependency is needed to reproduce it.
