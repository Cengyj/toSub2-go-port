#!/bin/sh
# Build and test the Go production path. No Node or Python is invoked.
set -eu
VERIFY_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$VERIFY_ROOT"
unformatted=$(gofmt -l cmd internal scripts docs/pure-go-runtime/engine-probe)
if [ -n "$unformatted" ]; then
  printf 'Go formatting required:\n%s\n' "$unformatted" >&2
  exit 1
fi
if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git diff --check
fi
# Fail on stale committed assets before the build can replace the evidence.
go run ./cmd/build-web -check
go test -count=1 ./...
go test -count=1 -race ./...
go vet ./...
TOSUB2_LEGACY_RUNTIME_TEST=1 go test -count=1 -race ./internal/jshost ./internal/legacyruntime ./cmd/tosub2
# ./... stops at nested module boundaries. Check the maintained engine probe
# explicitly with the same selected toolchain as the production module.
verify_toolchain=$(go env GOVERSION)
(
  cd docs/pure-go-runtime/engine-probe
  GOTOOLCHAIN="$verify_toolchain" go test -count=1 ./...
  GOTOOLCHAIN="$verify_toolchain" go vet ./...
)
verify_build_dir=$(mktemp -d "${TMPDIR:-/tmp}/tosub2-verify.XXXXXX")
trap 'rm -rf -- "$verify_build_dir"' EXIT
TOSUB2_BUILD_DIR="$verify_build_dir" ./scripts/build.sh
# Network-backed advisory checks use an explicit tool version without altering
# this module. Opt in when the Go vulnerability database is reachable.
if [ "${TOSUB2_VERIFY_VULN:-0}" = 1 ]; then
  go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
fi
