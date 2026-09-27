#!/bin/sh
set -eu
BUILD_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$BUILD_ROOT"
mkdir -p bin
go build -trimpath -ldflags='-s -w' -o bin/tosub2 ./cmd/tosub2
