#!/bin/sh
# Build and embed the frontend using the Go esbuild API and vendored browser libraries.
set -eu
BUILD_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$BUILD_ROOT"
exec go run ./cmd/build-web
