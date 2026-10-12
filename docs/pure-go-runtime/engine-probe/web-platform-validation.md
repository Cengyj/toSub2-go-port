# Pure-Go Web Platform validation

> Historical evidence: dated Node comparison runs below describe the pre-cleanup implementation only. The external oracle and dependencies have been removed. Current Go host tests remain in internal/jshost; use the current deployment report for release verification.

The host executes the embedded Web Platform bootstrap in Sobek. All callbacks
and Promise continuations run on the JavaScript owner's goroutine; this layer
does not issue network requests or invoke Node/Python. This is separate from the
engine syntax probe: engine support alone does not supply browser APIs.

## Tested behavior

`internal/jshost/webplatform_test.go` exercises observable behavior, including:

- WHATWG URL parsing, relative resolution, IDNA/default ports, setters and live
  URLSearchParams, including duplicate values and form encoding.
- UTF-8 encodeInto byte capacity and UTF-16 code-unit counts; UTF-8/UTF-16
  streaming decode, BOM, fatal invalid bytes, and Latin-1 base64 behavior.
- Immutable binary Blob construction and slices; copied bridge bytes and body
  buffers; Headers validation, combined values and separate Set-Cookie values.
- General default ReadableStream source start/pull/cancel, asynchronous startup,
  serialized pulls, size/high-water-mark backpressure, queued data before close,
  errors, pending reads, reader.closed, cancellation and lock release.
- Tee branches retain real data for a slower consumer and combine both cancel
  reasons. Async iteration releases its reader and honors preventCancel.
- pipeTo serializes writes, awaits close, propagates source errors to abort and
  destination errors to cancel, handles AbortSignal, releases pipe locks, and
  honors preventClose/preventAbort/preventCancel. pipeThrough uses that path.
- Response and Request can consume Uint8Array chunks from a real default stream.
  Non-byte body chunks reject instead of being silently discarded. clone keeps
  two readable bodies; constructing Request from another Request transfers the
  body and immediately disturbs/locks the original. Repeating that transfer
  fails. A replacement body may be provided even after the original was used.
- SHA-1/256/384/512 digest vectors; cryptographic random integer buffers, quota,
  byte offsets and v4 UUIDs; Performance entry prototypes, filtering, buffered
  and live observers, configured memory values and realm isolation.

The Request transfer/clone/replacement behavior, validation before transfer,
tee's retained original reader lock, and size callback receiver were checked
against Node v24.19.0's native Web Platform implementation on 2026-10-12. Node is a
development oracle only; it is not used by production.

```sh
go test internal/jshost/webplatform.go internal/jshost/webplatform_test.go -v
go test -race internal/jshost/webplatform.go internal/jshost/webplatform_test.go
CGO_ENABLED=0 go test internal/jshost/webplatform.go internal/jshost/webplatform_test.go
go test ./internal/jshost
```

## Explicit boundaries

This layer does not claim full Web Platform or Web Streams standard conformance.
ReadableStream implements default streams. Byte-source controllers and BYOB
readers throw NotSupportedError; they do not return a successful empty stream.
ReadableStream.from, WritableStream, TransformStream, TextEncoderStream and
TextDecoderStream constructors are not installed. pipeTo requires a writable
writer with real ready/closed/write/close/abort/releaseLock behavior; pipeThrough
requires an existing readable/writable transform. Unsupported destinations
reject instead of reporting successful delivery.

TextDecoder supports UTF-8/UTF-16 streaming and the measured single-byte legacy
paths. Stateful multibyte legacy streaming is explicitly rejected. The legacy
fatal decoder detects replacement characters; this is not an exhaustive Web
Encoding conformance claim. TextEncoder emits UTF-8.

SubtleCrypto currently supplies the four listed digest algorithms only; other
digest names reject with NotSupportedError. Encryption, signatures, key
generation/import/export, and derivation methods are not implemented.

Request/Response body input currently covers strings, URLSearchParams, Blob,
BufferSource and default ReadableStream. FormData serialization and multipart
body parsing are not supplied. Blob.stream returns a default stream carrying
copied Uint8Array chunks; BYOB reads remain explicitly unsupported.

Performance timing records come from supplied navigation/resource entries and
the host's actual network response hook. Heap figures are configured values,
not a measurement of the Go garbage collector or a browser heap. No request is
reported as completed merely to make an SDK check pass.
