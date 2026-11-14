#!/bin/sh
set -eu
BUILD_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$BUILD_ROOT"
exec go run ./cmd/build-web -build-go
